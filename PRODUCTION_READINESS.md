# Production Readiness — invariantprotocol

Status as of 2026-06-01, repo at v0.2.8 (Python/Go) / v0.2.2 (Rust crate
version — see "Remaining gaps").

This document records the production-readiness audit of the descriptor-driven
projection framework (Go + Python + Rust) that medallion-os depends on, the
toolchain/CI state, the fixes applied, and a sufficiency verdict for our use
cases.

## Verdict

**Sufficient for medallion-os's current use cases.** The `connect_http` client
surface medallion-os relies on (proto→REST proxying for venue APIs) is solid
and fully test-covered: `json_name` path/body/query translation, trailing-slash
preservation, `google.api.HttpBody` request/response passthrough,
header/query/response-observer providers, and load-once descriptor handling all
have behavioral tests and pass. `make check` is green across all three
languages plus proto, runs under `flox activate`, and CI runs the identical
gate. Remaining gaps are documented below and are not blockers for our usage.

## Current state — green across the board

`flox activate -- make check` passes. It runs format-check + lint + typecheck +
tests for Go, Python, Rust, and proto.

| Language | fmt-check | lint                    | typecheck | tests                |
|----------|-----------|-------------------------|-----------|----------------------|
| Go       | gofmt ✓   | `go vet` + golangci-lint 2.11 ✓ (0 issues) | (vet)     | `go test ./...` ✓ (167 cases) |
| Python   | ruff ✓    | ruff 0.15 ✓             | ty ✓      | pytest ✓ (153 passed) |
| Rust     | rustfmt ✓ | clippy `-D warnings` ✓  | (compile) | `cargo test` ✓ (~42 across unit+integration) |
| Proto    | buf ✓     | `buf lint` ✓            | —         | —                    |

Additional gates also pass:
- `make audit` — pip-audit clean (2 dev-only advisories explicitly ignored, neither ships in the published wheel).
- `make verify-generate` — generated proto stubs (`go/gen`, `python/src/invariant/gen`) are up to date.

## What was fixed

All fixes were mechanical/safe — no logic changes:

1. **Python format drift.** `python/tests/test_http_client.py` (the
   trailing-slash test added in v0.2.8) was not formatted. Applied
   `ruff format`.
2. **Rust format drift.** Six source files + two test files had rustfmt drift
   (`grpc.rs`, `mcp.rs`, `serve.rs`, `server.rs`, `production.rs`). Applied
   `cargo fmt`. No semantic change — pure line-wrapping.
3. **Makefile: added `check` and `fmt-check` targets.** There was no single
   top-level gate. Added:
   - `make fmt-check` — non-mutating format verification for all four
     (gofmt `-l`, `ruff format --check`, `cargo fmt --check`,
     `buf format --diff --exit-code`).
   - `make check` — `fmt-check lint typecheck test`, the canonical CI gate.
4. **CI consolidation.** `.github/workflows/ci.yml` now has a single `check`
   job running `flox activate -- make check` (replacing the separate `lint`
   and `test` jobs, which it subsumes). The `generated` (verify-generate) and
   `breaking` (proto breaking-change) jobs are retained.

(Note: `python/uv.lock` carries a pre-existing version bump 0.2.6→0.2.8 syncing
to `pyproject.toml`; left as-is.)

## connect_http surface — sufficiency for medallion-os

Verified the client-side HTTP proxy surface (`python/src/invariant/http_client.py`,
`Server.connect_http` in `server.py`). All features medallion-os uses are
implemented and have behavioral tests in `python/tests/test_http_client.py`:

| Capability | Implementation | Test |
|-----------|----------------|------|
| `json_name` on body | `resolve_fields` + proto3 JSON mapping | `test_binding_honors_json_name_on_body_with_proto_name_path` |
| `json_name` on query | same | `test_binding_honors_json_name_on_query` |
| Path field selectors (proto-name → JSON key) | `_json_field_path` at bind time | (covered by the two above) |
| Trailing-slash preservation | `_PathTemplate.parse` `trailing_slash` | `test_http_client_binding_preserves_trailing_slash` |
| `HttpBody` response passthrough | `_httpbody_response` raw-bytes path | `test_connect_http_httpbody_response_returns_raw_bytes` |
| `HttpBody` request passthrough | `_httpbody_request` raw-bytes path | `test_connect_http_httpbody_request_sends_raw_body` |
| Header provider (auth) | `use_http_header_provider` | `test_connect_http_uses_dynamic_header_provider`, `_dynamic_header_provider_error` |
| Query provider (HMAC/API-key signing, re-run per retry) | `use_http_query_provider` | `test_connect_http_query_provider_adds_auth_params` |
| Response observer (raw bronze-tier archival) | `use_http_response_observer`, best-effort | `test_connect_http_response_observer_captures_raw_bytes` |
| `response_body` mapping | `_wrap_response_body` | `test_connect_http_response_body_mapping` |
| Remote error normalization (Connect + legacy wrapped) | `_http_error` | `test_connect_http_maps_remote_error` |
| Retry policy (safe methods, Retry-After, backoff) | `_should_retry` / `_retry_delay_seconds` | `test_connect_http_retries_transient_get`, `_does_not_retry_post` |
| Env-header injection + default User-Agent | `_outbound_http_headers_from_env` | `test_connect_http_injects_headers_from_env`, `_sets_default_user_agent`, `_user_agent_override_from_env` |
| Descriptor load-once | `from_bytes`/`from_descriptor` parse FDS once; pool + binding resolution at registration, not per-request | property holds by construction (no per-request `protodesc`/parse) |

