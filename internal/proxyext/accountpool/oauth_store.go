package accountpool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LocalOAuthStore reads Claude credentials from the local filesystem.
// It looks for .credentials.json inside the configured credential_dir,
// the same location used by Claude Code / Claude CLI.
type LocalOAuthStore struct{}

// claudeCredentials mirrors the subset of fields we need from .credentials.json.
type claudeCredentials struct {
	ClaudeAIAccessToken string `json:"claudeAiSessionToken"` // subscription session token
	ExpiresAt           string `json:"expiresAt"`            // RFC3339 or empty
	AccessToken         string `json:"access_token"`         // API-key style, if present
}

// GetAccessToken implements TokenProvider.
// Returns an error if the credential file is missing, unparseable, contains no
// usable token, or if the token is already expired at the time of reading.
// An expired-token error allows the retry classifier to rotate to the next
// account immediately without burning an API round-trip to receive a 401.
func (s *LocalOAuthStore) GetAccessToken(_ context.Context, account AccountRef) (TokenResult, error) {
	dir := account.CredentialDir
	if dir == "" {
		return TokenResult{}, fmt.Errorf("accountpool: empty credential_dir for account %q", account.Name)
	}

	// Expand ~ manually since os.Open does not do shell expansion.
	if len(dir) > 1 && dir[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return TokenResult{}, fmt.Errorf("accountpool: expand home for %q: %w", account.Name, err)
		}
		dir = filepath.Join(home, dir[2:])
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

	// Reject already-expired tokens before hitting the API.
	// This lets the pool's FailureTokenInvalid classifier rotate to the next
	// account without a wasted round-trip. A small grace period (30 s) avoids
	// false positives from clock skew between the host and Anthropic's servers.
	const expiryGrace = 30 * time.Second
	if !result.ExpiresAt.IsZero() && time.Now().After(result.ExpiresAt.Add(-expiryGrace)) {
		return TokenResult{}, fmt.Errorf("accountpool: token expired (or expiring within %s) for account %q (expired %s)",
			expiryGrace, account.Name, result.ExpiresAt.Format(time.RFC3339))
	}

	return result, nil
}
