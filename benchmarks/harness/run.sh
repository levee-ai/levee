#!/usr/bin/env bash
# Levee overhead benchmark orchestrator.
#
# Safety first: nothing is measured until the ports are known free, both
# processes are known ready, and levee is known to be the binary this script
# just built. Those checks exist because an orphaned listener from an aborted
# run would otherwise be measured silently in place of the intended process,
# and the resulting number would sit inside every sanity band.
#
# The chain those checks break, in order: a failed k6 threshold exits non-zero,
# set -e aborts this script before any stop step, an unreaped background child
# keeps holding its static port, the next invocation's freshly built levee dies
# asynchronously on EADDRINUSE (cmd/levee/main.go reports the listener error
# through an error channel after startup has already been declared, so the
# process exits well after the shell that launched it moved on), and k6 then
# measures the orphan. The orphan may be a different binary or the other cell's
# config, and its numbers land comfortably inside every sanity band, so no
# downstream analysis can detect the substitution. Only the harness can.
#
# Written for bash 3.2, the system bash on macOS. No associative arrays, no
# nameref locals, no wait -n.
set -euo pipefail

# caffeinate keeps App Nap and sleep from perturbing a long run. The re-exec is
# the very first thing this script does, before any side effect, because exec
# replaces the process image WITHOUT running the EXIT trap. Probed at bash
# 3.2.57: a script that registers an EXIT trap and then execs itself fires that
# trap once, at the end of the second pass, never at the exec. So a re-exec
# placed after the mktemp below would abandon one temporary tree per run with
# nothing left to remove it.
if [ "${LEVEE_BENCH_CAFFEINATED:-}" != "1" ]
then
  exec env LEVEE_BENCH_CAFFEINATED=1 caffeinate -dimsu "$0" "$@"
fi

MOCK_PORT=19099
PROXY_PORT=18080
ADMIN_PORT=19090

REPO_ROOT="$(git rev-parse --show-toplevel)"
BENCH_DIR="${REPO_ROOT}/benchmarks"
FIXTURES_DIR="${REPO_ROOT}/testdata/fixtures"

MODE="${RESULTS_MODE:-quick}"
HARDWARE_TAG="${HARDWARE_TAG:-m3pro-macos}"

# One run-scoped temporary tree holds the built binaries, the rendered configs,
# and the budget state. mktemp -d gives a fresh private directory per run, so
# the state directory starts empty by construction rather than by an rm that
# could be skipped, and no run can inherit a previous run's snapshot. A fixed
# path under a world-writable parent would be squattable and would carry stale
# state between runs, and external reviewers re-run this on shared hosts.
WORK_DIR="$(mktemp -d)"
STATE_DIR="${WORK_DIR}/state"
BIN_DIR="${WORK_DIR}/bin"
mkdir -p "${STATE_DIR}" "${BIN_DIR}"

LEVEE_PID=""
MOCK_PID=""
LEVEE_VERSION=""
GIT_SHA=""
GIT_TREE=""
GIT_DIRTY=""

