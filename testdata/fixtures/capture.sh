#!/bin/bash
# Captures real OpenAI and Anthropic response fixtures for the replay tests.
# Run this in YOUR OWN terminal. Never run it through an agent shell or CI.
# Requires OPENAI_API_KEY and ANTHROPIC_API_KEY in the environment, plus jq.
# Cost: four requests with max_tokens 16, a fraction of a cent total.
#
# Usage:
#   bash testdata/fixtures/capture.sh            capture from the live APIs
#   bash testdata/fixtures/capture.sh --selftest sanitize-verify pipeline check, no network
#
# Override models if the defaults are no longer served (they must prefix-match
# a model in internal/budget/pricing.go so the dollar-budget tests bind):
#   OPENAI_MODEL=gpt-4o-mini ANTHROPIC_MODEL=claude-3-5-haiku-20241022 bash capture.sh
set -euo pipefail

OPENAI_MODEL="${OPENAI_MODEL:-gpt-4o-mini}"
ANTHROPIC_MODEL="${ANTHROPIC_MODEL:-claude-3-5-haiku-20241022}"
FIXTURE_ROOT="$(cd "$(dirname "$0")" && pwd)"
PROMPT="Reply with the single word ok."

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

scrub() {
  sed -E -e 's/sk-[A-Za-z0-9_-]{4,}/REDACTED_KEY/g' -e 's/req_[A-Za-z0-9]+/REDACTED_REQUEST_ID/g'
}

fail() {
  printf 'capture.sh: %s\n' "$1" | scrub >&2
  exit 1
}

require_tool() {
  if ! command -v "$1" >/dev/null 2>&1
  then
    fail "$1 is required and was not found"
  fi
}

# sanitize_json FILE: redact identifying fields in place-adjacent temp, print output path.
# jq reserializes, so .json fixtures are structure-faithful, not byte-faithful.
# The created field is deliberately KEPT as the in-band capture-time witness.
sanitize_json() {
  local input="$1" output="$1.clean"
  jq '(if has("id") then .id = "REDACTED_ID" else . end)
      | (if has("system_fingerprint") and .system_fingerprint != null then .system_fingerprint = "REDACTED_FINGERPRINT" else . end)' \
    "$input" > "$output"
  printf '%s' "$output"
}

# sanitize_sse FILE: sed line rewrites only, bytes otherwise untouched.
sanitize_sse() {
  local input="$1" output="$1.clean"
  sed -E -e 's/"id":"(chatcmpl|msg)_[^"]*"/"id":"REDACTED_ID"/g' \
         -e 's/"id":"chatcmpl-[^"]*"/"id":"REDACTED_ID"/g' \
         -e 's/"system_fingerprint":"[^"]*"/"system_fingerprint":"REDACTED_FINGERPRINT"/g' \
    "$input" > "$output"
  printf '%s' "$output"
}

# verify_redacted FILE KIND: the zero-replacement drift detector plus residual scan.
verify_redacted() {
  local file="$1" kind="$2"
  if ! grep -q 'REDACTED_ID' "$file"
  then
    fail "$kind sanitizer made zero id replacements, provider id format drifted, update the patterns"
  fi
  if grep -Eq 'chatcmpl-[A-Za-z0-9]|msg_[A-Za-z0-9]|fp_[A-Za-z0-9]|req_[A-Za-z0-9]|sk-ant-|sk-[A-Za-z0-9_-]{16,}' "$file"
  then
    fail "$kind still carries a live identifier after sanitization, refusing to keep it"
  fi
}

verify_lf_only() {
  if grep -q $'\r' "$1"
  then
    fail "$2 contains CR bytes, expected LF-only SSE, do not commit"
  fi
}

verify_markers() {
  local file="$1" kind="$2"
  case "$kind" in
    openai-json)
      jq -e '.usage.prompt_tokens > 0 and .usage.completion_tokens > 0' "$file" >/dev/null || fail "openai json fixture lacks positive usage"
      ;;
    anthropic-json)
      jq -e '.usage.input_tokens > 0 and .usage.output_tokens > 0' "$file" >/dev/null || fail "anthropic json fixture lacks positive usage"
      ;;
    openai-sse)
      grep -q '"prompt_tokens"' "$file" || fail "openai stream lacks a usage chunk, was stream_options.include_usage sent"
      grep -q '^data: \[DONE\]' "$file" || fail "openai stream lacks the DONE terminal marker"
      ;;
    anthropic-sse)
      grep -q '^event: message_start' "$file" || fail "anthropic stream lacks message_start"
      grep -q '"input_tokens"' "$file" || fail "anthropic stream lacks input_tokens in message_start usage"
      grep -q '^event: message_delta' "$file" || fail "anthropic stream lacks a message_delta usage event"
      grep -q '"output_tokens"' "$file" || fail "anthropic stream lacks output_tokens"
      grep -q '^event: message_stop' "$file" || fail "anthropic stream lacks message_stop"
      ;;
  esac
  local first_line
  first_line="$(grep -m1 -v '^[[:space:]]*$' "$file" || true)"
  case "$kind" in
    *-sse)
      case "$first_line" in
        event:*|data:*) : ;;
        *) fail "$kind first line is not an SSE field, headers may have leaked into the body" ;;
      esac
      ;;
    *-json)
      jq -e . "$file" >/dev/null 2>&1 || fail "$kind does not parse as JSON from byte zero"
      ;;
  esac
}

