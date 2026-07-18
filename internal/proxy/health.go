package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

// ProxyStats tracks aggregate proxy metrics.
type ProxyStats struct {
	startTime     time.Time
	TotalRequests int64
	TotalStubs    int64
	TokensSaved   int64
}

// RecordRequest records metrics for a completed request.
func (s *ProxyStats) RecordRequest(stubCount int, tokensBefore, tokensAfter int) {
	if s == nil {
		return
	}
	atomic.AddInt64(&s.TotalRequests, 1)
	atomic.AddInt64(&s.TotalStubs, int64(stubCount))
	saved := tokensBefore - tokensAfter
	if saved > 0 {
		atomic.AddInt64(&s.TokensSaved, int64(saved))
	}
}

// handleHealth serves the /health endpoint with JSON status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.stats.startTime).Round(time.Second)

	// C1 fix: read annotations count under lock
	s.mu.RLock()
	annCount := len(s.annotations)
	s.mu.RUnlock()

	resp := map[string]any{
		"status":       "ok",
		"uptime":       fmt.Sprintf("%s", uptime),
		"requests":     atomic.LoadInt64(&s.stats.TotalRequests),
		"stubs":        atomic.LoadInt64(&s.stats.TotalStubs),
		"tokens_saved": atomic.LoadInt64(&s.stats.TokensSaved),
		"annotations":  annCount,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleAccounts serves a per-account SMM pool status view (which account
// is active, health, and remaining 5h/7d rate-limit budgets). Returns 200
// with an "enabled":false body when SMM is disabled.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := proxyext.ActivePoolAccounts()
	resp := map[string]any{
		"enabled":  proxyext.IsEnabled(),
		"accounts": accounts,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleAccountToggle returns a handler that enables (enable=true) or disables
// (enable=false) a pool account at runtime. Usage:
//
//	GET/POST /accounts/enable?name=cdaniel
//	GET/POST /accounts/disable?name=cdaisy
//
// Disabling removes the account from rotation immediately; enabling clears the
// disable flag and resets its health so it can be selected again.
func (s *Server) handleAccountToggle(enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing ?name="})
			return
		}
		if !proxyext.ActivePoolSetEnabled(name, enable) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown account or pool disabled", "name": name})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": name, "enabled": enable})
	}
}