log() {
  printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

fail() {
  log "FAIL: $*"
  exit 1
}

cleanup() {
  local status=$?
  stop_levee || true
  stop_mock || true
  if [ -n "${WORK_DIR:-}" ] && [ -d "${WORK_DIR}" ]
  then
    rm -rf "${WORK_DIR}"
  fi
  exit "${status}"
}

# The trap covers EXIT, INT, and TERM together so that no abort path can leave
# a listener behind. EXIT alone is not enough on paper and INT and TERM alone
# are not enough either: a set -e abort raises no signal, and a signal delivered
# while a foreground child runs would otherwise skip the stop steps.
trap cleanup EXIT INT TERM

# stop_child signals one background process and then reaps it. Two properties
# matter here and neither is visible at the call site.
#
# First, the signal targets the process GROUP, the negative PID, not just the
# process, so anything the child spawned dies with it instead of surviving to
# hold a port. The negative form is only safe when the child really is its own
# group leader, so the PGID is confirmed to equal the PID first. Without that
# check a non-leader PID would name an unrelated group, and for a child that
# inherited this script's group that group contains this script and its caller.
#
# Second, the reap uses wait, not sleep. The next cell rebinds this exact port
# and only wait proves the previous owner is gone. sleep proves that some
# amount of time passed, which is a different claim: levee's shutdown drains
# both servers under a 10 second context, so any sleep short enough to be worth
# writing can expire first. Rebinding a port whose previous owner still holds
# it is the EADDRINUSE step of the orphan chain described at the top.
stop_child() {
  local pid="$1"
  if [ -z "${pid}" ]
  then
    return 0
  fi
  if ! kill -0 "${pid}" 2>/dev/null
  then
    return 0
  fi

  local pgid
  pgid="$(ps -o pgid= -p "${pid}" 2>/dev/null | tr -d ' ' || true)"
  if [ "${pgid}" = "${pid}" ]
  then
    kill -TERM -"${pid}" 2>/dev/null || true
  else
    log "warning: pid ${pid} is not a process-group leader, signalling the process alone"
    kill -TERM "${pid}" 2>/dev/null || true
  fi
  wait "${pid}" 2>/dev/null || true
}

assert_port_free() {
  local port="$1"
  local occupant
  occupant="$(lsof -nP -iTCP:"${port}" -sTCP:LISTEN -t 2>/dev/null || true)"
  if [ -n "${occupant}" ]
  then
    fail "port ${port} is held by PID ${occupant}, stop it before benchmarking"
  fi
}

# wait_for_http gates on readiness rather than on elapsed time, because a
# process that is merely started is not a process that has bound its listener.
# The optional pid and log arguments make a dead child fail immediately with
# its own diagnostics instead of failing ten seconds later as a readiness
# timeout, which would name the wrong cause. That distinction matters when a
# squatter holds the port: the poll would then succeed against the squatter
# while our own child was already dead.
wait_for_http() {
  local url="$1"
  local what="$2"
  local pid="${3:-}"
  local diag="${4:-}"
  local attempt=0
  while [ "${attempt}" -lt 100 ]
  do
    if [ -n "${pid}" ] && ! kill -0 "${pid}" 2>/dev/null
    then
      if [ -n "${diag}" ] && [ -s "${diag}" ]
      then
        log "last output from ${what}:"
        tail -n 5 "${diag}" >&2
      fi
      fail "${what} exited before it became ready at ${url}"
    fi
    if curl -fsS -o /dev/null "${url}" 2>/dev/null
    then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.1
  done
  fail "${what} never became ready at ${url}"
}

# build_binaries stamps the levee build with this run's short SHA. The stamp is
# the whole point: cmd/levee/main.go declares version as a package variable
# precisely so -ldflags can override it, /health echoes it back, and that pair
# is what makes an orphan distinguishable from the intended process.
#
# This function also resolves the git facts the manifest and the evidence-mode
# gate depend on, so it must run before preflight.
build_binaries() {
  GIT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short HEAD)"
  GIT_TREE="$(git -C "${REPO_ROOT}" rev-parse 'HEAD^{tree}')"
  if [ -n "$(git -C "${REPO_ROOT}" status --porcelain)" ]
  then
    GIT_DIRTY="true"
  else
    GIT_DIRTY="false"
  fi
  LEVEE_VERSION="bench-${GIT_SHA}"

  log "building levee ${LEVEE_VERSION}"
  go build -C "${REPO_ROOT}" -ldflags "-X main.version=${LEVEE_VERSION}" -o "${BIN_DIR}/levee" ./cmd/levee
  go build -C "${REPO_ROOT}" -o "${BIN_DIR}/mockprovider" ./benchmarks/harness/mockprovider
}

preflight() {
  # GIT_DIRTY is resolved by build_binaries. An empty value here means preflight
  # was called first, which would make the evidence-mode clean-tree check below
  # pass silently against an unset variable rather than against the tree.
  if [ -z "${GIT_DIRTY}" ]
  then
    fail "internal error: preflight ran before build_binaries, the tree state is unknown"
  fi

  command -v k6 > /dev/null || fail "k6 is not installed"
  command -v uv > /dev/null || fail "uv is not installed"
  command -v caffeinate > /dev/null || fail "caffeinate is unavailable, this harness targets macOS"

  # 10240 descriptors is a floor, not a setting. An already higher soft limit is
  # left alone: lowering it would be a pointless restriction, and on stock macOS
  # the soft limit is far above this floor, so an unconditional ulimit -n 10240
  # would silently reduce the limit while claiming to raise it.
  local fd_soft
  fd_soft="$(ulimit -Sn)"
  if [ "${fd_soft}" = "unlimited" ] || [ "${fd_soft}" -ge 10240 ]
  then
    log "file descriptor soft limit is ${fd_soft}, already above the 10240 floor"
  else
    ulimit -n 10240 || fail "could not raise the file descriptor limit to 10240 from ${fd_soft}"
  fi

  if [ "${MODE}" = "evidence" ]
  then
    if [ "${GIT_DIRTY}" = "true" ]
    then
      fail "evidence mode requires a clean tree, commit or stash first"
    fi
    if ! pmset -g ps | grep -q 'AC Power'
    then
      fail "evidence mode requires AC power, battery scheduling changes results"
    fi
    if pmset -g | grep -qi 'lowpowermode.*1'
    then
      fail "evidence mode requires Low Power Mode off"
    fi
  fi
}

start_mock() {
  assert_port_free "${MOCK_PORT}"

  # Monitor mode is switched on across the fork only. A background job started
  # under monitor mode becomes its own process-group leader, which is what lets
  # stop_child signal the whole group. It is switched straight back off so this
  # script stays in the terminal's foreground group and keeps receiving Ctrl-C
  # directly instead of handing it to whichever child runs in the foreground.
  set -m
  "${BIN_DIR}/mockprovider" --addr "127.0.0.1:${MOCK_PORT}" --fixtures "${FIXTURES_DIR}" \
    > "${WORK_DIR}/mock.log" 2>&1 &
  MOCK_PID=$!
  set +m

  wait_for_http "http://127.0.0.1:${MOCK_PORT}/healthz" "mock provider" \
    "${MOCK_PID}" "${WORK_DIR}/mock.log"
  assert_mock_fixtures
  log "mock ready on 127.0.0.1:${MOCK_PORT} (pid ${MOCK_PID})"
}

stop_mock() {
  stop_child "${MOCK_PID}"
  MOCK_PID=""
}