# capture URL AUTH_HEADER EXTRA_HEADER BODY OUT_BODY OUT_CONTENT_TYPE: one request,
# body-only capture. EXTRA_HEADER is optional, empty string means none.
# Auth and the extra header go through curl --config on stdin so the key never
# appears in argv. -q must stay the FIRST curl argument, curlrc suppression only
# works in that position, and a user curlrc with verbose or trace-ascii would
# dump auth headers during capture.
capture() {
  local url="$1" auth_header="$2" extra_header="$3" body="$4" out_body="$5" content_type_var="$6"
  local header_file="$WORK_DIR/headers.$$" status
  status="$(
    {
      printf 'header = "%s"\n' "$auth_header"
      if [ -n "$extra_header" ]
      then
        printf 'header = "%s"\n' "$extra_header"
      fi
    } | curl -q -sS -o "$out_body" -D "$header_file" -w '%{http_code}' \
      -H 'Content-Type: application/json' \
      --config - --data "$body" "$url"
  )"
  if [ "$status" = "429" ]
  then
    fail "provider returned 429, rate limited, wait a minute and re-run"
  fi
  if [ "$status" != "200" ]
  then
    printf 'capture.sh: %s returned %s, response body follows\n' "$url" "$status" >&2
    scrub < "$out_body" >&2
    exit 1
  fi
  local content_type
  content_type="$(grep -i '^content-type:' "$header_file" | head -1 | tr -d '\r' | cut -d' ' -f2-)"
  rm -f "$header_file"
  eval "$content_type_var=\"\$content_type\""
}

write_metadata() {
  local script_sha
  script_sha="$(git -C "$FIXTURE_ROOT" rev-parse --short HEAD 2>/dev/null || printf 'unknown')"
  cat > "$WORK_DIR/README.md" <<METADATA
# Recorded provider fixtures

These are live-captured response bodies from the real OpenAI and Anthropic
APIs, sanitized as listed below. They are replayed by
internal/proxy/fixture_test.go, which asserts exact end-to-end budget
accounting against the usage numbers inside these bytes.

## Capture metadata (script-written, do not hand-edit this block)

- Captured: $(date -u '+%Y-%m-%d %H:%M UTC')
- OpenAI model: $OPENAI_MODEL (non-streaming Content-Type: $OPENAI_JSON_CONTENT_TYPE, streaming: $OPENAI_SSE_CONTENT_TYPE)
- Anthropic model: $ANTHROPIC_MODEL (non-streaming Content-Type: $ANTHROPIC_JSON_CONTENT_TYPE, streaming: $ANTHROPIC_SSE_CONTENT_TYPE)
- Prompt: "$PROMPT" with max_tokens 16
- Capture script commit: $script_sha
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
METADATA
}

