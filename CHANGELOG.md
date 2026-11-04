# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/); versioning follows SemVer.

## [v0.2.0] - 2026-11-05

### Added
- Response cache for non-streaming requests: exact-match on a canonical hash of
  the request body, zero-temperature-only by default (`cache.mode: always` to
  widen), TTL + LRU eviction, `x-or-cache: hit|miss|bypass` response header,
  `X-Or-Cache: bypass` to opt out. Cache hits cost nothing and their avoided
  cost is booked as savings in the log, dashboard and metrics.
- Prometheus `/metrics` endpoint (hand-rolled text format, zero new
  dependencies): request/token/cost/savings counters, cache hits & misses,
  cascade upgrades.
- Docker support: multi-stage `Dockerfile` (distroless runtime, non-root),
  `docker-compose.yml`, `config.docker.yaml`, and a release workflow that
  publishes `ghcr.io/attnstack/omni-route` images plus cross-platform
  binaries via GoReleaser on `v*` tags.
- Version injection at build time (`-ldflags -X ...buildinfo.Version`),
  surfaced by `/healthz`, `/api/stats` and `-version`.
- API key comparison now uses constant-time comparison.

### Changed
- `gateway.BuildWorld` takes a `*cache.Cache` parameter; the cache instance
  survives config hot-reloads (only its TTL/capacity are reconfigured).

## [v0.1.0] - 2026-10-31

### Added
- OpenAI-compatible gateway: `/v1/chat/completions` (SSE + non-streaming),
  `/v1/models`; OpenAI-family and Anthropic upstream adapters; deterministic
  built-in mock provider.
- Task classifier, hard filters (capability / compliance / context length),
  Lagrangian cost-quality routing with adaptive λ, Thompson Sampling
  exploration.
- Cascade executor with tiered confidence checks (format / heuristic /
  optional LLM judge / self-consistency) and online upgrade-threshold
  calibration.
- Reliability layer: per-provider token buckets, retry with backoff,
  sliding-window circuit breaker, cross-model failover, client-cancel
  propagation.
- Explainable routing headers (`x-or-route-reason`), SQLite request log,
  embedded savings dashboard.
- Deterministic 4-policy benchmark harness (`omni-bench`).