# assert_levee_version is the closer on the orphan problem. A stale listener
# from an earlier run reports a different version, so it can never be
# mistaken for the binary this script just built. Readiness alone cannot make
# that distinction, because an orphan answers /health exactly as happily as the
# intended process does.
assert_levee_version() {
  local payload
  payload="$(curl -fsS "http://127.0.0.1:${ADMIN_PORT}/health" 2>/dev/null || true)"

  local reported
  reported="$(printf '%s' "${payload}" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
  if [ -z "${reported}" ]
  then
    fail "levee on port ${ADMIN_PORT} returned no version field, /health said '${payload}'"
  fi
  if [ "${reported}" != "${LEVEE_VERSION}" ]
  then
    fail "levee on port ${ADMIN_PORT} reports version '${reported}' but this run built '${LEVEE_VERSION}', an orphaned process is holding the port"
  fi
}

# start_levee renders a config template into the run-scoped state directory
# and discards levee stdout. The log level is hardcoded INFO and writes one to
# three lines per request, so the destination is part of the measurement and
# is recorded in the manifest.
#
# stdout and stderr are split deliberately. cmd/levee/main.go sends the slog
# handler to stdout and every fatal startup error to stderr, so discarding
# stdout keeps the measured per-request logging cost a write to /dev/null while
# keeping config, snapshot, and flag failures readable. Discarding both would
# turn any startup rejection into a bare readiness timeout with no cause.
start_levee() {
  local config_name="$1"
  local rendered="${WORK_DIR}/${config_name}.yaml"
  local stderr_log="${WORK_DIR}/levee-${config_name}.stderr"

  assert_port_free "${PROXY_PORT}"
  assert_port_free "${ADMIN_PORT}"

  # The templates carry a literal __STATE_DIR__ and their own snapshot
  # filenames, so each config writes to its own file under this run's state
  # directory. Sharing one snapshot file would let one cell restore the other
  # cell's state, and a restored pause answers a whole cell with 403 while a
  # refused old-format snapshot kills the process outright.
  sed "s|__STATE_DIR__|${STATE_DIR}|g" "${BENCH_DIR}/configs/${config_name}.yaml" > "${rendered}"

  set -m
  "${BIN_DIR}/levee" serve --config "${rendered}" > /dev/null 2> "${stderr_log}" &
  LEVEE_PID=$!
  set +m

  wait_for_http "http://127.0.0.1:${ADMIN_PORT}/health" "levee (${config_name})" \
    "${LEVEE_PID}" "${stderr_log}"
  assert_levee_version
  log "levee ready with ${config_name} (pid ${LEVEE_PID})"
}

stop_levee() {
  stop_child "${LEVEE_PID}"
  LEVEE_PID=""
}

# assert_mock_fixtures is the mock's half of the orphan defence. The version
# assert above closes the hole for levee, and the mock had the identical hole
# open: a stale mock from an earlier run holds the static port, the freshly
# started one dies on bind, the readiness poll succeeds on its FIRST try against
# the survivor, and the survivor answers every request in the matrix. A reviewer
# hit exactly that, with an orphan built from a different binary.
#
# Readiness cannot see it, so identity has to. The fixture bytes carry the token
# usage that drives reservation and reconcile, so an orphan loaded from a
# different fixtures tree measures a different code path while every sanity band
# still passes.
#
# The expected digest is recomputed here from the files on disk rather than
# trusted from the process under test, concatenating them in the order
# describeFixtures hashes them (openai json, openai sse, anthropic json,
# anthropic sse). The four byte lengths are asserted alongside it because a
# concatenation digest cannot see a byte moved across a fixture boundary, which
# the mock's own comment names as the blind spot. The resolved directory is
# compared too, and is deliberately NOT recorded in any results file: it is an
# absolute home-directory path, which the identity audit forbids.
assert_mock_fixtures() {
  local payload
  payload="$(curl -fsS "http://127.0.0.1:${MOCK_PORT}/healthz" 2>/dev/null || true)"
  if [ -z "${payload}" ]
  then
    fail "mock on port ${MOCK_PORT} returned nothing from /healthz"
  fi

  local expected_digest
  expected_digest="$(cat \
    "${FIXTURES_DIR}/openai/chat-completion.json" \
    "${FIXTURES_DIR}/openai/chat-completion-stream.sse" \
    "${FIXTURES_DIR}/anthropic/messages.json" \
    "${FIXTURES_DIR}/anthropic/messages-stream.sse" \
    | shasum -a 256 | cut -d' ' -f1)"

  local reported_dir reported_digest
  reported_dir="$(json_string_field "${payload}" fixtures_dir)"
  reported_digest="$(json_string_field "${payload}" fixtures_digest)"

  if [ "${reported_dir}" != "${FIXTURES_DIR}" ]
  then
    fail "mock on port ${MOCK_PORT} serves fixtures from a different directory than this run built against, an orphaned mock is holding the port"
  fi
  if [ -z "${reported_digest}" ]
  then
    fail "mock on port ${MOCK_PORT} reported no fixtures_digest, /healthz said '${payload}'"
  fi
  if [ "${reported_digest}" != "${expected_digest}" ]
  then
    fail "mock on port ${MOCK_PORT} serves fixtures with digest ${reported_digest} but this run's fixtures hash to ${expected_digest}, an orphaned mock is holding the port or the fixtures changed under it"
  fi

  assert_fixture_length "${payload}" openai_json "${FIXTURES_DIR}/openai/chat-completion.json"
  assert_fixture_length "${payload}" openai_sse "${FIXTURES_DIR}/openai/chat-completion-stream.sse"
  assert_fixture_length "${payload}" anthropic_json "${FIXTURES_DIR}/anthropic/messages.json"
  assert_fixture_length "${payload}" anthropic_sse "${FIXTURES_DIR}/anthropic/messages-stream.sse"

  MOCK_FIXTURES_DIGEST="${reported_digest}"
  MOCK_FIXTURE_BYTES="$(json_number_field "${payload}" openai_json)/$(json_number_field "${payload}" openai_sse)/$(json_number_field "${payload}" anthropic_json)/$(json_number_field "${payload}" anthropic_sse)"
  log "mock fixtures verified, digest ${MOCK_FIXTURES_DIGEST}, bytes ${MOCK_FIXTURE_BYTES}"
}

