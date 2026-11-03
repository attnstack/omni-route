# omni-route

<p align="center">
  <strong>An OpenAI-compatible LLM gateway that routes each request to the right model by task, cost and latency — tries the cheap model first, upgrades when it isn't good enough, and explains every decision in a response header.</strong>
</p>

<p align="center">
  <a href="README.zh-CN.md">简体中文</a> | English
</p>

<p align="center">
  <a href="https://github.com/attnstack/omni-route/stargazers"><img src="https://img.shields.io/github/stars/attnstack/omni-route?style=social" alt="stars"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="license"></a>
  <a href="https://github.com/attnstack/omni-route/actions"><img src="https://img.shields.io/github/actions/workflow/status/attnstack/omni-route/ci.yml?branch=main" alt="ci"></a>
  <a href="https://github.com/attnstack/omni-route/releases"><img src="https://img.shields.io/github/v/release/attnstack/omni-route" alt="release"></a>
  <img src="https://img.shields.io/badge/go-1.22%2B-00ADD8" alt="go">
  <a href="https://github.com/cuihuan/awesome-ai-gateway"><img src="https://img.shields.io/badge/listed%20in-Awesome%20AI%20Gateway-58a6ff?logo=awesomelists&logoColor=white" alt="listed in awesome ai gateway"></a>
</p>

<p align="center">
  <a href="#-quick-start">Quick Start</a> ·
  <a href="#-how-it-works">How it works</a> ·
  <a href="#-benchmark">Benchmark</a> ·
  <a href="#-known-limitations">Known limitations</a> ·
  <a href="#-contributing">Contributing</a>
</p>

> Running a mix of models is the norm now: DeepSeek for code, Gemini for long context, a small cheap model for easy tasks. But then every app needs its own plumbing for rate limits, retries, fallbacks and cost tracking. omni-route collapses all of that into **one OpenAI-compatible `/v1` endpoint**: point your client's `base_url` at it, and the gateway picks the model, retries, cascades and keeps the books.

**Pipeline**: task classification → cost-quality scoring (Lagrangian with adaptive λ + Thompson Sampling exploration) → cascade execution (cheap model first, confidence check, upgrade only when needed) → rate limiting / retry / circuit breaking / failover → explainable routing headers on every response + a savings dashboard.

## 📸 Demo

![omni-route savings dashboard](docs/dashboard.png)

> Screenshot from the built-in mock provider config (zero API keys): cumulative savings front and center, plus cascade upgrade rate, per-model cost split, per-task attribution, and the live routing quality table (EMA quality and λ).

## ⚡ 30-second Quick Start

No API keys needed — the built-in mock provider runs the whole loop end to end:

```bash
# 1. Start the gateway (requires Go >= 1.22)
go run ./cmd/omni-route -config config.demo.yaml

# 2. In another terminal, send a request (watch the response headers)
curl -sD - -o /dev/null http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-or-task: extraction' \
  -d '{"messages":[{"role":"user","content":"Extract the order info and return JSON: order A-100, 99 USD"}]}'
# Look at: x-or-route-reason / x-or-model / x-or-cost-usd / x-or-savings-usd

# 3. Streaming (standard OpenAI SSE)
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock-mid","messages":[{"role":"user","content":"Tell me a story"}],"stream":true}'

# 4. Open the savings dashboard at http://localhost:8080/
```

To use real models, copy [config.example.yaml](config.example.yaml) and set the `key_env` environment variables (OpenAI / DeepSeek / Anthropic). Clients only need `base_url` pointed at `http://localhost:8080/v1`.

Already running Ollama? Real models, zero API cost:

```bash
# Requires Ollama with qwen2.5:3b and qwen2.5:7b pulled — a real cascade chain: 3b first, judge, then 7b
go run ./cmd/omni-route -config config.ollama.yaml   # listens on :8081

curl -sD - http://localhost:8081/v1/chat/completions -H 'Content-Type: application/json' \
  -d @scripts/demo_request.json
```

Cost-strategy benchmark:

