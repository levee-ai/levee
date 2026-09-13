# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Reverse proxy for OpenAI-style and Anthropic-style APIs with first-class
  SSE streaming, byte-faithful forwarding, and phase-split provider timeouts
  (connect, response header, idle, request) so no total cap severs a healthy
  stream.
- Header-based agent identification with three enforcement modes per agent:
  enforce, observe, and passthrough.
- Budget enforcement with pre-call estimation, reservation, and 429 rejection.
  Post-response reconciliation from verified provider usage, including
  extraction from final streaming events, with conservative forfeiture when
  usage cannot be verified.
- Token budgets over rolling and fixed windows, and dollar budgets priced in
  integer microdollars with separate input and output rates per model. Model
  pricing resolves versioned model ids by longest prefix, and any model not in
  the pricing table is charged a conservative maximum-price fallback so an
  unpriced model can never under-count.
- Prometheus metrics on the admin listener and periodic JSON state snapshots
  that restore budget usage across restarts.
- Admin API: agent status, budget reset, and a durable pause kill switch that
  survives restarts and crashes, guarded against browser confused-deputy
  attacks on the loopback listener.
- Recorded live provider response fixtures with replay tests asserting exact
  end-to-end budget accounting.