# assert_fixture_length compares one reported length against the real file.
assert_fixture_length() {
  local payload="$1"
  local field="$2"
  local path="$3"

  local reported actual
  reported="$(json_number_field "${payload}" "${field}")"
  actual="$(wc -c < "${path}" | tr -d ' ')"
  if [ "${reported}" != "${actual}" ]
  then
    fail "mock reports ${field} at ${reported} bytes but the file on disk is ${actual} bytes, the mock is serving different fixture content"
  fi
}

# json_string_field and json_number_field read one flat field out of a compact
# JSON object. Both endpoints in play are written by encoding/json with no
# indentation and no nesting deeper than one level, and neither value can
# contain an escaped quote, so a sed extraction is honest here and keeps the
# harness free of a JSON dependency.
json_string_field() {
  printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"
}

json_number_field() {
  printf '%s' "$1" | sed -n "s/.*\"$2\":\([0-9]*\).*/\1/p"
}

# Measurement globals. RESULTS_DIR and ATTEMPTS_FILE are resolved in main, the
# rest in configure_mode, and they are declared here so a future call-order
# mistake trips set -u at the point of use instead of writing artifacts into an
# empty path. ENFORCE_MAX_VUS is deliberately absent from this block: declaring
# it would run at load time and erase an environment override, and overriding it
# is how the concurrency-cap gate gets exercised.
RESULTS_DIR=""
ATTEMPTS_FILE=""
MOCK_FIXTURES_DIGEST=""
MOCK_FIXTURE_BYTES=""
RATE_NONSTREAM=""
RATE_STREAM=""
WARMUP_DURATION=""
STEADY_START=""
STEADY_DURATION=""
REPETITIONS=""
STREAM_REPETITIONS=""
PROMPT_SIZES=""

# run_cell runs one k6 invocation and writes its raw CSV plus summary into the
# results directory. Every latency artifact in the results tree comes from
# here, so the filtering is code, never a manual step.
#
# k6 writes its progress and its handleSummary stdout line to stdout, which is
# redirected to stderr here. This script's stdout carries exactly one thing, the
# results directory path, so a caller can capture it.
run_cell() {
  local cell="$1"
  local target="$2"
  local stream="$3"
  local prompt_bytes="$4"
  local rate="$5"
  local max_vus="$6"

  local raw="${WORK_DIR}/${cell}.raw.csv.gz"
  local filtered="${RESULTS_DIR}/${cell}.duration.csv"
  local summary="${RESULTS_DIR}/${cell}.summary.json"

  local steady_duration="${STEADY_DURATION}"
  local warmup_duration="${WARMUP_DURATION}"

  # The preallocated pool follows the ceiling rather than sitting at a fixed 50.
  # k6 validates preAllocatedVUs against maxVUs before it starts and exits 104
  # with "maxVUs can't be less than preAllocatedVUs", so a fixed floor of 50
  # makes every enforce cell unrunnable, since those pin the ceiling to 40 to
  # stay under levee's per-agent admission slot count. An enforce cell is
  # therefore fully preallocated, which is the better measurement anyway,
  # because a pool that grows mid-run initializes VUs inside the steady window.
  local preallocated_vus=50
  if [ "${max_vus}" -lt "${preallocated_vus}" ]
  then
    preallocated_vus="${max_vus}"
  fi

  log "cell ${cell}: rate ${rate}, stream ${stream}, prompt ${prompt_bytes}B, vus ${preallocated_vus} to ${max_vus}"
  record_machine_state "${cell}" "before"

  local k6_status=0
  K6_NO_USAGE_REPORT=true \
  K6_CSV_TIME_FORMAT=unix_micro \
  TARGET_URL="${target}" \
  SUMMARY_PATH="${summary}" \
  CELL="${cell}" \
  RATE="${rate}" \
  PREALLOCATED_VUS="${preallocated_vus}" \
  MAX_VUS="${max_vus}" \
  WARMUP_DURATION="${warmup_duration}" \
  STEADY_START="${STEADY_START}" \
  STEADY_DURATION="${steady_duration}" \
  PROMPT_BYTES="${prompt_bytes}" \
  STREAM="${stream}" \
    k6 run --out "csv=${raw}" "${BENCH_DIR}/k6/overhead.js" >&2 || k6_status=$?

  record_machine_state "${cell}" "after"
  printf '%s k6_exit=%s\n' "${cell}" "${k6_status}" >> "${RESULTS_DIR}/k6-exit-codes.txt"
  record_dropped_iterations "${cell}" "${summary}"

  if [ "${k6_status}" -ne 0 ]
  then
    printf 'cell %s failed an integrity threshold, k6 exit %s\n' "${cell}" "${k6_status}" \
      >> "${ATTEMPTS_FILE}"
    fail "cell ${cell} failed an integrity threshold (k6 exit ${k6_status}), this run is invalid"
  fi

  filter_cell_csv "${raw}" "${filtered}" "${cell}"
}