selftest() {
  printf 'selftest: exercising sanitize, verify, and marker gates with canned payloads\n'
  local openai_json="$WORK_DIR/selftest-openai.json" openai_sse="$WORK_DIR/selftest-openai.sse"
  local anthropic_json="$WORK_DIR/selftest-anthropic.json" anthropic_sse="$WORK_DIR/selftest-anthropic.sse"
  local clean
  cat > "$openai_json" <<'CANNED'
{"id":"chatcmpl-abc123","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","system_fingerprint":"fp_44709d6fcb","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}
CANNED
  cat > "$openai_sse" <<'CANNED'
data: {"id":"chatcmpl-selftest1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","system_fingerprint":"fp_selftest1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}

data: {"id":"chatcmpl-selftest1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","system_fingerprint":"fp_selftest1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-selftest1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","system_fingerprint":"fp_selftest1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}

data: [DONE]
CANNED
  cat > "$anthropic_json" <<'CANNED'
{"id":"msg_selftest1","type":"message","role":"assistant","model":"claude-3-5-haiku-20241022","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":2}}
CANNED
  cat > "$anthropic_sse" <<'CANNED'
event: message_start
data: {"type":"message_start","message":{"id":"msg_abc123","model":"claude-3-5-haiku-20241022","usage":{"input_tokens":12,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
CANNED
  clean="$(sanitize_json "$openai_json")"
  verify_redacted "$clean" "selftest-openai-json"
  verify_markers "$clean" "openai-json"
  clean="$(sanitize_sse "$openai_sse")"
  verify_redacted "$clean" "selftest-openai-sse"
  verify_markers "$clean" "openai-sse"
  verify_lf_only "$clean" "selftest-openai-sse"
  clean="$(sanitize_json "$anthropic_json")"
  verify_redacted "$clean" "selftest-anthropic-json"
  verify_markers "$clean" "anthropic-json"
  clean="$(sanitize_sse "$anthropic_sse")"
  verify_redacted "$clean" "selftest-anthropic-sse"
  verify_markers "$clean" "anthropic-sse"
  verify_lf_only "$clean" "selftest-anthropic-sse"
  printf 'selftest: PASS\n'
}

require_tool jq
require_tool curl

if [ "${1:-}" = "--selftest" ]
then
  selftest
  exit 0
fi

if [ -z "${OPENAI_API_KEY:-}" ]
then
  fail "OPENAI_API_KEY is not set"
fi
if [ -z "${ANTHROPIC_API_KEY:-}" ]
then
  fail "ANTHROPIC_API_KEY is not set"
fi

printf 'Capturing four fixtures (models: %s, %s)\n' "$OPENAI_MODEL" "$ANTHROPIC_MODEL"

OPENAI_BODY="{\"model\":\"$OPENAI_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}"
OPENAI_STREAM_BODY="{\"model\":\"$OPENAI_MODEL\",\"max_tokens\":16,\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}"
ANTHROPIC_BODY="{\"model\":\"$ANTHROPIC_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}"
ANTHROPIC_STREAM_BODY="{\"model\":\"$ANTHROPIC_MODEL\",\"max_tokens\":16,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}"

capture "https://api.openai.com/v1/chat/completions" "Authorization: Bearer $OPENAI_API_KEY" "" "$OPENAI_BODY" "$WORK_DIR/openai.json" OPENAI_JSON_CONTENT_TYPE
capture "https://api.openai.com/v1/chat/completions" "Authorization: Bearer $OPENAI_API_KEY" "" "$OPENAI_STREAM_BODY" "$WORK_DIR/openai.sse" OPENAI_SSE_CONTENT_TYPE
capture "https://api.anthropic.com/v1/messages" "x-api-key: $ANTHROPIC_API_KEY" "anthropic-version: 2023-06-01" "$ANTHROPIC_BODY" "$WORK_DIR/anthropic.json" ANTHROPIC_JSON_CONTENT_TYPE
capture "https://api.anthropic.com/v1/messages" "x-api-key: $ANTHROPIC_API_KEY" "anthropic-version: 2023-06-01" "$ANTHROPIC_STREAM_BODY" "$WORK_DIR/anthropic.sse" ANTHROPIC_SSE_CONTENT_TYPE

OPENAI_JSON_CLEAN="$(sanitize_json "$WORK_DIR/openai.json")"
OPENAI_SSE_CLEAN="$(sanitize_sse "$WORK_DIR/openai.sse")"
ANTHROPIC_JSON_CLEAN="$(sanitize_json "$WORK_DIR/anthropic.json")"
ANTHROPIC_SSE_CLEAN="$(sanitize_sse "$WORK_DIR/anthropic.sse")"

verify_redacted "$OPENAI_JSON_CLEAN" "openai-json"
verify_redacted "$OPENAI_SSE_CLEAN" "openai-sse"
verify_redacted "$ANTHROPIC_JSON_CLEAN" "anthropic-json"
verify_redacted "$ANTHROPIC_SSE_CLEAN" "anthropic-sse"
verify_markers "$OPENAI_JSON_CLEAN" "openai-json"
verify_markers "$OPENAI_SSE_CLEAN" "openai-sse"
verify_markers "$ANTHROPIC_JSON_CLEAN" "anthropic-json"
verify_markers "$ANTHROPIC_SSE_CLEAN" "anthropic-sse"
verify_lf_only "$OPENAI_SSE_CLEAN" "openai-sse"
verify_lf_only "$ANTHROPIC_SSE_CLEAN" "anthropic-sse"

write_metadata

mkdir -p "$FIXTURE_ROOT/openai" "$FIXTURE_ROOT/anthropic"
mv "$OPENAI_JSON_CLEAN" "$FIXTURE_ROOT/openai/chat-completion.json"
mv "$OPENAI_SSE_CLEAN" "$FIXTURE_ROOT/openai/chat-completion-stream.sse"
mv "$ANTHROPIC_JSON_CLEAN" "$FIXTURE_ROOT/anthropic/messages.json"
mv "$ANTHROPIC_SSE_CLEAN" "$FIXTURE_ROOT/anthropic/messages-stream.sse"
mv "$WORK_DIR/README.md" "$FIXTURE_ROOT/README.md"

printf 'Done. Four fixtures plus README written under %s\n' "$FIXTURE_ROOT"
printf 'Review the git diff for account-identifying fields before committing.\n'
