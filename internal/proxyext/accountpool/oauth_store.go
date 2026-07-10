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

const tokenCacheTTL = 30 * time.Second

// tokenCache is the process-level cache keyed on the absolute credential dir path.
// sync.Map is chosen because reads vastly outnumber writes in normal operation.
var tokenCache sync.Map // map[string]*tokenCacheEntry

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
type LocalOAuthStore struct{}

// claudeCredentials mirrors the subset of fields we need from .credentials.json.
type claudeCredentials struct {
	ClaudeAIAccessToken string `json:"claudeAiSessionToken"` // subscription session token
	ExpiresAt           string `json:"expiresAt"`            // RFC3339 or empty
	AccessToken         string `json:"access_token"`         // API-key style, fallback
}

// GetAccessToken implements TokenProvider.
// On cache hit (within tokenCacheTTL and not invalidated), returns the cached
// token without a disk read. On cache miss or invalidation, reads the
// credential file, validates the token, and updates the cache.
func (s *LocalOAuthStore) GetAccessToken(_ context.Context, account AccountRef) (TokenResult, error) {
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

	// Prefer subscription session token; fall back to access_token field.
	token := creds.ClaudeAIAccessToken
	if token == "" {
		token = creds.AccessToken
	}
	if token == "" {
		return TokenResult{}, fmt.Errorf("accountpool: no usable token in %q", credsPath)
	}

	result := TokenResult{Token: token}
	if creds.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, creds.ExpiresAt); err == nil {
			result.ExpiresAt = t
		}
	}

	// Reject already-expired tokens before hitting the API. A 30-second grace
	// period avoids false positives from clock skew.
	const expiryGrace = 30 * time.Second
	if !result.ExpiresAt.IsZero() && time.Now().After(result.ExpiresAt.Add(-expiryGrace)) {
		return TokenResult{}, fmt.Errorf(
			"accountpool: token expired (or expiring within %s) for account %q (expires %s)",
			expiryGrace, account.Name, result.ExpiresAt.Format(time.RFC3339))
	}

	// Populate cache.
	tokenCache.Store(dir, &tokenCacheEntry{
		result:    result,
		readAt:    time.Now(),
		credsPath: credsPath,
	})

	return result, nil
}