# filter_cell_csv keeps only steady-scenario latency rows. Warmup rows are
# excluded by the scenario tag rather than by timestamp arithmetic, since k6
# phase timings are wall-clock based and the default CSV timestamp resolution
# is one second. Row counts before and after are recorded so the derivation
# from the discarded raw file stays checkable.
#
# The scenario column index is read out of the header rather than hardcoded, and
# matched whole rather than as a substring anywhere in the row. Substring
# matching would silently keep a warmup row whose URL or tag happened to carry
# the word, and a k6 column reorder would then be invisible instead of emptying
# the output and tripping the row-count check below.
filter_cell_csv() {
  local raw="$1"
  local out="$2"
  local cell="$3"

  local before after
  before="$(gunzip -c "${raw}" | wc -l | tr -d ' ')"

  gunzip -c "${raw}" | awk -F, '
    NR == 1 {
      for (column = 1; column <= NF; column++)
      {
        if ($column == "metric_name") { metric_column = column }
        if ($column == "scenario") { scenario_column = column }
      }
      if (metric_column == 0 || scenario_column == 0)
      {
        exit 3
      }
      print
      next
    }
    $scenario_column == "steady" && ($metric_column == "http_req_duration" || $metric_column == "http_req_waiting")
  ' > "${out}" || fail "cell ${cell} CSV header lacks the metric_name or scenario column, the k6 CSV format changed"

  after="$(wc -l < "${out}" | tr -d ' ')"
  printf '%s raw_rows=%s filtered_rows=%s\n' "${cell}" "${before}" "${after}" \
    >> "${RESULTS_DIR}/row-counts.txt"

  if [ "${after}" -lt 2 ]
  then
    fail "cell ${cell} produced no steady-scenario rows, the scenario tag or CSV format changed"
  fi
  gzip -9 "${out}"
}

# record_dropped_iterations pulls the per-scenario drop counts out of the cell
# summary into one file per run, so the whole matrix can be read at a glance.
#
# Only steady drops gate a run. Warmup drops are expected and are recorded
# anyway: a cell that starts dropping heavily in warmup is saying something about
# cold start or host load, and the enforce cells legitimately drop tens of
# iterations there while levee builds its tokenizer on the first request. A
# warmup count that climbs across runs is a signal, not noise to discard.
#
# This runs even for a cell that failed its threshold, which is deliberate: the
# counts are how a reader of the aborted directory sees WHERE the drops landed.
record_dropped_iterations() {
  local cell="$1"
  local summary="$2"
  if [ ! -s "${summary}" ]
  then
    printf '%s steady=unknown warmup=unknown, k6 wrote no summary\n' "${cell}" \
      >> "${RESULTS_DIR}/dropped-iterations.txt"
    return 0
  fi
  local steady warmup
  steady="$(json_number_field "$(tr -d '\n ' < "${summary}")" dropped_iterations_steady)"
  warmup="$(json_number_field "$(tr -d '\n ' < "${summary}")" dropped_iterations_warmup)"
  printf '%s steady=%s warmup=%s\n' "${cell}" "${steady:-absent}" "${warmup:-absent}" \
    >> "${RESULTS_DIR}/dropped-iterations.txt"
}

# record_machine_state captures the environment conditions that bound the
# drift story. A cell polluted by a background spike or a power-source change
# is otherwise indistinguishable from a real regression.
#
# The thermal reading is a placeholder when pmset has nothing to report, which
# is the normal case on Apple Silicon: pmset -g therm answers "No CPU power
# status has been recorded" and never prints the CPU_Speed_Limit line that Intel
# hosts do. An empty value would read as a broken capture rather than an absent
# one.
record_machine_state() {
  local cell="$1"
  local phase="$2"
  local thermal
  thermal="$(pmset -g therm 2>/dev/null | sed -n 's/.*CPU_Speed_Limit *= *\([0-9]*\).*/\1/p' | head -1)"
  {
    printf 'cell=%s phase=%s ' "${cell}" "${phase}"
    printf 'loadavg=%s ' "$(sysctl -n vm.loadavg | tr -d '{}' | tr -s ' ' '_')"
    printf 'thermal=%s ' "${thermal:-none-reported}"
    printf 'power=%s ' "$(pmset -g ps | head -1 | tr ' ' '_')"
    printf 'timewait=%s\n' "$(count_timewait)"
  } >> "${RESULTS_DIR}/machine-state.txt"
}

# count_timewait always prints one integer and always succeeds. grep -c cannot
# be used here: it prints 0 AND exits 1 when nothing matches, so the obvious
# "grep -c TIME_WAIT || echo 0" yields the two-line value "0\n0", and every
# arithmetic test against it then fails with "integer expression expected".
# Proven at bash 3.2.57, and it silently takes the wrong branch rather than
# aborting because the test sits inside an if.
count_timewait() {
  netstat -an 2>/dev/null | awk '$0 ~ /TIME_WAIT/ { total++ } END { print total + 0 }'
}

