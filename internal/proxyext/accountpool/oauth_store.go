package accountpool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// oauthTokenEndpoint is the Claude OAuth refresh endpoint.
const oauthTokenEndpoint = "https://claude.ai/api/auth/oauth/token"

// defaultHTTPClient is package-level so tests can swap it.
var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// RefreshAccessToken implements TokenProvider. It checks whether the stored
// token is expired or within 5 minutes of expiry and, if so, calls the Claude
// OAuth refresh endpoint to obtain a new access token. On success the updated
// credentials are written back to the credential file on disk.
func (s *LocalOAuthStore) RefreshAccessToken(ctx context.Context, account AccountRef) error {
	dir := account.CredentialDir
	if dir == "" {
		return fmt.Errorf("accountpool: empty credential_dir for account %q", account.Name)
	}
	if len(dir) > 1 && dir[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("accountpool: expand home for %q: %w", account.Name, err)
		}
		dir = filepath.Join(home, dir[2:])
	}

	credsPath := filepath.Join(dir, ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return fmt.Errorf("accountpool: read credentials %q: %w", credsPath, err)
	}

	var creds claudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("accountpool: parse credentials %q: %w", credsPath, err)
	}

	// No refresh token available — cannot refresh.
	if creds.ClaudeAiOauth.RefreshToken == "" {
		return fmt.Errorf("accountpool: no refresh token in %q", credsPath)
	}

	// Check whether refresh is needed: expired or within 5-minute grace.
	const refreshGrace = 5 * time.Minute
	needsRefresh := false
	if creds.ClaudeAiOauth.ExpiresAt > 0 {
		expiry := time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt)
		if time.Now().After(expiry.Add(-refreshGrace)) {
			needsRefresh = true
		}
	} else {
		// No expiry timestamp — assume it needs rotation.
		needsRefresh = true
	}
	if !needsRefresh {
		return nil // still fresh
	}

	// Build the refresh request.
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", creds.ClaudeAiOauth.RefreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenEndpoint, bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return fmt.Errorf("accountpool: create refresh request for %q: %w", account.Name, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("accountpool: refresh request failed for %q: %w", account.Name, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("accountpool: read refresh response for %q: %w", account.Name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("accountpool: refresh failed for %q: HTTP %d: %s", account.Name, resp.StatusCode, string(respBody))
	}

	// Parse response — Claude returns accessToken + expiresIn (seconds).
	var refreshResp struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
		// The response may also include a new refreshToken; we keep the
		// existing one if none is returned.
		RefreshToken string `json:"refreshToken,omitempty"`
	}
	if err := json.Unmarshal(respBody, &refreshResp); err != nil {
		return fmt.Errorf("accountpool: parse refresh response for %q: %w", account.Name, err)
	}

	if refreshResp.AccessToken == "" {
		return fmt.Errorf("accountpool: refresh response missing accessToken for %q", account.Name)
	}

	// Compute the new expiry.
	var newExpiresAt int64
	if refreshResp.ExpiresIn > 0 {
		newExpiresAt = time.Now().Add(time.Duration(refreshResp.ExpiresIn) * time.Second).UnixMilli()
	} else {
		// Fallback: extend by 24 hours (Claude OAuth default).
		newExpiresAt = time.Now().Add(24 * time.Hour).UnixMilli()
	}

	// Update the credentials struct.
	creds.ClaudeAiOauth.AccessToken = refreshResp.AccessToken
	creds.ClaudeAiOauth.ExpiresAt = newExpiresAt
	if refreshResp.RefreshToken != "" {
		creds.ClaudeAiOauth.RefreshToken = refreshResp.RefreshToken
	}

	updated, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("accountpool: marshal updated credentials for %q: %w", account.Name, err)
	}

	if err := os.WriteFile(credsPath, updated, 0600); err != nil {
		return fmt.Errorf("accountpool: write updated credentials %q: %w", credsPath, err)
	}

	return nil
}

// GetAccessToken implements TokenProvider.
// On cache hit (within tokenCacheTTL and not invalidated), returns the cached
// token without a disk read. On cache miss or invalidation, reads the
// credential file, validates the token, and updates the cache.
func (s *LocalOAuthStore) GetAccessToken(ctx context.Context, account AccountRef) (TokenResult, error) {
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

	// Proactive refresh: if the token is expired or within 5 minutes of expiry,
	// try to refresh before returning. This avoids unnecessary 401 failures
	// and keeps the account pool healthy.
	if !result.ExpiresAt.IsZero() {
		const refreshThreshold = 5 * time.Minute
		if time.Now().After(result.ExpiresAt.Add(-refreshThreshold)) {
			if err := s.RefreshAccessToken(ctx, account); err != nil {
				// Refresh failed — fall through to the strict check below.
				// Logging is done at the Pool level.
			} else {
				// Refresh succeeded — re-read the updated file.
				data, err = os.ReadFile(credsPath)
				if err == nil {
					if err := json.Unmarshal(data, &creds); err == nil {
						token = creds.ClaudeAiOauth.AccessToken
						result = TokenResult{Token: token}
						if creds.ClaudeAiOauth.ExpiresAt > 0 {
							result.ExpiresAt = time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt)
						}
					}
				}
			}
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
