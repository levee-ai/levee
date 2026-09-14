# Recorded provider fixtures

These are live-captured response bodies from the real OpenAI and Anthropic
APIs, sanitized as listed below. They are replayed by
internal/proxy/fixture_test.go, which asserts exact end-to-end budget
accounting against the usage numbers inside these bytes.

## Capture metadata (script-written, do not hand-edit this block)

- Captured: 2026-09-13 19:29 UTC
- OpenAI model: gpt-4o-mini (non-streaming Content-Type: application/json, streaming: text/event-stream; charset=utf-8)
- Anthropic model: claude-haiku-4-5-20251001 (non-streaming Content-Type: application/json, streaming: text/event-stream; charset=utf-8)
- Prompt: "Reply with the single word ok." with max_tokens 16
- Capture script commit: d9e2f85
- Redactions applied: response id values to REDACTED_ID, system_fingerprint to
  REDACTED_FINGERPRINT. Nothing else was altered. The jq pass reserializes the
  .json files (structure-faithful). The .sse files are byte-faithful outside
  the redacted lines. The OpenAI created timestamp is kept deliberately as the
  in-band capture-time witness.

## Rules for these files

- Capture is manual and user-run. CI only replays the committed bytes. Never
  automate capture in CI or store provider keys in CI secrets.
- The set is all-four-or-nothing. Never commit a partial or mixed-vintage set.
- Re-capture procedure: run this script with your own keys, then review the
  git diff for any NEW field that could identify your account before
  committing. Re-capture when a provider announces response-format changes and
  before cutting a release.
- Streaming fixtures were captured with stream_options.include_usage (OpenAI),
  matching what the proxy injects into forwarded streaming requests.
- benchmarks/ (planned harness) is a second consumer of these files. If they
  move, update both consumers.
- Ground truth for per-file age: git log -1 -- testdata/fixtures/
