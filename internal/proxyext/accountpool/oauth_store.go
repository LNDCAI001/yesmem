package accountpool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// claudeCredsFile is the credential filename written by Claude Code.
// Location: <credential_dir>/.credentials.json
const claudeCredsFile = ".credentials.json"

// claudeCredentials mirrors the JSON structure Claude Code writes.
// Only the fields needed for token injection are mapped.
type claudeCredentials struct {
	ClaudeAI struct {
		OAuthToken struct {
			AccessToken  string `json:"access_token"`
			ExpiresAt    int64  `json:"expires_at"` // unix ms or s depending on version
			RefreshToken string `json:"refresh_token"`
		} `json:"oauth_token"`
	} `json:"claudeAi"` // key used by Claude Code ≥ some version
}

// OAuthStore implements proxyext.TokenProvider by reading Claude Code's
// local credential files. No network calls; refresh is out of scope for v1.
// If the token is expired, it is still returned — let the upstream 401
// trigger account rotation rather than attempting refresh here.
type OAuthStore struct{}

var _ proxyext.TokenProvider = (*OAuthStore)(nil)

func (s *OAuthStore) GetAccessToken(_ context.Context, account proxyext.AccountRef) (proxyext.TokenResult, error) {
	path := filepath.Join(account.CredentialDir, claudeCredsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return proxyext.TokenResult{}, fmt.Errorf("accountpool: read credentials %q: %w", path, err)
	}

	var creds claudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return proxyext.TokenResult{}, fmt.Errorf("accountpool: parse credentials %q: %w", path, err)
	}

	tok := creds.ClaudeAI.OAuthToken.AccessToken
	if tok == "" {
		return proxyext.TokenResult{}, fmt.Errorf("accountpool: empty access_token in %q", path)
	}

	// Convert expiry: Claude Code may write ms or s.
	// Normalise to unix seconds for our TokenResult.
	exp := creds.ClaudeAI.OAuthToken.ExpiresAt
	if exp > 1e12 {
		exp = exp / 1000 // was milliseconds
	}

	_ = time.Unix(exp, 0) // validate parseable; not enforced in v1

	return proxyext.TokenResult{
		AccessToken: tok,
		ExpiresAt:   exp,
	}, nil
}