# wait_for_timewait_drain keeps one cell's connection churn from bleeding into
# the next. The levee-to-mock leg uses the standard library default of two
# idle connections per host, so a burst of dials leaves TIME_WAIT entries that
# would otherwise raise the following cell's tail.
wait_for_timewait_drain() {
  local attempt=0
  while [ "${attempt}" -lt 60 ]
  do
    local count
    count="$(count_timewait)"
    if [ "${count}" -lt 2000 ]
    then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  log "warning: TIME_WAIT stayed high, recording it and continuing"
}

# The matrix. Cells share one mock boot so they are directly comparable.
# Passthrough and enforce run back-to-back as a pair, and the pair repeats,
# because the enforcement signal is smaller than plausible drift between
# cells run minutes apart. A direct cell opens and closes the matrix as a
# drift canary.
#
# Direct payload cells exist so every proxied number has a baseline at its OWN
# payload size. The overhead figure publishes a quantile SHIFT, proxied minus
# direct at the same payload size and the same rate, so a payload size with no
# direct cell has no computable shift and the figure prints a SKIPPED row for it
# instead of a number.
#
# AMENDED 2026-09-16. The design pre-registered direct cells at the small payload
# and at 32768B only, on the reasoning that two sizes are enough to prove the
# generator and the mock are payload-insensitive. That is true of the
# insensitivity claim and false of the figure: evidence mode runs the enforce and
# passthrough arms at 150B, 4096B and 32768B, so the pre-registered set left the
# middle size with no baseline and made one of the three published rows a SKIPPED
# row. The 4096B direct cell below closes that hole. It is the size the design
# calls out as the crossover, where the tokenizer curve carries the enforcement
# path past the 500us tenet, so it is the row a reader is most likely to want.
#
# Cost is one more cell in both modes, 9 rather than 8 in quick mode and 43
# rather than 42 in evidence mode. A cell is one k6 invocation of steady_start
# plus steady_duration, so it adds roughly 35 seconds in quick mode and roughly
# 75 seconds in evidence mode. Direct cells take no TIME_WAIT drain wait after
# them, only proxied cells do.
run_matrix() {
  local direct_target="http://127.0.0.1:${MOCK_PORT}/v1/chat/completions"
  local proxy_target="http://127.0.0.1:${PROXY_PORT}/openai/v1/chat/completions"

  run_cell "direct-canary-open-nonstream-150" "${direct_target}" false 150 "${RATE_NONSTREAM}" 100

  local repetition=1
  while [ "${repetition}" -le "${REPETITIONS}" ]
  do
    local bytes
    for bytes in ${PROMPT_SIZES}
    do
      start_levee passthrough
      run_cell "passthrough-nonstream-${bytes}-r${repetition}" "${proxy_target}" false "${bytes}" "${RATE_NONSTREAM}" 100
      stop_levee
      wait_for_timewait_drain

      start_levee enforce
      # maxVUs stays strictly below the hardcoded 50-slot per-agent
      # concurrency cap, so a tail pileup shows up as dropped iterations
      # rather than as a 429 that also pollutes levee-side state.
      run_cell "enforce-nonstream-${bytes}-r${repetition}" "${proxy_target}" false "${bytes}" "${RATE_NONSTREAM}" "${ENFORCE_MAX_VUS}"
      stop_levee
      wait_for_timewait_drain
    done
    repetition=$((repetition + 1))
  done

  local stream_repetition=1
  while [ "${stream_repetition}" -le "${STREAM_REPETITIONS}" ]
  do
    run_cell "direct-stream-150-r${stream_repetition}" "${direct_target}" true 150 "${RATE_STREAM}" 100

    start_levee passthrough
    run_cell "passthrough-stream-150-r${stream_repetition}" "${proxy_target}" true 150 "${RATE_STREAM}" 100
    stop_levee
    wait_for_timewait_drain

    start_levee enforce
    run_cell "enforce-stream-150-r${stream_repetition}" "${proxy_target}" true 150 "${RATE_STREAM}" "${ENFORCE_MAX_VUS}"
    stop_levee
    wait_for_timewait_drain

    stream_repetition=$((stream_repetition + 1))
  done

  run_cell "direct-payload-4096" "${direct_target}" false 4096 "${RATE_NONSTREAM}" 100
  run_cell "direct-payload-32768" "${direct_target}" false 32768 "${RATE_NONSTREAM}" 100
  run_cell "direct-canary-close-nonstream-150" "${direct_target}" false 150 "${RATE_NONSTREAM}" 100
}