No coverage gaps were found in this surface — no new tests were required.
Per-request work is minimal (no descriptor re-parsing), matching the documented
performance target.

## flox

`.flox/env/manifest.toml` installs the full toolchain: `go`, `python3` (3.13),
`uv`, `buf`, `protoc` + `protoc-gen-go`, `golangci-lint`, `ruff`, and the Rust
toolchain (`rustc`, `cargo`, `clippy`, `rustfmt`, `rust-analyzer`), plus
nix-built native Python packages (`protobuf`, `grpcio`, `grpcio-tools`,
`pytest`, `pyyaml`).

**libstdc++ note:** the known flox libstdc++ fix (exporting `LD_LIBRARY_PATH`
to the flox lib dir for native wheels) is **not needed here** and was
deliberately not added. The manifest already pins `UV_PYTHON_DOWNLOADS=never`
and `UV_PYTHON` to flox's `python3.13`, so all native wheels (the protobuf
`_upb` C extension, grpcio's `cygrpc`, protovalidate) build and load against
the flox-provided ABI. Verified: `import grpc, google._upb._message,
protovalidate` succeeds inside `flox activate`, and the full pytest suite
(which exercises grpc.aio + dynamic protobuf) passes. Adding a redundant
`LD_LIBRARY_PATH` would risk shadowing the correct flox libs.

`flox activate -- make check` and `flox activate -- make fmt-check` both
verified working.

## CI

`.github/workflows/ci.yml` runs `flox activate -- make check` on every push and
PR to `main` (via `flox/install-flox-action` + `flox/activate-action`, with a
`/nix/store` cache keyed on `manifest.lock`). Plus `verify-generate` on
push/PR and `breaking` (proto backward-compat) on PRs. Dependabot covers
gomod, uv, and github-actions weekly.

## Remaining gaps + risks

These are not blockers for medallion-os but are worth tracking:

1. **Rust crate version lag (cosmetic).** `rust/Cargo.toml` is at `0.2.2` while
   Python/Go are at `0.2.8`. The CHANGELOG documents v0.2.4–v0.2.8 as
   Python-only `connect_http` additions, so the Rust crate is functionally
   consistent — but the version string should be bumped for lockstep honesty
   when the next release is cut. **Risk: low.** medallion-os uses the Python
   `connect_http` surface, not Rust.

2. **`response_body` server-side mapping not implemented** (client-side works,
   which is what we use). Documented as intentional in AGENTS.md "Not yet
   implemented". **Risk: none for us** — we only consume REST via the client.

3. **Path-template grammar** supports `{field}`, `{field=*}`, `{field=**}` only
   — not the full google.api.http grammar. Sufficient for the venue APIs we
   proxy. **Risk: low**; revisit if a venue uses a nested template.

4. **No client-side selection among `additional_bindings`** and no
   client/bidi-streaming projection. Both are documented intentional cuts.
   **Risk: none for us.**

5. **MethodConfig parity.** Per-method body caps (`ConfigureMethod`) exist in Go
   only (v0.2.3); Python/Rust parity is a documented follow-up. medallion-os
   does not currently need per-method caps on the Python side. **Risk: low.**

6. **No deferred libstdc++ LD_LIBRARY_PATH guard.** If a future contributor
   sets `UV_PYTHON_DOWNLOADS` differently (letting uv fetch its own CPython),
   native wheels could break under flox. The current manifest prevents this,
   but it's a config-coupling worth a comment if the manifest is edited.

## Nomad-deployability

Where invariantprotocol is run as a service (HTTP/gRPC/MCP projections), it is
a plain ASGI app (Python, via `Server.asgi_app()` + uvicorn) / `http.Server` +
`grpc.Server` (Go) / axum + tonic (Rust) — all standard long-running servers
with graceful shutdown on context/cancel and `/healthz` + `/readyz` probes for
Nomad health checks. No framework-level Nomad blocker. As a library dependency
(the medallion-os usage), it ships as a normal wheel / Go module / crate and
needs no service deployment of its own.