```bash
go run ./cmd/omni-bench -config config.demo.yaml -dataset data/bench_tasks.jsonl
```

## ✨ Features

| Feature | Details |
|---|---|
| OpenAI protocol compatible | `/v1/chat/completions` (streaming SSE + non-streaming), `/v1/models`; works with openai-python and langchain out of the box |
| Multi-protocol upstreams | One adapter covers all OpenAI-style APIs (DeepSeek/Qwen/Ollama), plus full bidirectional Anthropic Messages API translation, including tool calls |
| Task classification | Bilingual rule-based classifier (6 task classes, zero dependencies, microsecond latency); explicit override via the `x-or-task` header |
| Cost-quality routing | `score = Q − λ·C`; λ adapts to budget burn-down; Thompson Sampling (exact Beta posterior sampling) for online exploration |
| Cascade execution | Cheap model first → confidence checks (format / heuristic / LLM judge / self-consistency) → upgrade only when needed; upgrade rate calibrated online toward a target (~30%) |
| Explainable routing | Every response carries `x-or-route-reason`; non-stream cascades can attach a full `omni_route` report; every request logged to SQLite |
| Reliability | Per-provider token buckets (matched to RPM quotas), retry with exponential backoff, sliding-window circuit breaker, cross-model failover, client disconnect propagates upstream |
| Savings dashboard | Single embedded page (built into the binary, no static files): cumulative savings, model share, upgrade trend, per-task cost attribution |
| Reproducible benchmarks | omni-bench compares 4 policies (premium/cheap/cascade/lagrangian) with a fixed seed; runs in CI |

## 🏗 How it works

```mermaid
flowchart LR
    C[Client<br>change base_url, done] --> GW

    subgraph GW[omni-route gateway]
        AUTH[Auth / body limit / logging] --> DEC[Routing decision]
        DEC -->|"classify: 6 task types"| CLS[Rule classifier]
        DEC -->|"hard filters: caps / compliance / context"| F[Filter]
        DEC -->|"score = Q − λ·C<br>Q ~ Beta(a,b) sample"| S[Score + Thompson exploration]
        S -->|non-streaming| CAS[Cascade executor<br>small model → judge → upgrade]
        S -->|streaming| DIRECT[Single model, direct]
        CAS --> EX[Executor<br>rate limit / retry / breaker / failover]
        DIRECT --> EX
        EX --> P1[OpenAI-style adapter]
        EX --> P2[Anthropic adapter]
        EX --> P3[Mock adapter]
        GW -.->|async| DB[(SQLite request log)]
        GW -.->|state snapshot| ST[(state.json)]
    end
    P1 --> UP1[OpenAI/DeepSeek/Ollama]
    P2 --> UP2[Claude]
    P3 --> UP3[Built-in mock]
```

### The core algorithms

**1. Lagrangian cost-quality scoring** (`internal/router`)

```
maximize E[quality]  subject to  E[cost] <= budget (per task type)

score(model) = Q(model) − λ·C(model)
λ ← clip(λ · (1 + η·(spent/budget − 1)), λ_min, λ_max)
```

Q is a per-(task, model) quality EMA (α=0.2); C is the estimated per-request cost from token prices and length. Overspending the budget pushes λ up, which biases routing toward cheaper models; underspending lets λ fall and quality dominate. λ is a shadow price with units "quality per dollar" — init=50 by default, which puts λ·C on the same order as Q∈[0,1] for typical per-request costs (1e-4 to 1e-2 USD).

**2. Thompson Sampling exploration** (`internal/router/bandit.go`)

Each (task, model) pair keeps a Beta(α, β) posterior (continuous rewards in [0,1], standard exponential-family update). At decision time we sample `q̃ ~ Beta(a,b)` and score with the sample instead of the point estimate — cold-started models get explored automatically, and routing converges to the best one as data accumulates. Beta sampling is implemented as a ratio of two Gamma draws (Marsaglia-Tsang rejection sampling), ~30 lines, zero dependencies. The Lagrangian score is the skeleton that keeps spend within budget; the bandit fine-tunes within it. The two layers are decoupled and each can be switched off independently.