# configure_mode sets every knob that differs between a fast local check and a
# publishable run.
#
# ENFORCE_MAX_VUS is a variable rather than a literal so the concurrency-cap
# gate can be exercised deliberately. internal/budget hardcodes 50 admission
# slots per agent, a slot is held until the deferred reconcile runs after the
# response bytes have already reached k6, and 40 keeps the VU pool strictly
# below the cap so a tail pileup surfaces as dropped iterations rather than as
# a 429 that also leaves levee-side state altered.
configure_mode() {
  RATE_NONSTREAM=500
  RATE_STREAM=250
  WARMUP_DURATION=10s
  STEADY_START=12s
  ENFORCE_MAX_VUS="${ENFORCE_MAX_VUS:-40}"
  case "${MODE}" in
    quick)
      STEADY_DURATION=20s
      REPETITIONS=1
      STREAM_REPETITIONS=1
      PROMPT_SIZES="150"
      ;;
    evidence)
      STEADY_DURATION=60s
      REPETITIONS=5
      STREAM_REPETITIONS=3
      PROMPT_SIZES="150 4096 32768"
      ;;
    *)
      fail "unknown RESULTS_MODE '${MODE}', use quick or evidence"
      ;;
  esac
}

# capture_microbench records the component costs the enforcement figure
# annotates. They are measured on THIS host during THIS run, so the figure
# never carries stale constants from another machine.
#
# The logcost package is in the list for exactly that reason. Structured logging
# is a real component of the enforce minus passthrough delta, since an enforced
# request writes three slog lines where a passthrough request writes one, and it
# is the one component no benchmark under internal/ measures. Leaving it out
# would put a hardcoded microsecond figure in the enforcement annotation, which
# is the failure this whole file exists to prevent. Its benchmark names all end
# in Line or RequestLines, so the pattern picks up the three call-site shapes and
# both composites without matching anything else.
capture_microbench() {
  log "capturing component micro-benchmarks"
  {
    printf 'go_version=%s\n' "$(go version)"
    go test -C "${REPO_ROOT}" \
      -bench='Estimate|ReserveReconcile|ReadRequestBody|Line$|RequestLines$' \
      -benchmem -run='^$' \
      ./internal/tokens/ ./internal/budget/ ./internal/proxy/ \
      ./benchmarks/harness/logcost/ 2>&1 \
      | grep -E '^(Benchmark|ok|PASS|goos|goarch|pkg|cpu)'
  } > "${RESULTS_DIR}/microbench.txt"
}

# check_bands evaluates the pre-registered sanity bands mechanically. They are
# gates, not prose: a human reading figures would be exactly the failure mode
# pre-registration exists to prevent.
check_bands() {
  log "evaluating pre-registered sanity bands"
  if ! uv run --script "${BENCH_DIR}/plots/check_bands.py" "${RESULTS_DIR}" \
    > "${RESULTS_DIR}/bands.txt" 2>&1
  then
    cat "${RESULTS_DIR}/bands.txt" >&2
    printf 'sanity bands violated, see bands.txt\n' >> "${ATTEMPTS_FILE}"
    fail "sanity bands violated, see ${RESULTS_DIR}/bands.txt, this run is NOT publishable"
  fi
  cat "${RESULTS_DIR}/bands.txt" >&2
}

# audit_results refuses to declare a run committable while it still contains
# anything that identifies the operator or their machine. Compressed
# artifacts are decompressed first, otherwise they would evade both this and
# the repository's own content rules.
#
# The staged manifest is audited here rather than in the results directory,
# because the manifest is written last on purpose and is also the artifact most
# likely to carry a path. Auditing the directory only, as the surrounding order
# would otherwise imply, would exempt exactly the file whose every field is a
# captured command's output.
#
# The username comes from id -un, not from $USER: an unset or empty $USER would
# turn the alternation into an empty branch that matches every file, which fails
# safe but reports a cause that has nothing to do with the artifacts.
audit_results() {
  log "auditing results for identifying content"
  local scratch="${WORK_DIR}/audit"
  rm -rf "${scratch}"
  mkdir -p "${scratch}"
  cp -R "${RESULTS_DIR}/." "${scratch}/"
  if [ -f "${WORK_DIR}/MANIFEST" ]
  then
    cp "${WORK_DIR}/MANIFEST" "${scratch}/MANIFEST"
  fi
  find "${scratch}" -name '*.gz' -exec gunzip -f {} +

  local username hostname_short
  username="$(id -un)"
  hostname_short="$(hostname -s)"
  if [ -z "${username}" ] || [ -z "${hostname_short}" ]
  then
    fail "could not resolve the username or hostname to audit against"
  fi

  local pattern="${username}|${hostname_short}|/Users/|serial|Serial"
  if grep -rEl "${pattern}" "${scratch}" > /dev/null 2>&1
  then
    # -n rather than -ln, because -l suppresses the line numbers -n asks for and
    # a bare file list does not say WHICH field leaked. Capped because a CSV
    # column that matched would otherwise print tens of thousands of lines.
    grep -rEn "${pattern}" "${scratch}" 2>/dev/null | head -20 >&2 || true
    fail "results contain identifying content, fix the manifest commands before committing"
  fi
  log "identity audit clean"
}

