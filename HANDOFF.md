# SMM Proxy Extension — Operator Handoff

## What Is Deployed
Branch: feature/smm-proxyext-v1

All proxyext scaffolding is complete and verified:
- internal/proxyext/types.go — ForwardContext (BytesFlushed, SelectedAccount, OriginalBody)
- internal/proxyext/extension.go — BeforeForward, OnPreStreamResponse
- internal/proxyext/hooks.go — dispatcher with panic recovery
- internal/proxyext/noop.go — no-op implementation
- internal/proxyext/accountpool/ — 7 files, fully tested, race-clean
- internal/proxy/proxy_forward_smm.go — retry loop, s.httpClient, smmWinningAuth
- internal/proxy/proxy_forward.go — SMM gate wired, early return, keepalive/fork patched

## Verified Build State
- go build ./...  PASS
- go test -race ./internal/proxyext/...  PASS, zero DATA RACE

## To Enable SMM
Add to yesmem config:
  smm:
    enabled: true
    account_pool:
      enabled: true
      max_pre_stream_retries: 2
      accounts:
        - credential_dir: ~/.claude/          # primary account
        - credential_dir: ~/.claude-work/     # second account

## Security Invariants (Do Not Remove)
- x-api-key stripped in both canonical and raw form (extension.go)
- Token strings never logged (oauth_store.go redacts before logging)
- SelectedAccount never written to any outbound header

## Feature B (staticplan)
Gated at mode: off. Do not wire TransformStaticPayload until
compress_context.go block ordering is verified.
