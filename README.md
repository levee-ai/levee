<p align="center">
  <img src="assets/levee-banner.png" alt="Levee">
</p>

<p align="center"><em>Hard spending limits for AI agents.</em></p>

# Levee

[![Go version](https://img.shields.io/github/go-mod/go-version/levee-ai/levee)](https://github.com/levee-ai/levee/blob/main/go.mod)
[![License](https://img.shields.io/github/license/levee-ai/levee)](https://github.com/levee-ai/levee/blob/main/LICENSE)
[![CI](https://github.com/levee-ai/levee/actions/workflows/ci.yml/badge.svg)](https://github.com/levee-ai/levee/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/levee-ai/levee)](https://goreportcard.com/report/github.com/levee-ai/levee)
[![Go Reference](https://pkg.go.dev/badge/github.com/levee-ai/levee.svg)](https://pkg.go.dev/github.com/levee-ai/levee)

Stops AI agents from draining your budget, deleting your database, or running
forever. Safety infrastructure for agent fleets. Today Levee enforces token and
dollar budgets: it is a reverse proxy between your agents and LLM providers
that refuses to forward a request once the agent's budget is gone. Action-level
guardrails and runtime duration limits are on the roadmap below. Application
layer policies hope agents follow the rules. Levee enforces budgets at the
infrastructure layer, outside the agent process, and the security section below
describes the egress blocking that makes that boundary real.

## Quickstart

Five minutes from install to an enforced budget. You need an OpenAI API key in
`OPENAI_API_KEY`, or an Anthropic key in `ANTHROPIC_API_KEY` for the Anthropic
variant below.

Install, picking one. A prebuilt binary needs no toolchain. The command below is
macOS arm64, and the
[releases page](https://github.com/levee-ai/levee/releases/latest) has linux and
darwin for amd64 and arm64, plus `checksums.txt`:

```bash
curl -sL https://github.com/levee-ai/levee/releases/download/v0.1.0/levee_0.1.0_darwin_arm64.tar.gz | tar xz levee
```

From source, which needs Go 1.26 or later and `$(go env GOPATH)/bin` on your PATH:

```bash
go install github.com/levee-ai/levee/cmd/levee@latest
```

Or skip the binary entirely and run the container, see
[Running in Docker](#running-in-docker).

Write a minimal config:

```bash
mkdir -p /tmp/levee-quickstart && cd /tmp/levee-quickstart
cat > levee.yaml <<'EOF'
listen:
  proxy_port: 8080
  admin_port: 9090
  admin_bind: "127.0.0.1"

state:
  snapshot_path: "./levee-state.json"
  snapshot_interval: "30s"

providers:
  - name: openai
    upstream: "https://api.openai.com"
  - name: anthropic
    upstream: "https://api.anthropic.com"

agents:
  - name: "researcher"
    identifier:
      type: header
      header_name: "X-Levee-Agent"
      header_value: "researcher"
    mode: enforce
    budgets:
      - type: tokens
        limit: 100000
        window: "1h"
        window_type: rolling

defaults:
  unknown_agent: block
  unknown_model_tokenizer: "cl100k_base"
EOF
```

Validate it and start the proxy:

```bash
levee validate --config levee.yaml
levee serve --config levee.yaml &
```

`validate` prints `config valid` and exits. `serve` starts two listeners: the
proxy on port 8080 across all interfaces, and the admin API on 127.0.0.1:9090,
loopback only by default.

Send a request through the proxy. The agent identifies itself with one header,
and its provider API key passes through untouched:

```bash
curl -sS http://localhost:8080/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Levee-Agent: researcher" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -d '{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word ok."}]}'
```

The response is the provider's ordinary chat completion. Behind it, Levee
reserved the estimated cost before forwarding, then reconciled the reservation
to the usage the provider reported. Substitute any model your key can access.

The Anthropic path works the same way. The first path segment names the
provider and the rest is forwarded as-is. The `x-api-key` and
`anthropic-version: 2023-06-01` headers are required by the provider:

```bash
curl -sS http://localhost:8080/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -H "X-Levee-Agent: researcher" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word ok."}]}'
```

Inspect the budget and stop:

```bash
curl -sS http://127.0.0.1:9090/agents
curl -sS http://127.0.0.1:9090/health
ls -l ./levee-state.json
kill %1
```

`/agents` shows the settled accounting. This output is from a real run, after
the single OpenAI request above consumed 16 tokens:

```json
{"agents":[{"name":"researcher","mode":"enforce","paused":false,"in_flight":0,"budgets":[{"type":"tokens","limit":100000,"used":16,"reserved":0,"remaining":99984}]}]}
```

The state file is created with mode 0600. It first appears when the initial
snapshot is written, within one `snapshot_interval` (30 seconds in this
config), so an immediate `ls` can report no such file. `kill %1` triggers a
graceful shutdown that writes a final state snapshot regardless.

## Running in Docker

```bash
docker run -p 8080:8080 -p 9090:9090 \
  -v "$PWD/levee.yaml:/etc/levee/config.yaml:ro" \
  -v "$PWD/levee-state:/var/lib/levee" \
  ghcr.io/levee-ai/levee:0.1.0
```

Published for `linux/amd64` and `linux/arm64` at
[ghcr.io/levee-ai/levee](https://github.com/levee-ai/levee/pkgs/container/levee).

The image is built `FROM scratch`, so it has no writable filesystem and no `/tmp`.
`snapshot_path` has to resolve inside a mounted volume or Levee refuses to start,
and that volume has to be writable by uid 1000, which the container runs as.

`admin_bind` defaults to `127.0.0.1`, which is unreachable from outside the
container. Setting it to `0.0.0.0` exposes the admin API, which has no
authentication of its own, so put something in front of it.

## How it works

> [!WARNING]
> Levee is fail-safe, not fail-open. If the Levee process is down, agents
> cannot reach providers at all. This is deliberate: a budget guard that fails
> open is not a guard. If you need fail-open semantics, front Levee with a load
> balancer fallback route, and accept that the fallback path is unmetered.

An agent points its OpenAI or Anthropic base URL at Levee and adds one
identifying header. Levee resolves the agent from that header, estimates the
request's cost, checks the budget, and either forwards the request or refuses
it. No SDK and no code changes beyond the base URL and the header.

The accounting is built around one guarantee. **The budget Levee enforces is
never less than what the provider actually consumed, even when the provider
fails.** Providers time out, omit the `usage` object, emit malformed SSE, drop
streams mid-generation, and clients disconnect before the final usage event
arrives. A spend tracker that only counts confirmed usage silently
under-counts in exactly those moments, which means an agent can keep spending
after its budget is gone. Levee's accounting is conservative by construction:

- **Reserve.** Before a request is forwarded, Levee estimates its worst-case
  cost and reserves that amount against the agent's budget.
- **Reconcile.** When the provider returns verified usage, the reservation is
  adjusted to the actual cost.
- **Forfeit.** When usage cannot be verified (timeout, missing usage, broken
  stream, client disconnect), the reservation is kept, never refunded on hope.
  Over-counting is bounded and visible. Under-counting is never allowed.

The same discipline applies to token budgets and dollar budgets. Dollar
budgets are priced per model with separate input and output rates, in integer
microdollars, from the table in `internal/budget/pricing.go`. A model missing
from that table is charged at the highest known rate and logged, so an unknown
model can never under-count.

Streaming is first-class. SSE responses are forwarded as they arrive, and
usage is read from the final events of the stream (OpenAI emits a terminal
usage chunk, Anthropic reports input tokens in `message_start` and output
tokens in `message_delta`). On OpenAI streaming requests Levee injects
`stream_options.include_usage` into the forwarded body so that terminal usage
chunk exists. An idle watchdog bounds silent streams, and a stream that dies
before its usage arrives forfeits the full reservation.

Every agent runs in one of three modes:

- `enforce` (the default): full enforcement. A request that would exceed the
  budget is refused with a 429 before anything reaches the provider.
- `observe`: budgets are tracked, breaches are logged and counted in metrics,
  and requests are never blocked. Start here to collect baseline spend.
- `passthrough`: no budget accounting. Requests are forwarded untouched.

**Measured overhead.** The proxy hop adds 0.281ms to median latency at a
150-byte prompt and 500 requests per second, against a 0.558ms direct baseline.
Enforcement adds more and scales with prompt size, so no figure here is
meaningful without its payload attached, and no post-fix enforcement number is
published yet. Methodology, the evidence run, and the validity bands it was
judged against are in [benchmarks/](benchmarks/README.md).

When an enforce-mode agent's budget cannot cover a request's estimated cost,
Levee refuses it without forwarding anything upstream:

```
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 14400
X-Budget-Remaining: 0.00045
```

```json
{
  "error": {
    "type": "budget_exhausted",
    "message": "dollars budget exhausted for agent \"researcher\"",
    "agent": "researcher",
    "budget": {
      "type": "dollars",
      "limit": 50.00,
      "used": 49.99955,
      "remaining": 0.00045,
      "reset_at": "2026-09-14T00:00:00Z"
    }
  }
}
```

Token budgets render as integers. Dollar budgets render as decimals with
microdollar precision, trimmed to at least two decimal places, so a fully
spent budget reads `0.00` and a nearly spent one reads `0.00045`.
`Retry-After` counts the seconds until the binding budget's window resets. A
remaining balance that went negative is clamped to zero in this body, while
the admin API shows the raw value.

## Configuration reference

Levee reads one YAML file. Unknown keys are rejected, validation reports every
error at once, and `levee validate --config <path>` checks a file without
starting anything. The `--config` flag takes the path as a separate argument.
A commented full example lives at `configs/example.yaml`.

### `listen`

```yaml
listen:
  proxy_port: 8080
  admin_port: 9090
  admin_bind: "127.0.0.1"
```

- `proxy_port` (required, 1 to 65535): agent traffic. The proxy binds all
  interfaces (0.0.0.0).
- `admin_port` (required, 1 to 65535, must differ from `proxy_port`): the
  admin API, `/health`, and `/metrics`.
- `admin_bind` (default `"127.0.0.1"`): admin listener address. Levee warns at
  startup when this is widened beyond loopback, because the admin API has no
  authentication.

### `state`

```yaml
state:
  snapshot_path: "/var/lib/levee/state.json"
  snapshot_interval: "30s"
```

- `snapshot_path` (required): where budget state is persisted as JSON so usage
  and pauses survive restarts. The parent directory must already exist
  (`validate` refuses otherwise) and `serve` probes it for writability at
  startup. The file is written with mode 0600.
- `snapshot_interval` (required): how often state is written, a duration from
  `1s` to `5m`.

### `providers`

```yaml
providers:
  - name: openai
    upstream: "https://api.openai.com"
    timeouts:
      connect: "10s"
      response_header: "120s"
      idle: "120s"
      request: "600s"
```

- `name` (required, unique): the first path segment agents call. A request to
  `/openai/v1/chat/completions` is forwarded to the `openai` upstream at
  `/v1/chat/completions`.
- `upstream` (required): the provider base URL, `https` only. The one exception
  is `http://` on a literal loopback address such as `127.0.0.1` or `::1`
  (written `http://[::1]:9999`), which exists for local mock upstreams during
  development and benchmarking. On a plaintext upstream the pass-through API
  keys travel unencrypted on that hop and are readable by any local process
  that can capture or bind the port, so never use it for a real provider. Levee
  logs a warning at startup for each plaintext upstream, with any URL-embedded
  password redacted. Hostnames are not accepted for `http://`, including
  `localhost`, because a hostname is resolved when the connection is made and
  could point off-box.
- `timeouts` (optional, defaults shown above): the timeout policy is split by
  phase so a healthy stream is never severed by a total cap.
  - `connect` (default `10s`, bounds `1s` to `60s`): TCP connect.
  - `response_header` (default `120s`, bounds `5s` to `600s`): time to first
    byte on streaming requests. Raise this for slow reasoning models that
    think for a long time before the first token.
  - `idle` (default `120s`, bounds `5s` to `600s`): the longest silent gap
    allowed between stream chunks, enforced by the idle watchdog.
  - `request` (default `600s`, bounds `5s` to `900s`): total duration cap for
    non-streaming requests only. Streaming carries no total cap on purpose: a
    healthy stream can legitimately run for many minutes, so it is bounded per
    phase by `response_header` and then `idle` instead.

### `agents`

```yaml
agents:
  - name: "researcher"
    identifier:
      type: header
      header_name: "X-Levee-Agent"
      header_value: "researcher"
    mode: enforce
    budgets:
      - type: tokens
        limit: 1000000
        window: "1h"
        window_type: rolling
      - type: dollars
        limit: 50.00
        window: "24h"
        window_type: fixed
        reset_at: "00:00Z"
```

- `name` (required, unique).
- `identifier` (required): how requests map to this agent. `type` must be
  `header`. `header_name` and `header_value` are both required, and the value
  match is case sensitive. Two agents cannot share the same header name and
  value pair (header names compare case-insensitively).
- `mode` (default `enforce`): `enforce`, `observe`, or `passthrough`,
  semantics under How it works above.
- `budgets`: required for `enforce` and `observe`, optional for `passthrough`
  (still validated to catch typos, ignored at runtime).
  - `type`: `tokens` or `dollars`.
  - `limit`: greater than zero. Token limits must be integers. Dollar limits
    carry at most 2 decimal places and at most one billion dollars (amounts
    are stored as integer microdollars, and the ceiling keeps a fat-fingered
    limit from silently saturating).
  - `window` (required): any duration of `1s` or longer, for example `1h` or
    `24h`.
  - `window_type` (required): `rolling` (a sliding window over the trailing
    period) or `fixed` (resets at a wall-clock instant).
  - `reset_at`: required for `fixed` windows, `HH:MMZ` format, UTC.

### `defaults`

```yaml
defaults:
  unknown_agent: block
  unknown_model_tokenizer: "cl100k_base"
```

- `unknown_agent` (required): `block` rejects requests that match no
  configured agent with a 403. `passthrough` forwards them unmetered, and
  `serve` warns at startup that pause and budgets do not cover that traffic.
- `unknown_model_tokenizer` (required): the tiktoken encoding used to estimate
  tokens for models the estimator does not recognize. One of `cl100k_base`,
  `p50k_base`, `p50k_edit`, `r50k_base`, `gpt2`, `o200k_base`.

## Admin API

The admin listener (default `127.0.0.1:9090`) serves five management routes
plus health and metrics. It has no authentication in the MVP, which is why it
stays on loopback (see Security considerations).

| Route | What it does |
|---|---|
| `GET /agents` | Every configured agent with mode, paused flag, in-flight reservation count, and per-budget limit, used, reserved, and remaining |
| `GET /agents/{name}` | One agent, same shape |
| `POST /agents/{name}/reset` | Clears committed usage on every budget |
| `POST /agents/{name}/pause` | Kill switch, new requests from the agent get 429 until unpaused |
| `POST /agents/{name}/unpause` | Lifts the pause |
| `GET /health` | Liveness plus snapshot recency |
| `GET /metrics` | Prometheus exposition |

`GET /agents` output appears in the quickstart above. `remaining` on this
surface is raw and can go negative, a deliberate divergence from the 429 body,
which clamps it for client consumption. `reset_at` renders only for fixed
windows. `/health` reports:

```json
{"last_snapshot_at":"2026-09-13T19:46:16Z","snapshot_age_seconds":0,"status":"ok","version":"dev"}
```

`last_snapshot_at` and `snapshot_age_seconds` appear only after the first
successful snapshot write, so their absence right after startup means the
snapshotter has not ticked yet.

Pause is a kill switch, applied ahead of mode: enforce, observe, and
passthrough agents all stop. A paused agent's requests get a 429 with error
type `agent_paused` and an advisory `Retry-After: 60` (a pause has no reset
instant, the header only slows well-behaved retry loops). It gates new
admissions only, so in-flight requests and open streams finish. It survives
restarts because every admin mutation forces a synchronous snapshot write
before responding:

```bash
curl -sS -X POST http://127.0.0.1:9090/agents/researcher/pause
```

```json
{"action":"pause","agent":"researcher","paused":true,"persisted":true,"status":"ok"}
```

`persisted: false` means the snapshot write failed and the change is in
memory only: it still applies immediately, but a crash before the next
successful snapshot would lose it.

Reset clears committed usage and reports what it cleared, in each budget's own
unit. In-flight reservations are untouched and settle normally. Resetting a
passthrough agent returns 409, there are no budgets to reset.

Two browser confused-deputy guards cover the whole listener. Under a loopback
bind, requests whose Host header is not loopback are rejected (DNS rebinding
defense, also covering `/health` and `/metrics`, so point same-host scrapers
at 127.0.0.1). Mutating POSTs that carry a non-loopback Origin header are
rejected (a cross-site bodyless POST skips CORS preflight). curl sends no
Origin header, so command-line use is unaffected.

## Metrics

`GET /metrics` on the admin port serves these families. Agent label values are
the configured agent names plus `unknown` for traffic that matched no agent.

| Family | Labels | Meaning |
|---|---|---|
| `levee_usage_missing_total` | agent, provider | Successful provider responses that carried no usage field (the reservation was forfeited) |
| `levee_sse_parse_error_total` | agent, provider | Streams ended by a scanner error, a transport read failure or an oversized line |
| `levee_estimation_drift` | agent, provider | Histogram of (actual - estimated) / estimated for settlements with authoritative usage |
| `levee_forfeit_total` | agent, provider, reason | Reservations forfeited in full, by reason |
| `levee_negative_budget_total` | agent | Settlements that pushed committed usage past the limit, counted at the crossing |
| `levee_stream_read_timeout_total` | agent, provider | Streams ended by the idle watchdog |
| `levee_stream_upstream_drop_total` | agent, provider | Streams that reached EOF without a terminal marker |
| `levee_observe_breach_total` | agent | Budget breaches in observe mode (the request was forwarded anyway) |
| `levee_tiktoken_fallback_total` | agent, provider | Settlements where at least one usage half was estimated rather than provider-reported |
| `levee_reconcile_error_total` | agent, operation | Budget store operations that failed during settlement |

Every known label combination is pre-initialized at startup so rates work from
the first scrape. The registry also serves the standard Go and process
collectors.

## Security considerations

Levee's MVP trust model is a network perimeter around agents that are honest
but potentially buggy.

- **Header identity is trusted as sent.** An agent that sends another agent's
  header value spends that agent's budget, and one that sends a passthrough
  agent's value escapes metering entirely. This spoofing risk is accepted for
  the MVP's single-team threat model, where the enemy is a bug or a loop
  rather than a hostile agent. HMAC-signed agent identities are on the
  roadmap.
- **You must block direct provider egress from agent networks.** Levee cannot
  stop an agent that changes its base URL back to the real provider. Use
  network policy (Kubernetes NetworkPolicy, Docker network rules, or firewall
  rules) to make Levee the only egress path to LLM providers. This deployment
  step is what makes the infrastructure-layer enforcement boundary real.
- **Keep the admin API on loopback.** It has no authentication, and it can
  reset budgets and unpause agents, so anyone who can reach it can disarm your
  kill switch. The Host and Origin guards defend against browser
  confused-deputy tricks only, they are no substitute for the loopback bind.
- **Protect the state file.** It is written with mode 0600 and carries budget
  usage and the paused-agent set, so tampering could grant spend or disarm a
  pause. Keep it on a volume only the Levee process user can access.
- **Provider keys pass through untouched.** Agents send their own
  `Authorization` or `x-api-key` headers. Levee forwards them without storing,
  logging, validating, or rewriting them. They stay encrypted in transit
  because provider upstreams are `https` only. The one exception is a plaintext
  `http://` upstream on a literal loopback address, allowed for local mock
  upstreams, where the keys travel unencrypted on that hop and are readable by
  any local process that can capture or bind the port. Never use it for a real
  provider.

## Roadmap

None of the following exists in the code today:

- Action-level guardrails, allow and deny rules for what an agent may do
  beyond spending
- Runtime duration limits, bounding how long an agent may keep running
- HMAC-signed agent identities, closing the header spoofing gap
- Hot config reload

## Contributing and license

Contributions are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md) for scope
and workflow. Report vulnerabilities per [SECURITY.md](SECURITY.md). Levee is
licensed under [Apache 2.0](LICENSE).
