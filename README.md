# Levee

Budget and safety guardrails for AI agents, enforced outside the agent. Levee is a
reverse proxy that sits between your agents and LLM providers and refuses to let a
runaway agent spend past its limits.

## The guarantee Levee is built around

**The budget Levee enforces is never less than what the provider actually consumed,
even when the provider fails.** Providers time out, omit the `usage` object, emit
malformed SSE, drop streams mid-generation, and clients disconnect before the final
usage event arrives. Most spend-tracking tools silently under-count in exactly those
moments, which means an agent can keep spending after its budget is gone. Levee's
accounting is conservative by construction:

- **Reserve.** Before a request is forwarded, Levee estimates its worst-case cost and
  reserves that amount against the agent's budget.
- **Reconcile.** When the provider returns verified usage, the reservation is adjusted
  to the actual cost.
- **Forfeit.** When usage cannot be verified (timeout, missing usage, broken stream,
  client disconnect), the reservation is kept, never refunded on hope. Over-counting is
  bounded and visible. Under-counting is never allowed.

The same discipline applies to token budgets and dollar budgets.

**Fail-safe posture:** when Levee cannot evaluate a request against a budget, it
refuses the request rather than waving it through. A guardrail that fails open is not
a guardrail.

## Status

Pre-release, working toward v0.1.0. Working today:

- Streaming and non-streaming proxying for OpenAI-style and Anthropic-style APIs
- Header-based agent identification
- Three enforcement modes per agent: enforce, observe, passthrough
- Phase-split provider timeouts (connect, response header, idle) tuned for streaming
- Pre-call token estimation, budget reservation, and 429 enforcement
- Post-response token reconciliation from verified usage, including SSE final-event
  extraction
- Dollar budgets with microdollar-precision pricing

In progress (see `docs/ROADMAP.md`): Prometheus metrics and state snapshots, admin API,
recorded real-provider test fixtures, and a reproducible open-loop benchmark harness
with published methodology.

## How it fits your stack

Levee is a metering and enforcement layer, deliberately narrow. It decides how much an
agent may spend and stops it at the line. It composes with, and does not replace,
action-authorization layers, content guardrails, and observability platforms. Point
your agent's OpenAI or Anthropic base URL at Levee, give each agent an identifying
header, and set its budget in the config.

## Quick start

See `configs/example.yaml` for a commented configuration and `docs/architecture/` for
the design records (error handling and the accounting decision table, streaming design,
timeout policy, security model). A full quickstart lands with v0.1.0.

## License

See `LICENSE`.
