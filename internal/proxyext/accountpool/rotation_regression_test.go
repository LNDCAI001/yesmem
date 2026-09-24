package accountpool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
)

// writeCreds creates <tmp>/<name>/.credentials.json with the given expiry.
func writeCreds(t *testing.T, name string, expires time.Time) accountpool.AccountRef {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "tok-" + name, "refreshToken": "r", "expiresAt": expires.UnixMilli(),
	}})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return accountpool.AccountRef{Name: name, CredentialDir: dir}
}

// Regression (proxy.log 2026-08-06 19:28:20-21): with cooldown_seconds_after_429
// unset (0), a 429 left the account instantly "available" and the sticky
// selector retried the SAME exhausted account instead of rotating.
func TestPool_429WithUnsetCooldownRotatesToNextAccount(t *testing.T) {
	a := writeCreds(t, "a", time.Now().Add(time.Hour))
	b := writeCreds(t, "b", time.Now().Add(time.Hour))
	p, err := accountpool.NewPool(accountpool.Config{Enabled: true, MaxPreStreamRetries: 2, CooldownSeconds: 0,
		Accounts: []accountpool.AccountRef{a, b}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := p.SelectAndGetToken(context.Background(), accountpool.RequestMeta{})
	if err != nil || first.Name != "a" {
		t.Fatalf("first select = %q, %v; want a", first.Name, err)
	}
	if retry, _ := p.ShouldRetry(429, 0, first); !retry {
		t.Fatal("429 should be retryable")
	}
	second, _, err := p.SelectAndGetToken(context.Background(), accountpool.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != "b" {
		t.Fatalf("after 429 on a, selected %q; want b (rotation)", second.Name)
	}
}

// The proxy cannot refresh OAuth tokens itself, so an account whose stored
// token is already expired can only produce 401s. It must be skipped rather
// than burning one of the few pre-stream retries.
func TestPool_skipsAccountWithExpiredToken(t *testing.T) {
	dead := writeCreds(t, "dead", time.Now().Add(-48*time.Hour))
	live := writeCreds(t, "live", time.Now().Add(time.Hour))
	p, err := accountpool.NewPool(accountpool.Config{Enabled: true, MaxPreStreamRetries: 2, CooldownSeconds: 300,
		Accounts: []accountpool.AccountRef{dead, live}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	acc, tok, err := p.SelectAndGetToken(context.Background(), accountpool.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Name != "live" || tok.Token != "tok-live" {
		t.Fatalf("selected %q (%q); want live", acc.Name, tok.Token)
	}
}
