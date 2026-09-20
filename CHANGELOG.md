# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Documented running the published container image, including that the `scratch`
  image has no writable filesystem, so `snapshot_path` must resolve inside a
  mounted volume owned by uid 1000.
- Prebuilt binary and container install paths in the quickstart, so installing no
  longer requires a Go toolchain.

### Changed

- The quickstart no longer reads as though Levee needs a provider API key
  configured. The key belongs to the calling agent, and Levee forwards it without
  storing, logging, or validating it.

## [0.1.0] - 2026-09-20

First release.

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
- Provider upstreams may use `http://` when the host is a literal loopback
  address such as `127.0.0.1` or `::1`, for local mock upstreams during
  development and benchmarking. Levee warns at startup for each plaintext
  upstream, because the pass-through API keys travel unencrypted on that hop.
  Hostnames are not accepted for `http://`, including `localhost`, because a
  hostname is resolved when the connection is made and could point off-box.

### Fixed

- The enforcement path tokenized every request body twice, once to size the
  budget reservation and again to label the settlement drift log line and the
  drift histogram. The reservation estimate is now carried out of admission to
  the settlement site, so an enforced request tokenizes once. Tokenizing is
  linear in prompt bytes, roughly 120ns per byte, so the saving grows with the
  prompt. Measured on an Apple M3 Pro with a new enforced-path benchmark, one
  non-streaming enforced request drops from 146us to 126us at a 150-byte
  prompt, from 1.18ms to 0.73ms at 4KB, and from 7.57ms to 4.39ms at 32KB, with
  allocations per request roughly halved at the larger sizes (8,040 to 4,122 at
  4KB, and 62,412 to 31,404 at 32KB). The existing proxy benchmark could not
  see any of this, because its proxy configures no agents and never reaches the
  estimator.
- The settlement drift log line and the drift histogram reported an estimate
  that was not always the one the reservation was made against. An OpenAI
  streaming request has `stream_options` injected after admission, and a body
  with no recognizable `messages` array is counted whole by the estimator, so
  for that request shape the drift was computed against a larger estimate than
  the budget actually reserved. Both now use the reserved value. Every other
  request shape reports exactly the same estimate, drift, and histogram
  observation as before.