# stage_manifest writes the manifest into the work directory so audit_results
# can inspect it, and install_manifest moves it into place afterwards. The
# manifest still lands LAST: a directory without one is visibly an aborted run
# and can never be mistaken for evidence.
stage_manifest() {
  {
    printf 'run_mode=%s\n' "${MODE}"
    printf 'hardware_tag=%s\n' "${HARDWARE_TAG}"
    printf 'levee_git_sha=%s\n' "${GIT_SHA}"
    printf 'levee_git_tree=%s\n' "${GIT_TREE}"
    printf 'levee_tree_dirty=%s\n' "${GIT_DIRTY}"
    printf 'levee_version_stamp=%s\n' "${LEVEE_VERSION}"
    printf 'go_version=%s\n' "$(go version)"
    printf 'k6_version=%s\n' "$(k6 version)"
    printf 'uv_version=%s\n' "$(uv --version)"
    printf 'os_version=%s\n' "$(sw_vers -productVersion)"
    printf 'cpu=%s\n' "$(sysctl -n machdep.cpu.brand_string)"
    printf 'cpu_cores=%s\n' "$(sysctl -n hw.ncpu)"
    printf 'memory_bytes=%s\n' "$(sysctl -n hw.memsize)"
    printf 'somaxconn=%s\n' "$(sysctl -n kern.ipc.somaxconn)"
    printf 'tcp_msl_ms=%s\n' "$(sysctl -n net.inet.tcp.msl)"
    printf 'ephemeral_port_first=%s\n' "$(sysctl -n net.inet.ip.portrange.first)"
    printf 'ephemeral_port_last=%s\n' "$(sysctl -n net.inet.ip.portrange.last)"
    printf 'ulimit_nofile=%s\n' "$(ulimit -n)"
    # go env has no GOMAXPROCS key, it answers with an empty line, so the
    # honest record is the environment variable levee would read plus the fact
    # that an unset one leaves the Go runtime defaulting to hw.ncpu, recorded
    # above as cpu_cores.
    printf 'gomaxprocs=%s\n' "${GOMAXPROCS:-unset-runtime-defaults-to-cpu-cores}"
    printf 'gogc=%s\n' "${GOGC:-default}"
    printf 'rate_nonstreaming_rps=%s\n' "${RATE_NONSTREAM}"
    printf 'rate_streaming_rps=%s\n' "${RATE_STREAM}"
    printf 'warmup_duration=%s\n' "${WARMUP_DURATION}"
    printf 'steady_start=%s\n' "${STEADY_START}"
    printf 'steady_duration=%s\n' "${STEADY_DURATION}"
    printf 'repetitions=%s\n' "${REPETITIONS}"
    printf 'stream_repetitions=%s\n' "${STREAM_REPETITIONS}"
    printf 'prompt_sizes_bytes=%s\n' "${PROMPT_SIZES}"
    printf 'enforce_max_vus=%s\n' "${ENFORCE_MAX_VUS}"
    printf 'upstream_scheme=http-loopback\n'
    printf 'levee_log_destination=/dev/null\n'
    printf 'k6_usage_report=disabled\n'
    printf 'state_dir=run-scoped-mktemp\n'
    # Levee is restarted per proxied cell against a run-scoped state directory
    # that mktemp created empty, and each config owns its own snapshot path with
    # a 5 minute interval that no cell is long enough to reach. So every cell
    # starts from fresh state rather than a restored snapshot. That is asserted
    # rather than claimed: a snapshot written at any point during the matrix
    # would leave a file behind and make this count non-zero.
    printf 'snapshot_files_written=%s\n' "$(find "${STATE_DIR}" -type f | wc -l | tr -d ' ')"
    printf 'snapshot_state_per_cell=fresh\n'
    printf 'mock_fixtures_digest=%s\n' "${MOCK_FIXTURES_DIGEST}"
    printf 'mock_fixture_bytes_openai_json_sse_anthropic_json_sse=%s\n' "${MOCK_FIXTURE_BYTES}"
    printf 'fixture_sha256:\n'
    # Sorted so two runs on the same tree produce byte-identical manifest
    # blocks. find walks directories in filesystem order, which differs between
    # machines and makes an otherwise clean evidence diff noisy.
    find "${FIXTURES_DIR}" -type f \( -name '*.json' -o -name '*.sse' \) -exec shasum -a 256 {} + \
      | sed "s|${FIXTURES_DIR}/||" | sort
  } > "${WORK_DIR}/MANIFEST"
}

install_manifest() {
  mv "${WORK_DIR}/MANIFEST" "${RESULTS_DIR}/MANIFEST"
  log "manifest written, run complete"
}

main() {
  build_binaries
  preflight
  configure_mode

  local run_ordinal=1
  local dirty_suffix=""
  if [ "${GIT_DIRTY}" = "true" ]
  then
    dirty_suffix="-dirty"
  fi
  local base="${BENCH_DIR}/results/$(date -u '+%Y-%m-%d')-${GIT_SHA}${dirty_suffix}-${HARDWARE_TAG}-${MODE}"
  while [ -d "${base}-r${run_ordinal}" ]
  do
    run_ordinal=$((run_ordinal + 1))
  done
  RESULTS_DIR="${base}-r${run_ordinal}"
  mkdir -p "${RESULTS_DIR}"
  ATTEMPTS_FILE="${RESULTS_DIR}/attempts.txt"
  printf 'attempt 1 started %s mode %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "${MODE}" > "${ATTEMPTS_FILE}"
  log "results directory ${RESULTS_DIR}"

  start_mock
  run_matrix
  stop_mock

  capture_microbench
  check_bands
  stage_manifest
  audit_results
  install_manifest
  printf 'attempt 1 completed successfully %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> "${ATTEMPTS_FILE}"
  printf '%s\n' "${RESULTS_DIR}"
}

main "$@"
