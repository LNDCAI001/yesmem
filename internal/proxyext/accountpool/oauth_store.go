package accountpool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// tokenCacheEntry holds a cached token and the timestamp at which the
// credential file was last read. Entries are invalidated on 401 or after
// tokenCacheTTL seconds to ensure fresh reads after credential rotation.
type tokenCacheEntry struct {
	result    TokenResult
	readAt    time.Time
	credsPath string
}

const (
	tokenCacheTTL        = 30 * time.Second
	tokenCacheEvictEvery = 60 * time.Second
)

// tokenCache is the process-level cache keyed on the absolute credential dir path.
// sync.Map is chosen because reads vastly outnumber writes in normal operation.
var (
	tokenCache      sync.Map  // map[string]*tokenCacheEntry
	evictOnce       sync.Once // ensures the eviction goroutine is started exactly once
)

// startEviction starts a background goroutine that purges stale entries from
// tokenCache every tokenCacheEvictEvery. Called lazily on first use so that
// packages importing accountpool in tests do not start background goroutines
// unconditionally.
func startEviction() {
	evictOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(tokenCacheEvictEvery)
			defer ticker.Stop()
			for range ticker.C {
				now := time.Now()
				tokenCache.Range(func(k, v interface{}) bool {
					entry := v.(*tokenCacheEntry)
					if now.Sub(entry.readAt) > tokenCacheTTL {
						tokenCache.Delete(k)
					}
					return true
				})
			}
		}()
	})
}

// InvalidateToken removes the cached token for the given credentialDir.
// Call this when a 401 is received for an account so the next attempt
// re-reads the credential file from disk (in case the token was refreshed
// by Claude CLI / Claude Code between requests).
func InvalidateToken(credentialDir string) {
	if credentialDir == "" {
		return
	}
	tokenCache.Delete(credentialDir)
}

// LocalOAuthStore reads Claude credentials from the local filesystem.
// Results are cached in memory for tokenCacheTTL to avoid redundant disk reads
// on every request. The cache is invalidated on 401 via InvalidateToken.
// A background eviction goroutine (started once per process) removes stale
// entries so the sync.Map does not grow unboundedly in long-lived processes.
type LocalOAuthStore struct{}

// claudeCredentials mirrors the subset of fields we need from .credentials.json.
type claudeOAuthInner struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // Unix milliseconds
}

type claudeCredentials struct {
	ClaudeAiOauth claudeOAuthInner `json:"claudeAiOauth"`
}

// GetAccessToken implements TokenProvider.
// On cache hit (within tokenCacheTTL and not invalidated), returns the cached
// token without a disk read. On cache miss or invalidation, reads the
// credential file, validates the token, and updates the cache.
//
// Expired tokens are NOT rejected at this layer. Claude Code on the host
// refreshes .credentials.json via its own OAuth mechanism. If the cached
// token is stale, the upstream 401 triggers InvalidateToken and the next
// retry re-reads from disk — which will contain the refreshed token.
// This avoids needing to bypass Cloudflare from the Go proxy.
func (s *LocalOAuthStore) GetAccessToken(_ context.Context, account AccountRef) (TokenResult, error) {
	// Ensure the background eviction goroutine is running.
	startEviction()

	dir := account.CredentialDir
	if dir == "" {
		return TokenResult{}, fmt.Errorf("accountpool: empty credential_dir for account %q", account.Name)
	}

	// Expand ~ manually since os.Open does not perform shell expansion.
	if len(dir) > 1 && dir[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return TokenResult{}, fmt.Errorf("accountpool: expand home for %q: %w", account.Name, err)
		}
		dir = filepath.Join(home, dir[2:])
	}

	// Check cache before hitting the filesystem.
	if v, ok := tokenCache.Load(dir); ok {
		entry := v.(*tokenCacheEntry)
		if time.Since(entry.readAt) < tokenCacheTTL {
			// Return cached result only if the token is still valid.
			const expiryGrace = 30 * time.Second
			if entry.result.ExpiresAt.IsZero() || time.Now().Before(entry.result.ExpiresAt.Add(-expiryGrace)) {
				return entry.result, nil
			}
			// Token in cache is expiring — fall through to re-read.
			tokenCache.Delete(dir)
		}
	}

	credsPath := filepath.Join(dir, ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return TokenResult{}, fmt.Errorf("accountpool: read credentials %q: %w", credsPath, err)
	}

	var creds claudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return TokenResult{}, fmt.Errorf("accountpool: parse credentials %q: %w", credsPath, err)
	}

	token := creds.ClaudeAiOauth.AccessToken
	if token == "" {
		return TokenResult{}, fmt.Errorf("accountpool: no usable token in %q", credsPath)
	}

	result := TokenResult{Token: token}
	if creds.ClaudeAiOauth.ExpiresAt > 0 {
		result.ExpiresAt = time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt)
	}

	// Populate cache.
	tokenCache.Store(dir, &tokenCacheEntry{
		result:    result,
		readAt:    time.Now(),
		credsPath: credsPath,
	})

	return result, nil
}

// RefreshAccessToken implements TokenProvider. It re-reads the credential file
// from disk and warms the in-memory cache. Claude Code on the host manages the
// actual OAuth refresh and writes updated .credentials.json. The Go proxy
// cannot refresh directly because claude.ai is behind Cloudflare (requires
// browser cookies). Instead, the proxy trusts that the host-side Claude Code
// process will refresh before the access token expires.
//
// On startup this is called for every configured account, warming the cache
// so the first request gets a fast cache hit instead of a disk read.
func (s *LocalOAuthStore) RefreshAccessToken(ctx context.Context, account AccountRef) error {
	dir := account.CredentialDir
	if dir == "" {
		return nil
	}
	if len(dir) > 1 && dir[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, dir[2:])
	}

	credsPath := filepath.Join(dir, ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return nil
	}
	var creds claudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil
	}
	token := creds.ClaudeAiOauth.AccessToken
	if token == "" {
		return nil
	}
	result := TokenResult{Token: token}
	if creds.ClaudeAiOauth.ExpiresAt > 0 {
		result.ExpiresAt = time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt)
	}
	// Warm cache regardless of expiry. If stale, the upstream 401 will
	// trigger InvalidateToken and the next retry re-reads from disk.
	tokenCache.Store(dir, &tokenCacheEntry{
		result:    result,
		readAt:    time.Now(),
		credsPath: credsPath,
	})
	return nil
}