**3. Cascade economics** (`internal/cascade`)

Instead of "pick the best model", try the cheap one first and upgrade only when the answer doesn't pass a confidence check. Checks are ordered by marginal cost:

| Tier | Cost | Catches |
|---|---|---|
| Format check | 0 (deterministic) | Invalid JSON, truncation, empty answers |
| Heuristic judge | 0 (local compute) | Over-short answers, refusals, off-topic replies |
| LLM judge (optional) | One small-model call | Semantically bad answers |
| Self-consistency (optional) | k samples | Unstable reasoning outputs |

Fail the check → move up the chain. The threshold is calibrated online: an upgrade rate above target (default 30%) means the bar is too strict (the small model's work is wasted) → relax it; below target means real failures are slipping through → tighten it.

**A deliberate protocol-level tradeoff**: cascades apply to non-streaming requests only. Once a streamed chunk has been sent it cannot be "unsent" — upgrading would resend content to the client. So streaming requests always go to a single directly-selected model. This is a documented decision, not a missing feature.

## 📦 Benchmark

```bash
go run ./cmd/omni-bench -config config.demo.yaml -dataset data/bench_tasks.jsonl
# Setup: dataset = data/bench_tasks.jsonl (48 synthetic tasks, 6 classes x 8);
#        sample size = 48 requests per policy x 4 policies; baseline = premium (always the strongest model);
#        significance = deterministic simulation (fixed seed, identical on every rerun), no sampling error.
```

| Policy | Description | Total cost (48 req) | Accuracy | Upgrades |
|---|---|---|---|---|
| premium | Always the baseline model | $0.054995 | 100% (48/48) | — |
| cheap | Always the cheapest model | $0.002083 | 91.7% (44/48) | — |
| **cascade** | Cheap first + judged upgrades | **$0.004923** | **100% (48/48)** | 4 |
| lagrangian | Q−λC direct selection | $0.003861 | 91.7% (44/48) | — |

> The table above is a **mock simulation** (Windows/amd64, `config.demo.yaml`, seed=42 — fully deterministic, rerun anywhere and you get the same numbers).
> Reading: cheap saves 96.2% over premium but gets 4 answers wrong; **cascade's 4 upgrades fix exactly those 4 answers**, matching premium's accuracy at 91.0% lower cost.
> This is not evidence about real model quality (the oracle is the mock's deterministic generator). Real-model numbers below.

**Real models (local Ollama, 2026-10-31)**: `qwen2.5:3b → qwen2.5:7b` cascade chain, 24 tasks (6 classes x 4). Quality scored by a 7b LLM judge (a proxy metric, and the judge shares lineage with the premium model — treat the numbers as directional), cost at shadow prices (roughly qwen-turbo/plus list prices; see `config.ollama.yaml` comments). Raw data in `reports/bench-ollama.md`.

```bash
go run ./cmd/omni-bench -config config.ollama.yaml -dataset data/bench_tasks_real.jsonl \
  -judge qwen2.5:7b -max-tokens 700 -temperature 0
```

| Policy | Judge pass rate | Mean judge score | Total cost (shadow) | Savings vs premium | Upgrades | Mean latency |
|---|---|---|---|---|---|---|
| premium (always 7b) | 100% (24/24) | 0.917 | $0.008359 | — | 0 | 4.82s |
| cheap (always 3b) | 91.7% (22/24) | 0.892 | $0.001022 | 87.8% | 0 | 3.27s |
| cascade (3b→7b) | 95.8% (23/24) | 0.904 | $0.001692 | 79.8% | 4 | 3.83s |
| lagrangian | 91.7% (22/24) | 0.892 | $0.001022 | 87.8% | 0 | 3.03s |

On real models cascade lands between cheap and premium: 4 points of quality above cheap for 8 points of savings given away. In the mock benchmark cascade matches premium; on real models it doesn't — the discriminator and judge signals aren't sharp enough yet. That's the main weakness being worked on.

Things that went wrong while putting this benchmark together, for the record:

- I initially set `max_tokens` to 220, which truncated every code answer; the format check flagged truncation and upgraded — it looked like "the discriminator upgrades too much" but was a benchmark-parameter bug. The `-detail` flag was added while debugging this.
- Tried 3b as its own cascade judge: indistinguishable from the pure heuristic, and harsh on top (couldn't tell a 0.5 answer from a 0.8 one). Switched to 7b.
- The calibrator was updating its data but the checker kept reading a static threshold — the loop wasn't wired. Fixed, and `TestCheckerWiredCalibratorAffectsDecisions` guards the wiring now.
- Per-task calibration can't fill its window at low traffic (24 tasks ÷ 6 classes), so the calibrator is global-threshold. The first window cold-starts with a few missed upgrades; self-heals once traffic accumulates.

Routing decision overhead (gateway's own latency; `go test -bench=. -benchmem ./internal/router/`, Windows/amd64 @ Ryzen 7 7735H):

| Benchmark | Result |
|---|---|
| BenchmarkDecide (classify + filter + score + TS sample) | 6,134 ns/op, 1,216 B/op, 21 allocs/op |
| BenchmarkDecideExplicit (explicit model passthrough) | 707 ns/op |
| BenchmarkRecordOutcome (state update) | 98 ns/op |
| BenchmarkSampleBeta (Beta posterior sample) | 184 ns/op, 0 allocs |

> Routing decisions cost single-digit microseconds against upstream calls in the hundreds of milliseconds — the gateway adds no perceptible latency. Absolute numbers vary by machine; rerun the benchmarks on your own hardware.

## 🚀 Verify from scratch

```bash
# 0. Prereq: Go >= 1.22
go version

# 1. Dependencies (only 2 direct deps: yaml.v3 + modernc.org/sqlite, pure Go, no CGO)
go mod tidy                                   # expect: go.sum generated, no errors

# 2. Build
go build ./...                                # expect: silent
go vet ./...                                  # expect: no output

# 3. Tests (unit + integration, all on the mock provider, no keys)
go test ./...                                 # expect: all ok, includes end-to-end gateway tests

# 4. Performance benchmarks
go test -bench=. -benchmem ./internal/router/  # expect: microsecond-scale per decision

# 5. Cost-strategy benchmark
go run ./cmd/omni-bench -config config.demo.yaml -dataset data/bench_tasks.jsonl
# expect: 4-policy table; cascade clearly cheaper than premium with comparable accuracy

# 6. Start the demo gateway
go run ./cmd/omni-route -config config.demo.yaml
# expect log: omni-route listening addr=:8080 providers=1 models=3 mode=cascade

# 7. Demo request (another terminal)
curl -sD - -o /dev/null http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-or-task: extraction' \
  -d '{"messages":[{"role":"user","content":"Extract and return JSON: order A-100"}]}'
# expect: 200; x-or-route-reason: task=extraction,tier=cascade,model=mock-premium,upgraded=true,...

# 8. SSE compatibility matrix (Git Bash)
bash scripts/sse_matrix.sh                    # expect: ALL SSE MATRIX CHECKS PASSED

# 9. Dashboard
# Open http://localhost:8080/ — cumulative savings, upgrade rate, recent requests
```

## ⚙️ Configuration reference

<details>
<summary>All configuration keys (with defaults)</summary>

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8080` | Listen address |
| `api_keys` | `[]` (allow + warn) | Gateway auth; clients send `Authorization: Bearer <key>` |
| `seed` | `0` (random) | Non-zero makes routing decisions reproducible |
| `providers.*.kind` | — | `openai` / `anthropic` / `mock` |
| `providers.*.rpm` | 600 | Token-bucket rate (match upstream quota) |
| `models.*.price_in/out` | — | USD per 1M tokens; basis for all cost accounting |
| `models.*.caps` | — | streaming/tools/json_mode/max_context_tokens; hard-filter inputs |
| `routing.mode` | `cascade` | `direct` / `lagrangian` / `cascade` |
| `routing.budget_per_task` | — | USD per 1k requests; the constraint λ adapts to |
| `routing.baseline` | priciest model | The savings reference point ("always the strongest model") |
| `routing.compliance_groups` | — | Data-class groups: `x-or-data-class` restricts eligible providers |
| `routing.lambda.{init,min,max,eta}` | 50/1/5000/0.05 | λ adaptation parameters |
| `cascades.<task>` | — | Cascade chain (≤ max_upgrades+1 tiers) |
| `cascade.confidence.threshold` | 0.6 | Initial confidence threshold (drifts with online calibration) |
| `cascade.confidence.llm_judge` | off | Judge model + sample rate (async, never blocks the request) |
| `reliability.{retries,backoff_base_ms}` | 2/200 | Retry policy (idempotent setups only, before first byte) |
| `reliability.breaker.*` | 300s/20/0.2/30s | Sliding-window circuit breaker |
| `timeouts.{connect,first_byte,total}_ms` | 5000/60000/300000 | Upstream timeout trio |

</details>

## 🔌 API extensions

A small set of gateway-specific endpoints beyond the OpenAI protocol:

| Endpoint | Description |
|---|---|
| Request header `x-or-task` | Explicit task type (`code/reasoning/summarization/translation/extraction/chat`), bypasses the classifier |
| Request header `x-or-data-class` | Data-compliance class; on match with `compliance_groups` restricts eligible providers |
| `POST /v1/feedback` | `{"request_id":"...","verdict":"good"\|"bad"}` — a thumbs-down lowers that (task, model) quality score directly |
| `GET /api/stats`, `GET /api/requests` | Dashboard data sources |
| `POST /admin/reload` | Hot-reload config (requires `admin_key_env`) |
| Response headers `x-or-*` | route-reason / model / cost / savings / request-id / failover-from |
| Response body `omni_route` | Full cascade report on non-streaming cascades (per-tier score, cost, upgrade reason) |

## Known limitations

1. **Cascades don't cover streaming** — a protocol-level tradeoff (see above). Possible future work: buffer the first N tokens for judging + SSE retry semantics, at the cost of first-token latency.
2. **Rule-based classifier ceiling** — mixed intents ("translate this code") resolve by score max and can misclassify; `x-or-task` override plus the feedback loop are the backstops. No automatic correction.
3. **The heuristic judge is a proxy metric** — relevance = keyword coverage (with cross-language neutralization), completeness = length ratio; it can be fooled by answers that parrot the question. Refusal keywords false-positived once on a translation containing "cannot" in tests. The LLM judge is optional, and its own capability sets the ceiling on discrimination (tried 3b as a judge; see Benchmark).
4. **Calibration threshold is global** — per-task thresholds need enough per-task traffic to fill the window; they silently do nothing at small scale (learned the hard way). The global threshold cold-starts with occasional missed upgrades in the first window. Future work: per-task thresholds with traffic guarantees, or Bayesian prior blending.
5. **Single-process state** — quality/λ/breakers live in memory; snapshots persist to disk, but replicas would each learn separately. Future work: external state (Redis) + reconciliation.
6. **Token estimation ±20%** — when a provider doesn't report usage, the gateway estimates via a CJK/Latin heuristic and flags it `estimated`; for cost-critical setups use `stream_options.include_usage` (the gateway sets it automatically).
7. **Mock benchmark ≠ real quality** — the mock cost comparison is a simulation; the real-data run is local Ollama + shadow prices + a judge metric (see Benchmark). Re-running with real cloud keys is still TODO.
8. **No semantic cache** — exact-duplicate requests could be answered for free; not implemented yet.
9. **No multimodal passthrough** — text content only; image/audio content arrays are degraded.

## 🤝 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Quick check:

```bash
gofmt -l -w . && go vet ./... && go test -race ./...
```

Before submitting: new features need tests; PRs touching the core algorithms (router/cascade) should state the expected effect on benchmark numbers.

## 📄 License

[MIT](LICENSE)
