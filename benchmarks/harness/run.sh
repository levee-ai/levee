#!/usr/bin/env bash
# Levee overhead benchmark orchestrator. It builds levee and a fixture-serving
# mock provider, runs a matrix of k6 cells against them, and writes one results
# directory per run.
#
# Two modes:
#
#   RESULTS_MODE=quick benchmarks/harness/run.sh       13 cells, disposable
#   RESULTS_MODE=evidence benchmarks/harness/run.sh    53 cells, publishable
#
# Quick is the default. Its output is gitignored, so it can never be mistaken for
# evidence, and evidence mode is the only mode whose directory is committable.
#
# It refuses to start, or refuses partway, on any of these:
#
#   - k6, uv or caffeinate missing. The host quiescence and power checks are
#     macOS-only, so this harness targets macOS.
#   - MOCK_PORT, PROXY_PORT or ADMIN_PORT already held by a listener.
#   - LEVEE_BENCH_SYNTHETIC_IDLE_READINGS set, in either mode.
#   - BENCH_MAX_VUS at 50 or above.
#   - PROMPT_SIZES overridden in evidence mode.
#   - Evidence mode only: a dirty tree, battery power, Low Power Mode on, or a
#     host CPU idle reading below HOST_IDLE_FLOOR_PERCENT.
#   - Any cell that fails a k6 integrity threshold, or a sanity band in
#     check_bands.py.
#
# What to do when one of those fires: benchmarks/methodology/triage.md.
#
# Nothing is measured until the ports are known free, both processes are known
# ready, and levee is known to be the binary this script just built. An aborted run
# can leave a child holding a static port, the next run's levee then dies
# asynchronously on EADDRINUSE, and k6 measures the survivor, whose numbers land
# inside every sanity band. Only the harness can catch that substitution.
#
# Written for bash 3.2, the system bash on macOS. No associative arrays, no
# nameref locals, no wait -n.
set -euo pipefail

# LEVEE_BENCH_SOURCED says whether this file is being sourced rather than
# executed. A driver sources it to drive the host quiescence functions against
# scripted readings. It is derived from BASH_SOURCE rather than from an
# environment variable, which cannot be exported into a real run by accident.
# Assigning a variable is not a side effect, so this stays above the re-exec.
if [ "${BASH_SOURCE[0]}" = "$0" ]
then
  LEVEE_BENCH_SOURCED=no
else
  LEVEE_BENCH_SOURCED=yes
fi

# caffeinate keeps App Nap and sleep from perturbing a long run. The re-exec is
# the first thing this script does, before any side effect, because exec replaces
# the process image without running the EXIT trap. Probed at bash 3.2.57: a
# script that registers an EXIT trap and then execs itself fires that trap once,
# at the end of the second pass, never at the exec. A re-exec placed after the
# mktemp below would abandon one temporary tree per run.
#
# A sourced copy must not re-exec, because exec would replace the sourcing
# script's process image.
if [ "${LEVEE_BENCH_SOURCED}" = "no" ] && [ "${LEVEE_BENCH_CAFFEINATED:-}" != "1" ]
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
# and the budget state. mktemp -d starts the state directory empty by
# construction rather than by an rm that could be skipped, so no run can inherit
# a previous run's snapshot, and nothing under a world-writable parent is
# squattable.
WORK_DIR="$(mktemp -d)"
STATE_DIR="${WORK_DIR}/state"
BIN_DIR="${WORK_DIR}/bin"
mkdir -p "${STATE_DIR}" "${BIN_DIR}"

# The read position for the scripted-reading hook, see sample_cpu_idle_percent.
# It is a file rather than a shell variable because every caller reads the
# sampler through a $( ) subshell, and a counter incremented inside a subshell
# does not survive the return.
SYNTHETIC_IDLE_POSITION_FILE="${WORK_DIR}/synthetic-idle-read-position"

LEVEE_PID=""
MOCK_PID=""
LEVEE_VERSION=""
GIT_SHA=""
GIT_TREE=""
GIT_DIRTY=""

# Everything the matrix is tuned by, in one block, because reading order in the
# rest of this file does not match execution order: preflight calls the host
# quiescence functions defined far below it. Every derivation, calibration table
# and frozen cut is in benchmarks/methodology/calibration.md, and the dated
# history of why each value moved is in benchmarks/CHANGELOG.md.

# The first two are resolved in main and the rest in configure_mode. Declared here
# so a call-order mistake trips set -u at the point of use rather than writing
# artifacts into an empty path.
RESULTS_DIR=""
ATTEMPTS_FILE=""
CONTENDED_CELLS_FILE=""
MOCK_FIXTURES_DIGEST=""
MOCK_FIXTURE_BYTES=""
RATE_STREAM=""
WARMUP_SECONDS=""
STEADY_START_SECONDS=""
STEADY_SECONDS=""
WARMUP_DURATION=""
STEADY_START=""
STEADY_DURATION=""
REPETITIONS=""
STREAM_REPETITIONS=""

# BENCH_MAX_VUS and PROMPT_SIZES are deliberately absent from the group above, and
# are resolved in configure_mode. Declaring either here would run at load time and
# erase an environment override, and overriding both is how the concurrency cap
# and a single payload size get exercised.

# Counters rather than resolved settings, so they start at their zero value rather
# than empty: both mid-run rules read them on the first reading. They are plain
# globals, which is safe only because the chain that mutates them carries no
# subshell and no pipeline, main to run_matrix to run_cell to record_machine_state
# to evaluate_mid_run_quiescence. Wrapping run_cell in $( ) would break it.
MID_RUN_READING_COUNT=0
MID_RUN_BREACH_COUNT=0
MID_RUN_CONSECUTIVE_BREACHES=0
MID_RUN_CONSECUTIVE_BREACH_LABELS=""

# How far below its demanded arrival rate a cell may land before its latency is
# queue residence rather than service time. The legitimate envelope is about 0.02
# percent and the saturation failure this catches was 51 percent.
RATE_SHORTFALL_TOLERANCE_PERCENT=2

# The two integrity tolerances, each a fraction of the cell's own demand carried in
# basis points, applied as demanded times points over 10000 with integer
# truncation and then raised to the floor. They replaced an absolute count==0 and
# rate==0, which across 1,224,053 steady iterations per evidence run was a lottery
# rather than a quality bar.
#
# 100 basis points is 1 percent of demanded steady iterations, against a measured
# benign envelope of 0.490 percent, and the floor of 25 keeps one host stall,
# observed at 13 dropped iterations, from failing a small window. The
# failed-request pair is 20 times tighter in relative terms because a dropped
# iteration is the load generator giving up while a failed request is levee
# answering 429 or erroring, and every failure shape worth catching there is
# sustained rather than singular.
STEADY_DROP_TOLERANCE_BASIS_POINTS=100
STEADY_DROP_TOLERANCE_FLOOR=25
STEADY_FAILED_TOLERANCE_BASIS_POINTS=5
STEADY_FAILED_TOLERANCE_FLOOR=5

# The host quiescence floor, in percent CPU idle. It sits above every one of the 26
# contended calibration readings and below only the lowest 2 of the 26 ambient
# ones. It resolves the contended regime it was calibrated against, worth about 16
# points of idle, and cannot resolve a milder one. loadavg is recorded beside it in
# machine-state.txt and is deliberately not a gate, because the invalidated run
# and the quiet re-measurements overlap on loadavg.
HOST_IDLE_FLOOR_PERCENT=60

# Two adjacent confirmed breaches fail the run. The before and after readings of one
# cell are adjacent, so a cell contended start to finish trips this at the cell that
# caused it, and contention held across a run trips it in the first two minutes
# rather than at minute 52.
CONSECUTIVE_BREACH_LIMIT=2

# A host can be contended throughout without ever landing two breaches side by
# side, so this caps the fraction of all mid-run readings that may breach, checked
# once at the end of the matrix. It sits 7.7 times above the observed quiet rate
# and roughly half the one observed pervasive-contention rate.
PERVASIVE_BREACH_FRACTION_PERCENT=10

# The payload the A/A control pair runs at. The control states the estimator's
# noise floor beside the measurement whose signal is closest to it. At 4096B the
# signal is 160 times the floor and needs no such statement.
AA_CONTROL_PAYLOAD_BYTES=150

# The payload check_bands.py runs the primary enforcement gate at. Named here
# because quick mode has to include it in PROMPT_SIZES or that gate has no cells to
# read, see configure_mode.
ENFORCEMENT_GATE_PAYLOAD_BYTES=4096

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

# EXIT, INT and TERM together, because a set -e abort raises no signal while a
# signal delivered during a foreground child would otherwise skip the stop steps.
trap cleanup EXIT INT TERM

# stop_child signals one background process and then reaps it. Two properties
# matter and neither is visible at the call site.
#
# The signal targets the process group, the negative PID, so anything the child
# spawned dies with it instead of surviving to hold a port. That form is only
# safe when the child really is its own group leader, so the PGID is confirmed to
# equal the PID first. Without that check a non-leader PID names an unrelated
# group, and for a child that inherited this script's group that group contains
# this script and its caller.
#
# The reap uses wait, not sleep. The next cell rebinds this exact port and only
# wait proves the previous owner is gone. levee's shutdown drains both servers
# under a 10 second context, so any sleep short enough to be worth writing can
# expire first, and rebinding a port whose previous owner still holds it is the
# EADDRINUSE step of the orphan chain.
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

# wait_for_http gates on readiness rather than on elapsed time, because a process
# that is merely started has not necessarily bound its listener. The optional pid
# and log arguments make a dead child fail immediately with its own diagnostics
# instead of failing ten seconds later as a readiness timeout, which would name
# the wrong cause. That matters when a squatter holds the port: the poll would
# succeed against the squatter while our own child was already dead.
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

# cmd/levee/main.go declares version as a package variable precisely so -ldflags
# can override it, /health echoes it back, and that pair is what makes an orphan
# distinguishable from the intended process. This also resolves the git facts the
# manifest and the evidence-mode gate depend on, so it must run before preflight.
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

  # The scripted-reading hook is refused here, in every mode, because preflight
  # sits on the only path that creates a results directory.
  if [ -n "${LEVEE_BENCH_SYNTHETIC_IDLE_READINGS:-}" ]
  then
    fail "LEVEE_BENCH_SYNTHETIC_IDLE_READINGS is set, which replaces the host CPU idle sensor with a scripted list of values. That hook is for the sourced verification driver only, and a real run refuses to start while it is set rather than measuring against fabricated host readings. Unset it and start again"
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

  # The startup quiescence gate stays a hard refusal on one reading while the
  # mid-run rules require a sustained breach. The asymmetry is deliberate:
  # refusing here costs five seconds, refusing at cell 40 costs the 52 minutes
  # already spent. One sample is taken even in quick mode, so an operator learns
  # what their machine reads before attempting an evidence run.
  local startup_idle
  startup_idle="$(robust_cpu_idle_percent)"
  log "host CPU idle at startup is ${startup_idle} percent, floor is ${HOST_IDLE_FLOOR_PERCENT}"
  enforce_startup_quiescence "${startup_idle}"
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

# assert_levee_version closes the orphan problem for levee. A stale listener from
# an earlier run reports a different version stamp. Readiness alone cannot make
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

# The log level is hardcoded INFO and writes one to three lines per request, so
# the destination is part of the measurement and is recorded in the manifest.
#
# stdout and stderr are split deliberately. cmd/levee/main.go sends the slog
# handler to stdout and every fatal startup error to stderr, so discarding stdout
# keeps the measured per-request logging cost a write to /dev/null while keeping
# config, snapshot and flag failures readable. Discarding both would turn any
# startup rejection into a bare readiness timeout with no cause.
start_levee() {
  local config_name="$1"
  local rendered="${WORK_DIR}/${config_name}.yaml"
  local stderr_log="${WORK_DIR}/levee-${config_name}.stderr"

  assert_port_free "${PROXY_PORT}"
  assert_port_free "${ADMIN_PORT}"

  # The templates carry a literal __STATE_DIR__ and their own snapshot filenames,
  # so each config writes to its own file under this run's state directory.
  # Sharing one snapshot file would let one cell restore the other cell's state,
  # and a restored pause answers a whole cell with 403 while a refused
  # old-format snapshot kills the process outright.
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

# The mock's half of the orphan defence. The fixture bytes carry the token usage
# that drives reservation and reconcile, so a survivor loaded from a different tree
# measures a different code path while every sanity band still passes.
#
# The expected digest is recomputed from the files on disk rather than trusted from
# the process under test, concatenating them in the order describeFixtures hashes
# them: openai json, openai sse, anthropic json, anthropic sse. The four byte
# lengths are asserted alongside it because a concatenation digest cannot see a
# byte moved across a fixture boundary. The resolved directory is compared too and
# is deliberately not recorded in any results file, because it is an absolute
# home-directory path the identity audit forbids.
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

# Both endpoints in play are written by encoding/json with no indentation and no
# nesting deeper than one level, and neither value can contain an escaped quote, so
# a sed extraction is honest here and keeps the harness free of a JSON dependency.
json_string_field() {
  printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"
}

json_number_field() {
  printf '%s' "$1" | sed -n "s/.*\"$2\":\([0-9]*\).*/\1/p"
}

# json_decimal_field reads a field whose value may carry a decimal point or an
# exponent, which json_number_field cannot do: its [0-9]* class stops at the
# point, so it silently returns 0 for 0.06 and 1 for 1.6e-05. That truncation
# would turn a recorded failure rate into a reassuring zero. All three keep the
# leading quote in the pattern so a key can never be matched as the suffix of a
# longer one.
json_decimal_field() {
  printf '%s' "$1" | sed -n "s/.*\"$2\":\([-0-9.eE+]*\).*/\1/p"
}

# Levee needs about 10.94ms of CPU per enforced 32768B request against 0.233ms for
# a passthrough one, so no single rate sits at a sane utilization in both arms at
# every size. Target utilization is at most roughly 40 percent of measured
# capacity, above which a queue forms and the cell reports residence time.
#
# The rate belongs to the payload size rather than to the cell because the published
# estimator subtracts arms at one size, so they must share one rate or the shift
# compares two different operating points. overhead_figure.py enforces that
# independently: Group.rate raises when a group mixes rates. The binding constraint
# at each size is therefore the slowest arm, always enforce. Derivations in
# benchmarks/methodology/calibration.md.
rate_for_payload() {
  case "$1" in
    150)
      # Not separately measured, and bounded below by the 4096B figure because
      # estimation cost is linear in prompt bytes at about 120ns per byte and
      # every other term is shared. Unchanged deliberately: every historical
      # reading the bands are calibrated against was taken at this rate.
      printf '500\n'
      ;;
    4096)
      # 18 percent of a measured enforce capacity of 2706 rps.
      printf '500\n'
      ;;
    32768)
      # 37.5 percent of a measured enforce peak of about 400 rps. Throughput is
      # retrograde past that peak, 295 rps at concurrency 8 and 260 at 40, so a
      # bigger VU pool makes this worse rather than better. Passthrough here has
      # capacity above 7100 rps, so it runs at about 2 percent. Both arms share
      # the rate, so the rate belongs to the slower arm.
      printf '150\n'
      ;;
    *)
      fail "no measured capacity for a ${1}B prompt, measure it and add it to rate_for_payload before benchmarking that size"
      ;;
  esac
}

# Integer arithmetic, because the shell has no floats and a floor is the correct
# rounding for a minimum.
min_steady_requests() {
  local rate="$1"
  local seconds="$2"
  printf '%s\n' "$(( rate * seconds * (100 - RATE_SHORTFALL_TOLERANCE_PERCENT) / 100 ))"
}

# The denominator both integrity tolerances are fractions of. The plus one k6
# actually schedules is deliberately not added: the clean product makes the
# allowance a hair smaller than the observed population, which is the safe
# direction, and it keeps this the same quantity min_steady_requests takes its
# percentage of.
demanded_steady_requests() {
  local rate="$1"
  local seconds="$2"
  printf '%s\n' "$(( rate * seconds ))"
}

# check_bands.py mirrors this integer operation with floor division rather than a
# float multiply, so a cell can never be judged one number by k6 and a different
# number by the band checker.
tolerance_from_basis_points() {
  local demanded="$1"
  local points="$2"
  local floor="$3"
  local scaled=$(( demanded * points / 10000 ))
  if [ "${scaled}" -lt "${floor}" ]
  then
    printf '%s\n' "${floor}"
  else
    printf '%s\n' "${scaled}"
  fi
}

max_steady_dropped_iterations() {
  tolerance_from_basis_points "$1" "${STEADY_DROP_TOLERANCE_BASIS_POINTS}" \
    "${STEADY_DROP_TOLERANCE_FLOOR}"
}

max_steady_failed_requests() {
  tolerance_from_basis_points "$1" "${STEADY_FAILED_TOLERANCE_BASIS_POINTS}" \
    "${STEADY_FAILED_TOLERANCE_FLOOR}"
}

# levee's CPU time at the two edges of the steady window, so CPU seconds per
# request falls out of the artifact instead of having to be inferred from a latency
# curve. It runs as a background job alongside k6 rather than bracketing the whole
# invocation, because bracketing would fold the warmup scenario's CPU into the
# delta while the request count divides only the steady rows.
#
# ps -o cputime reports the process's own accumulated user plus system time.
# Probed on this host: a short-lived child printed "  0:00.04" then "  0:00.12" two
# seconds apart, so the field is [DD-][HH:]MM:SS.ss with leading padding and
# hundredth-of-a-second resolution.
#
# A direct cell has no levee in the path, so the pid is empty and both readings
# record "na" rather than a zero that would read as a measured absence of work.
sample_levee_cpu() {
  local out="$1"
  local pid="$2"
  local start_delay="$3"
  local window="$4"

  if [ -z "${pid}" ]
  then
    printf 'na na\n' > "${out}"
    return 0
  fi

  sleep "${start_delay}"
  local before after
  before="$(ps -o cputime= -p "${pid}" 2>/dev/null | tr -d ' ')"
  sleep "${window}"
  after="$(ps -o cputime= -p "${pid}" 2>/dev/null | tr -d ' ')"
  printf '%s %s\n' "${before:-na}" "${after:-na}" > "${out}"
}

# The parse folds colon-separated fields from the left at 60 each, so it handles
# MM:SS.ss and HH:MM:SS.ss without caring which it got, and adds any DD- prefix
# that a process older than a day would carry. levee never lives that long inside
# one cell, and the prefix is handled anyway so an unexpected format cannot
# silently parse to a wrong number. Either reading absent prints "na".
cpu_delta_seconds() {
  awk -v before="$1" -v after="$2" '
    function to_seconds(text) {
      days = 0
      if (index(text, "-") > 0)
      {
        split(text, halves, "-")
        days = halves[1] + 0
        text = halves[2]
      }
      count = split(text, fields, ":")
      total = 0
      for (position = 1; position <= count; position++)
      {
        total = total * 60 + fields[position]
      }
      return total + days * 86400
    }
    BEGIN {
      if (before == "na" || after == "na" || before == "" || after == "")
      {
        print "na"
        exit 0
      }
      printf "%.2f\n", to_seconds(after) - to_seconds(before)
    }'
}

# The divisor is the steady request count from the cell summary, which is the same
# population the sampling window covers and the same population every published
# percentile comes from.
record_cpu_per_request() {
  local cell="$1"
  local samples="$2"
  local summary="$3"

  local before after
  before="na"
  after="na"
  if [ -s "${samples}" ]
  then
    before="$(awk '{ print $1 }' < "${samples}")"
    after="$(awk '{ print $2 }' < "${samples}")"
  fi

  local delta
  delta="$(cpu_delta_seconds "${before}" "${after}")"

  local steady_requests=""
  if [ -s "${summary}" ]
  then
    steady_requests="$(json_number_field "$(tr -d '\n ' < "${summary}")" steady_request_count)"
  fi

  local per_request
  per_request="$(awk -v delta="${delta}" -v requests="${steady_requests:-0}" 'BEGIN {
    if (delta == "na" || requests + 0 <= 0) { print "na"; exit 0 }
    printf "%.4f", (delta * 1000.0) / requests
  }')"

  printf '%s cpu_seconds=%s steady_requests=%s cpu_ms_per_request=%s\n' \
    "${cell}" "${delta}" "${steady_requests:-unknown}" "${per_request}" \
    >> "${RESULTS_DIR}/cpu-seconds.txt"
}

# Achieved throughput subsumes the drop count. A cell can miss its demanded rate
# without k6 dropping a single iteration, if the VU pool absorbs the growing queue
# instead of running out of workers, and then the latency number is queue
# residence while every drop gate reads clean. Whether the backlog drops or merely
# waits, the completions inside the window still fall short of the demand.
record_achieved_rate() {
  local cell="$1"
  local summary="$2"
  local demanded="$3"
  local minimum="$4"

  if [ ! -s "${summary}" ]
  then
    printf '%s demanded_rps=%s achieved_rps=unknown minimum_requests=%s, k6 wrote no summary\n' \
      "${cell}" "${demanded}" "${minimum}" >> "${RESULTS_DIR}/achieved-rate.txt"
    return 0
  fi

  local flat steady_requests
  flat="$(tr -d '\n ' < "${summary}")"
  steady_requests="$(json_number_field "${flat}" steady_request_count)"

  awk -v cell="${cell}" -v demanded="${demanded}" -v minimum="${minimum}" \
    -v requests="${steady_requests:-0}" -v seconds="${STEADY_SECONDS}" 'BEGIN {
      achieved = (seconds > 0) ? requests / seconds : 0
      shortfall = (demanded > 0) ? (1.0 - achieved / demanded) * 100.0 : 0
      printf "%s demanded_rps=%s achieved_rps=%.2f shortfall_pct=%.2f steady_requests=%s minimum_requests=%s\n", \
        cell, demanded, achieved, shortfall, requests, minimum
    }' >> "${RESULTS_DIR}/achieved-rate.txt"
}

# Every latency artifact in the results tree comes from run_cell, so the filtering
# is code, never a manual step.
#
# k6 writes its progress and its handleSummary stdout line to stdout, redirected
# to stderr here. This script's stdout carries exactly one thing, the results
# directory path, so a caller can capture it.
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

  # The pool is identical in every cell. Under any queueing the pool size is part of
  # what is being measured, since it bounds how much backlog can form before k6
  # drops instead of waiting, so two arms with different pools are not comparable.
  #
  # It must stay strictly under 50 because internal/budget/store.go hardcodes
  # DefaultStreamLimit at 50 admission slots per agent, a slot is held until the
  # deferred reconcile runs after the response bytes have already reached k6, and an
  # exhausted slot answers 429 rather than queueing. A 429 fails the http_req_failed
  # threshold and perturbs levee-side state, so equalizing means bringing
  # passthrough down to 40 rather than enforce up.
  #
  # The pool is fully preallocated. k6 validates preAllocatedVUs against maxVUs and
  # exits 104 on "maxVUs can't be less than preAllocatedVUs", and a pool that grows
  # mid-run would initialize VUs inside the steady window.
  local preallocated_vus="${max_vus}"

  local steady_seconds="${STEADY_SECONDS}"
  local minimum_requests
  minimum_requests="$(min_steady_requests "${rate}" "${steady_seconds}")"

  # Both allowances are derived from this cell's own demand rather than fixed, so
  # the k6 threshold expressions carry real numbers.
  local demanded_requests max_dropped max_failed
  demanded_requests="$(demanded_steady_requests "${rate}" "${steady_seconds}")"
  max_dropped="$(max_steady_dropped_iterations "${demanded_requests}")"
  max_failed="$(max_steady_failed_requests "${demanded_requests}")"

  log "cell ${cell}: rate ${rate}, stream ${stream}, prompt ${prompt_bytes}B, vus ${preallocated_vus} to ${max_vus}, demanded steady requests ${demanded_requests}, minimum steady requests ${minimum_requests}, allowed steady drops ${max_dropped}, allowed steady failures ${max_failed}"
  record_machine_state "${cell}" "before"

  # The CPU sampler is started before k6 so its own STEADY_START delay is
  # measured from the same instant k6 measures its scenario start times from. It
  # is reaped unconditionally below, even on a k6 failure, so no sleeper outlives
  # the cell. On a k6 invocation that dies immediately, which only a broken
  # option can cause, that reap waits out the remaining window once.
  local cpu_samples="${WORK_DIR}/${cell}.cpu"
  sample_levee_cpu "${cpu_samples}" "${LEVEE_PID}" "${STEADY_START_SECONDS}" "${steady_seconds}" &
  local cpu_sampler_pid=$!

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
  STEADY_SECONDS="${steady_seconds}" \
  MIN_STEADY_REQUESTS="${minimum_requests}" \
  DEMANDED_STEADY_REQUESTS="${demanded_requests}" \
  MAX_STEADY_DROPPED_ITERATIONS="${max_dropped}" \
  MAX_STEADY_FAILED_REQUESTS="${max_failed}" \
  PROMPT_BYTES="${prompt_bytes}" \
  STREAM="${stream}" \
    k6 run --out "csv=${raw}" "${BENCH_DIR}/k6/overhead.js" >&2 || k6_status=$?

  wait "${cpu_sampler_pid}" 2>/dev/null || true

  record_machine_state "${cell}" "after"
  printf '%s k6_exit=%s\n' "${cell}" "${k6_status}" >> "${RESULTS_DIR}/k6-exit-codes.txt"
  record_dropped_iterations "${cell}" "${summary}" "${max_dropped}" "${demanded_requests}"
  record_failed_requests "${cell}" "${summary}" "${max_failed}" "${demanded_requests}"
  record_achieved_rate "${cell}" "${summary}" "${rate}" "${minimum_requests}"
  record_cpu_per_request "${cell}" "${cpu_samples}" "${summary}"

  if [ "${k6_status}" -ne 0 ]
  then
    printf 'cell %s failed an integrity threshold, k6 exit %s\n' "${cell}" "${k6_status}" \
      >> "${ATTEMPTS_FILE}"
    fail "cell ${cell} failed an integrity threshold (k6 exit ${k6_status}), this run is invalid"
  fi

  filter_cell_csv "${raw}" "${filtered}" "${cell}"
}

# Warmup rows are excluded by the scenario tag rather than by timestamp
# arithmetic, since k6 phase timings are wall-clock based and the default CSV
# timestamp resolution is one second. Row counts before and after are recorded so
# the derivation from the discarded raw file stays checkable.
#
# The scenario column index is read out of the header rather than hardcoded, and
# matched whole rather than as a substring anywhere in the row. Substring matching
# would silently keep a warmup row whose URL or tag carried the word, and a k6
# column reorder would then be invisible instead of emptying the output and
# tripping the row-count check below.
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

# The allowance and the demanded count sit on every line beside the raw counts, so
# a reader can see how far inside its budget each cell sat. Warmup drops are
# tolerated by design and recorded anyway, because the enforce cells legitimately
# drop tens of iterations there while levee builds the o200k_base encoder on its
# first enforced request, which blocks the pool for roughly 130ms.
#
# This runs even for a cell that failed its threshold, which is how a reader of an
# aborted directory sees where the drops landed.
record_dropped_iterations() {
  local cell="$1"
  local summary="$2"
  local allowed="$3"
  local demanded="$4"
  if [ ! -s "${summary}" ]
  then
    printf '%s steady=unknown warmup=unknown allowed=%s demanded=%s, k6 wrote no summary\n' \
      "${cell}" "${allowed}" "${demanded}" >> "${RESULTS_DIR}/dropped-iterations.txt"
    return 0
  fi
  local steady warmup
  steady="$(json_number_field "$(tr -d '\n ' < "${summary}")" dropped_iterations_steady)"
  warmup="$(json_number_field "$(tr -d '\n ' < "${summary}")" dropped_iterations_warmup)"
  printf '%s steady=%s warmup=%s allowed=%s demanded=%s\n' \
    "${cell}" "${steady:-absent}" "${warmup:-absent}" "${allowed}" "${demanded}" \
    >> "${RESULTS_DIR}/dropped-iterations.txt"
}

# A tolerated count that appears nowhere in the artifact tree is a tolerated count
# nobody can audit. Every cell writes a line whether it failed a request or not,
# so a present-and-all-zero file is a positive statement rather than an absence of
# evidence.
record_failed_requests() {
  local cell="$1"
  local summary="$2"
  local allowed="$3"
  local demanded="$4"
  if [ ! -s "${summary}" ]
  then
    printf '%s steady=unknown warmup=unknown allowed=%s demanded=%s, k6 wrote no summary\n' \
      "${cell}" "${allowed}" "${demanded}" >> "${RESULTS_DIR}/failed-requests.txt"
    return 0
  fi
  local flat steady warmup rate
  flat="$(tr -d '\n ' < "${summary}")"
  steady="$(json_number_field "${flat}" http_req_failed_steady_count)"
  warmup="$(json_number_field "${flat}" http_req_failed_warmup_count)"
  rate="$(json_decimal_field "${flat}" http_req_failed_steady_rate)"
  printf '%s steady=%s warmup=%s steady_rate=%s allowed=%s demanded=%s\n' \
    "${cell}" "${steady:-absent}" "${warmup:-absent}" "${rate:-absent}" \
    "${allowed}" "${demanded}" >> "${RESULTS_DIR}/failed-requests.txt"
}

# One system-wide CPU idle percentage, or "na" when it cannot be read. An
# unreadable sensor is never reported as a passing number, because the gate treats
# an unknown host as a refused host.
#
# The source, verified on this host: top's CPU line reads
# "CPU usage: 15.95% user, 18.9% sys, 65.95% idle", so the parse splits on commas,
# takes the field containing "idle", and strips everything that is not a digit or a
# dot. The percentage carries one or two decimals depending on the value, which is
# why the strip is a character class rather than a fixed cut.
#
# top -l 2 -n 0 -s 2 prints two "CPU usage" lines and the awk keeps the LAST one,
# a true 2 second interval average costing about 2.5 seconds. Keeping the first
# would reintroduce the noisy startup reading this exists to avoid: probed here,
# five -l 1 samples read 60.96, 41.86, 59.75, 57.51 and 58.16 on an untouched
# machine, while the interval form spread only 59.85 to 72.76 over 10 readings.
#
# LEVEE_BENCH_SYNTHETIC_IDLE_READINGS is the test hook and this is the only place
# that serves a value from it. It cannot affect a real run, because preflight
# refuses to start while it is set and preflight sits on the only path that creates
# a results directory. An exhausted list fails loudly rather than degrading to
# "na", which would let a mis-written test read as a passing one.
sample_cpu_idle_percent() {
  if [ -n "${LEVEE_BENCH_SYNTHETIC_IDLE_READINGS:-}" ]
  then
    next_synthetic_idle_reading
    return 0
  fi

  local snapshot
  snapshot="$(top -l 2 -n 0 -s 2 2>/dev/null || true)"
  printf '%s\n' "${snapshot}" | awk -F',' '
    /^CPU usage/ {
      for (position = 1; position <= NF; position++)
      {
        if ($position ~ /idle/) { latest = $position }
      }
    }
    END {
      if (latest == "") { print "na"; exit 0 }
      gsub(/[^0-9.]/, "", latest)
      if (latest == "") { print "na"; exit 0 }
      print latest
    }'
}

next_synthetic_idle_reading() {
  local read_index=1
  if [ -s "${SYNTHETIC_IDLE_POSITION_FILE}" ]
  then
    read_index="$(cat "${SYNTHETIC_IDLE_POSITION_FILE}")"
  fi
  printf '%s\n' "$((read_index + 1))" > "${SYNTHETIC_IDLE_POSITION_FILE}"

  local reading
  reading="$(printf '%s\n' "${LEVEE_BENCH_SYNTHETIC_IDLE_READINGS:-}" \
    | awk -v wanted="${read_index}" '{
        for (position = 1; position <= NF; position++) { values[++total] = $position }
      }
      END {
        if (wanted > total) { print "" ; exit 0 }
        print values[wanted]
      }')"
  if [ -z "${reading}" ]
  then
    fail "the scripted idle reading list is exhausted at index ${read_index}, so the verification driver asked for more readings than it supplied. Extend LEVEE_BENCH_SYNTHETIC_IDLE_READINGS. Remember that robust_cpu_idle_percent consumes THREE readings whenever the first one falls below the floor and ONE otherwise"
  fi
  printf '%s\n' "${reading}"
}

# The comparison lives in awk because the shell has no floats and the readings
# carry two decimals.
idle_below_floor() {
  awk -v value="$1" -v floor="${HOST_IDLE_FLOOR_PERCENT}" 'BEGIN {
    print (value + 0 < floor + 0) ? "yes" : "no"
  }'
}

# The reading both gates act on: one sample, re-sampled twice more only when it
# falls below the floor, then the median of the three. The confirmation cannot
# weaken the gate, because it is two more real measurements rather than a retry
# until success, and a host with sustained contention reads below the floor on all
# three since the contended regime's single-sample maximum was 53.62 against the
# floor of 60. Only a transient can be voted out, which is why the mid-run policy
# needs CONSECUTIVE_BREACH_LIMIT and PERVASIVE_BREACH_FRACTION_PERCENT as well.
robust_cpu_idle_percent() {
  local first second third
  first="$(sample_cpu_idle_percent)"
  if [ "${first}" = "na" ]
  then
    printf 'na\n'
    return 0
  fi
  if [ "$(idle_below_floor "${first}")" = "no" ]
  then
    printf '%s\n' "${first}"
    return 0
  fi
  second="$(sample_cpu_idle_percent)"
  third="$(sample_cpu_idle_percent)"
  if [ "${second}" = "na" ] || [ "${third}" = "na" ]
  then
    printf '%s\n' "${first}"
    return 0
  fi
  awk -v a="${first}" -v b="${second}" -v c="${third}" 'BEGIN {
    values[1] = a + 0
    values[2] = b + 0
    values[3] = c + 0
    for (outer = 1; outer <= 3; outer++)
    {
      for (inner = outer + 1; inner <= 3; inner++)
      {
        if (values[inner] < values[outer])
        {
          swap = values[outer]
          values[outer] = values[inner]
          values[inner] = swap
        }
      }
    }
    printf "%.2f\n", values[2]
  }'
}

# The startup gate. One sub-floor reading refuses an evidence run outright, before
# the first cell. Quick mode warns and continues.
#
# An unreadable reading refuses an evidence run as well. A gate whose sensor is
# broken has to fail closed, or the first tool change turns the whole check into a
# silent pass that still prints reassuring text. Startup is also the right place
# for that to be strict: if the sampler is permanently broken it is broken here,
# so every mid-run "na" after a clean startup is a transient.
enforce_startup_quiescence() {
  local idle="$1"

  if [ "${idle}" = "na" ]
  then
    if [ "${MODE}" = "evidence" ]
    then
      fail "host CPU idle could not be read at startup, so host quiescence is UNKNOWN. An evidence run refuses an unreadable gate rather than treating it as a pass, because the contention this gate exists to catch is invisible to every other check in the harness"
    fi
    log "warning: host CPU idle could not be read at startup, quick mode continues, an evidence run would refuse here"
    return 0
  fi

  if [ "$(idle_below_floor "${idle}")" = "no" ]
  then
    return 0
  fi

  if [ "${MODE}" = "evidence" ]
  then
    fail "host CPU idle is ${idle} percent at startup, below the ${HOST_IDLE_FLOOR_PERCENT} percent floor an evidence run requires. The machine is not quiet enough to measure on. Contention of roughly this size was proven SUFFICIENT to move the 150B enforcement shift from +15us to over +100us while every integrity gate still read clean, which is what invalidated the first completed 43-cell run. Quit the browser, the video-conferencing app and any background build, wait for the endpoint-security agents and the file indexer to settle, and start again"
  fi
  log "warning: host CPU idle is ${idle} percent at startup, below the ${HOST_IDLE_FLOOR_PERCENT} percent floor, quick mode records it and continues, an evidence run would refuse here"
}

# An unreadable reading counts as a breach, because it is a host that cannot be
# shown to be quiet and an unknown host is a refused host. It gets its own reason
# rather than being folded into the sub-floor case, so a reader of
# contended-cells.txt can tell a busy machine from a blind one.
breach_reason() {
  local idle="$1"
  if [ "${idle}" = "na" ]
  then
    printf 'idle_unreadable\n'
    return 0
  fi
  if [ "$(idle_below_floor "${idle}")" = "yes" ]
  then
    printf 'idle_below_floor\n'
    return 0
  fi
  printf 'none\n'
}

# One line per reading, not per cell, so a cell contended at both edges appears
# twice with different phases, which is exactly the sustained case. check_bands.py
# keys on the cell name so the duplication costs it nothing.
record_contended_cell() {
  local cell="$1"
  local phase="$2"
  local reason="$3"
  local idle="$4"
  printf 'cell=%s phase=%s reason=%s cpu_idle_pct=%s cpu_idle_floor_pct=%s\n' \
    "${cell}" "${phase}" "${reason}" "${idle}" "${HOST_IDLE_FLOOR_PERCENT}" \
    >> "${CONTENDED_CELLS_FILE}"
}

# The mid-run gate. It refuses on sustained breach rather than on one reading,
# against CONSECUTIVE_BREACH_LIMIT and PERVASIVE_BREACH_FRACTION_PERCENT.
#
# An isolated breach is recorded, warned about, and the run continues. It is not
# ignored: the cell goes into contended-cells.txt, the reading carries
# cpu_idle_breach=yes in machine-state.txt, and check_bands.py drops that
# repetition out of every median. A transient costs one repetition of five rather
# than 52 minutes.
evaluate_mid_run_quiescence() {
  local cell="$1"
  local phase="$2"
  local idle="$3"
  local reason="$4"
  local label="cell ${cell} phase ${phase}"

  MID_RUN_READING_COUNT=$((MID_RUN_READING_COUNT + 1))

  # A clean reading breaks the run of consecutive breaches. Resetting here rather
  # than only counting upward is what makes the rule about adjacency.
  if [ "${reason}" = "none" ]
  then
    MID_RUN_CONSECUTIVE_BREACHES=0
    MID_RUN_CONSECUTIVE_BREACH_LABELS=""
    return 0
  fi

  MID_RUN_BREACH_COUNT=$((MID_RUN_BREACH_COUNT + 1))
  MID_RUN_CONSECUTIVE_BREACHES=$((MID_RUN_CONSECUTIVE_BREACHES + 1))
  if [ -z "${MID_RUN_CONSECUTIVE_BREACH_LABELS}" ]
  then
    MID_RUN_CONSECUTIVE_BREACH_LABELS="${label}"
  else
    MID_RUN_CONSECUTIVE_BREACH_LABELS="${MID_RUN_CONSECUTIVE_BREACH_LABELS} then ${label}"
  fi

  log "warning: host quiescence breach at ${label}, cpu_idle_pct=${idle}, floor ${HOST_IDLE_FLOOR_PERCENT}, reason ${reason}. That is breach ${MID_RUN_BREACH_COUNT} of ${MID_RUN_READING_COUNT} mid-run readings so far, and ${MID_RUN_CONSECUTIVE_BREACHES} in a row. The cell is recorded in contended-cells.txt and check_bands.py will drop its repetition from every median"

  if [ "${MID_RUN_CONSECUTIVE_BREACHES}" -lt "${CONSECUTIVE_BREACH_LIMIT}" ]
  then
    return 0
  fi

  # The limit is reached. The before and after readings of one cell are adjacent
  # in this sequence, so this fires both for a cell contended throughout and for
  # contention that spanned a cell boundary. Both are sustained.
  if [ "${MODE}" = "evidence" ]
  then
    fail "host quiescence breached on ${MID_RUN_CONSECUTIVE_BREACHES} CONSECUTIVE readings, which reaches the limit of ${CONSECUTIVE_BREACH_LIMIT}. The consecutive breaches were ${MID_RUN_CONSECUTIVE_BREACH_LABELS}, the last at cpu_idle_pct=${idle} against the ${HOST_IDLE_FLOOR_PERCENT} percent floor. Adjacent breaches mean the contention PERSISTED rather than passing through, and sustained contention of this kind is what invalidated the first completed evidence run: it held roughly 108us of amplification across every repetition of the 150-byte enforcement pair while every integrity gate read clean. A single isolated dip would have been recorded and tolerated, so this run stopped because the machine stayed busy. Quit the browser, the video-conferencing app and any background build, wait for the endpoint-security agents and the file indexer to settle, and start again"
  fi
  log "warning: host quiescence breached on ${MID_RUN_CONSECUTIVE_BREACHES} consecutive readings, ${MID_RUN_CONSECUTIVE_BREACH_LABELS}, reaching the limit of ${CONSECUTIVE_BREACH_LIMIT}, quick mode continues, an evidence run would refuse here"
}

# The second half of the sustained-breach rule, once after the last cell, for the
# pervasive-but-intermittent host that never lands two breaches side by side.
#
# It is called before stage_manifest on purpose. A run that fails here leaves a
# directory with no MANIFEST, which the results README defines as an aborted run
# that can never masquerade as evidence, and it leaves bands.txt behind so the
# numbers are still readable for diagnosis.
enforce_quiescence_breach_budget() {
  if [ "${MID_RUN_READING_COUNT}" -le 0 ]
  then
    log "no mid-run host quiescence readings were taken, so the pervasive-breach budget has nothing to evaluate"
    return 0
  fi

  # Integer arithmetic, because the shell has no floats. Fails when the breach
  # fraction is STRICTLY GREATER than the permitted percentage.
  local scaled_breaches permitted
  scaled_breaches=$((MID_RUN_BREACH_COUNT * 100))
  permitted=$((MID_RUN_READING_COUNT * PERVASIVE_BREACH_FRACTION_PERCENT))

  printf '# summary: %s of %s mid-run readings breached the %s percent idle floor, budget is %s percent\n' \
    "${MID_RUN_BREACH_COUNT}" "${MID_RUN_READING_COUNT}" \
    "${HOST_IDLE_FLOOR_PERCENT}" "${PERVASIVE_BREACH_FRACTION_PERCENT}" \
    >> "${CONTENDED_CELLS_FILE}"

  if [ "${scaled_breaches}" -le "${permitted}" ]
  then
    log "host quiescence breach budget satisfied, ${MID_RUN_BREACH_COUNT} of ${MID_RUN_READING_COUNT} mid-run readings breached, budget is ${PERVASIVE_BREACH_FRACTION_PERCENT} percent"
    return 0
  fi

  if [ "${MODE}" = "evidence" ]
  then
    printf 'pervasive host contention, %s of %s mid-run readings below the idle floor\n' \
      "${MID_RUN_BREACH_COUNT}" "${MID_RUN_READING_COUNT}" >> "${ATTEMPTS_FILE}"
    fail "host quiescence breached on ${MID_RUN_BREACH_COUNT} of ${MID_RUN_READING_COUNT} mid-run readings, which is more than the ${PERVASIVE_BREACH_FRACTION_PERCENT} percent budget an evidence run allows. No two breaches were necessarily adjacent, so the consecutive rule did not fire, and that is the case this budget exists for: a host that is contended THROUGHOUT but intermittently reads above the floor. The recorded example is the quick matrix at 31918d9, 5 of 26 readings with none adjacent, on a host whose worst reading of 51.48 sits inside the proven contended regime. A run this pervasively contended cannot be published however clean its bands look, because the contention that invalidated the first evidence run passed every band. The offending cells are named in contended-cells.txt. No MANIFEST was written, so this directory is visibly an aborted run"
  fi
  log "warning: host quiescence breached on ${MID_RUN_BREACH_COUNT} of ${MID_RUN_READING_COUNT} mid-run readings, past the ${PERVASIVE_BREACH_FRACTION_PERCENT} percent budget, quick mode continues, an evidence run would refuse here"
}

# A cell polluted by a background spike or a power-source change is otherwise
# indistinguishable from a real regression. cpu_idle_pct is the gating field,
# sampled at both edges of every cell while k6 is not running, so it measures the
# ambient host rather than the benchmark's own load.
#
# Read the two phases separately when a number looks odd. On this host the before
# reading runs systematically LOWER than the after reading, by 6.6 and 10.9 points
# of mean idle in the two runs the gate was calibrated against and by 3.2 and 5.6
# in the two quick runs since. The before sample is taken right after a levee
# spawn, a config render and the previous cell's TIME_WAIT drain, so its 2 second
# window can overlap the harness's own setup work rather than pure ambient load.
# The bias is left in place because every calibration figure was measured through
# this same sampler. Without this note a reader diagnoses a phantom background job.
#
# Only the DIRECTION is durable, not the counts. A lightly loaded host breaches on
# before readings only. A heavily loaded one breaches on both, measured at 9 of 13
# after readings on one contended run, so do not use the phase to rule out
# contention.
#
# The thermal reading is a placeholder when pmset has nothing to report, the normal
# case on Apple Silicon: pmset -g therm answers "No CPU power status has been
# recorded" and never prints the CPU_Speed_Limit line Intel hosts do. An empty
# value would read as a broken capture rather than an absent one.
record_machine_state() {
  local cell="$1"
  local phase="$2"
  local thermal idle reason breached
  thermal="$(pmset -g therm 2>/dev/null | sed -n 's/.*CPU_Speed_Limit *= *\([0-9]*\).*/\1/p' | head -1)"
  idle="$(robust_cpu_idle_percent)"
  reason="$(breach_reason "${idle}")"
  if [ "${reason}" = "none" ]
  then
    breached="no"
  else
    breached="yes"
  fi
  {
    printf 'cell=%s phase=%s ' "${cell}" "${phase}"
    printf 'cpu_idle_pct=%s ' "${idle}"
    printf 'cpu_idle_floor_pct=%s ' "${HOST_IDLE_FLOOR_PERCENT}"
    printf 'cpu_idle_breach=%s ' "${breached}"
    # loadavg is kept and is not a gate. It is recorded precisely because it was
    # proven not to discriminate between the invalidated run and the quiet
    # re-measurements, so a reader can see the two fields disagree rather than
    # having to take that finding on trust.
    printf 'loadavg=%s ' "$(sysctl -n vm.loadavg | tr -d '{}' | tr -s ' ' '_')"
    printf 'thermal=%s ' "${thermal:-none-reported}"
    printf 'power=%s ' "$(pmset -g ps | head -1 | tr ' ' '_')"
    printf 'timewait=%s\n' "$(count_timewait)"
  } >> "${RESULTS_DIR}/machine-state.txt"
  if [ "${breached}" = "yes" ]
  then
    record_contended_cell "${cell}" "${phase}" "${reason}" "${idle}"
  fi
  evaluate_mid_run_quiescence "${cell}" "${phase}" "${idle}" "${reason}"
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

# The levee-to-mock leg uses the standard library default of two idle connections
# per host, so a burst of dials leaves TIME_WAIT entries that would otherwise raise
# the following cell's tail.
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

# The matrix, in execution order. Every cell below is one k6 invocation, and all
# cells share one mock boot so they are directly comparable.
#
#   1. direct-canary-open-nonstream-150, the opening drift canary.
#   2. For each repetition up to REPETITIONS, for each size in PROMPT_SIZES:
#        a. passthrough at that size, then enforce at that size
#        b. at AA_CONTROL_PAYLOAD_BYTES only, controla then controlb, both on the
#           passthrough config, so the pair has a known true shift of zero
#   3. For each repetition up to STREAM_REPETITIONS, at 150 bytes: direct-stream,
#      then passthrough-stream, then enforce-stream.
#   4. direct-payload-4096 then direct-payload-32768.
#   5. direct-canary-close-nonstream-150, the closing drift canary.
#
# Levee is started before every proxied cell and stopped after it, so no two
# proxied cells share a process, and every proxied cell is then followed by
# wait_for_timewait_drain. Direct cells get neither, because nothing was started for
# them and nothing has to drain.
#
# The pair repeats because the enforcement signal is smaller than plausible drift
# between cells run minutes apart.
#
# Every size in PROMPT_SIZES needs a direct cell somewhere in steps 1, 3 or 4. The
# published estimator is a quantile shift, proxied minus direct at the same size and
# rate, so a size with no direct cell has no computable shift and
# overhead_figure.py prints SKIPPED for that row instead of a number.
#
# The cells, the estimator and the load model are in
# benchmarks/methodology/estimator-and-matrix.md.
run_matrix() {
  local direct_target="http://127.0.0.1:${MOCK_PORT}/v1/chat/completions"
  local proxy_target="http://127.0.0.1:${PROXY_PORT}/openai/v1/chat/completions"

  local rate_150 rate_4096 rate_32768
  rate_150="$(rate_for_payload 150)"
  rate_4096="$(rate_for_payload 4096)"
  rate_32768="$(rate_for_payload 32768)"

  run_cell "direct-canary-open-nonstream-150" "${direct_target}" false 150 "${rate_150}" "${BENCH_MAX_VUS}"

  local repetition=1
  while [ "${repetition}" -le "${REPETITIONS}" ]
  do
    local bytes
    for bytes in ${PROMPT_SIZES}
    do
      local rate
      rate="$(rate_for_payload "${bytes}")"

      start_levee passthrough
      run_cell "passthrough-nonstream-${bytes}-r${repetition}" "${proxy_target}" false "${bytes}" "${rate}" "${BENCH_MAX_VUS}"
      stop_levee
      wait_for_timewait_drain

      start_levee enforce
      run_cell "enforce-nonstream-${bytes}-r${repetition}" "${proxy_target}" false "${bytes}" "${rate}" "${BENCH_MAX_VUS}"
      stop_levee
      wait_for_timewait_drain

      # The A/A control sits immediately after the pair it calibrates so it sees the
      # same host conditions, and restarts levee between its two arms because the
      # pair it calibrates does. Measured on a quiet host the A/A shift is -1, +4
      # and 0us, so the noise floor is about 4us. It is not a gate: a contended A/A
      # pair still read 13us, so a passing control does not certify a quiet host.
      if [ "${bytes}" = "${AA_CONTROL_PAYLOAD_BYTES}" ]
      then
        start_levee passthrough
        run_cell "controla-nonstream-${bytes}-r${repetition}" "${proxy_target}" false "${bytes}" "${rate}" "${BENCH_MAX_VUS}"
        stop_levee
        wait_for_timewait_drain

        start_levee passthrough
        run_cell "controlb-nonstream-${bytes}-r${repetition}" "${proxy_target}" false "${bytes}" "${rate}" "${BENCH_MAX_VUS}"
        stop_levee
        wait_for_timewait_drain
      fi
    done
    repetition=$((repetition + 1))
  done

  local stream_repetition=1
  while [ "${stream_repetition}" -le "${STREAM_REPETITIONS}" ]
  do
    run_cell "direct-stream-150-r${stream_repetition}" "${direct_target}" true 150 "${RATE_STREAM}" "${BENCH_MAX_VUS}"

    start_levee passthrough
    run_cell "passthrough-stream-150-r${stream_repetition}" "${proxy_target}" true 150 "${RATE_STREAM}" "${BENCH_MAX_VUS}"
    stop_levee
    wait_for_timewait_drain

    start_levee enforce
    run_cell "enforce-stream-150-r${stream_repetition}" "${proxy_target}" true 150 "${RATE_STREAM}" "${BENCH_MAX_VUS}"
    stop_levee
    wait_for_timewait_drain

    stream_repetition=$((stream_repetition + 1))
  done

  run_cell "direct-payload-4096" "${direct_target}" false 4096 "${rate_4096}" "${BENCH_MAX_VUS}"
  run_cell "direct-payload-32768" "${direct_target}" false 32768 "${rate_32768}" "${BENCH_MAX_VUS}"
  run_cell "direct-canary-close-nonstream-150" "${direct_target}" false 150 "${rate_150}" "${BENCH_MAX_VUS}"
}

# Arrival rates are deliberately not set here, because each payload size carries
# its own rate from rate_for_payload.
#
# Durations are held as integer seconds and the k6 duration strings are derived
# from them, because the CPU sampler and the achieved-rate arithmetic both need the
# number and neither should be parsing a "20s" string back apart.
configure_mode() {
  RATE_STREAM=250
  WARMUP_SECONDS=10
  STEADY_START_SECONDS=12
  BENCH_MAX_VUS="${BENCH_MAX_VUS:-40}"
  case "${MODE}" in
    quick)
      STEADY_SECONDS=20
      REPETITIONS=1
      STREAM_REPETITIONS=1
      # Overridable in quick mode only, so one payload size can be verified after its
      # rate changes without paying for a whole evidence matrix. Evidence mode
      # ignores the override, see below, because a variable that could quietly trim
      # the pre-registered set would let a published matrix drop the inconvenient
      # size. An override that drops ENFORCEMENT_GATE_PAYLOAD_BYTES still runs and
      # ends in VERDICT INVALID with check_bands.py naming the missing pair, which
      # is correct rather than a harness bug.
      PROMPT_SIZES="${PROMPT_SIZES:-150 ${ENFORCEMENT_GATE_PAYLOAD_BYTES}}"
      ;;
    evidence)
      STEADY_SECONDS=60
      REPETITIONS=5
      STREAM_REPETITIONS=3
      if [ -n "${PROMPT_SIZES:-}" ] && [ "${PROMPT_SIZES}" != "150 4096 32768" ]
      then
        fail "PROMPT_SIZES cannot be overridden in evidence mode, it was '${PROMPT_SIZES}' and a published run must cover the pre-registered set 150 4096 32768"
      fi
      PROMPT_SIZES="150 4096 32768"
      ;;
    *)
      fail "unknown RESULTS_MODE '${MODE}', use quick or evidence"
      ;;
  esac
  WARMUP_DURATION="${WARMUP_SECONDS}s"
  STEADY_START="${STEADY_START_SECONDS}s"
  STEADY_DURATION="${STEADY_SECONDS}s"

  if [ "${BENCH_MAX_VUS}" -ge 50 ]
  then
    fail "BENCH_MAX_VUS is ${BENCH_MAX_VUS}, which is not strictly below the 50 admission slots internal/budget/store.go allows per agent, so enforce cells would answer 429 instead of queueing"
  fi
}

# The component costs the enforcement figure annotates, measured on this host during
# this run, so the figure never carries stale constants from another machine.
#
# The logcost package is in the list because an enforced request writes three slog
# lines where a passthrough request writes one, and it is the one component of the
# enforce minus passthrough delta that no benchmark under internal/ measures. Its
# benchmark names all end in Line or RequestLines, so the pattern picks up the three
# call-site shapes and both composites without matching anything else.
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

# The bands are gates, not prose: a human reading figures would be exactly the
# failure mode pre-registration exists to prevent.
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

# Refuses to declare a run committable while it still contains anything identifying
# the operator or their machine. Compressed artifacts are decompressed first,
# otherwise they would evade both this and the repository's own content rules.
#
# The staged manifest is audited here rather than in the results directory, because
# it is written last and is the artifact most likely to carry a path. Auditing the
# directory only would exempt exactly the file whose every field is a captured
# command's output.
#
# The username comes from id -un, not from $USER: an unset or empty $USER would turn
# the alternation into an empty branch that matches every file, which fails safe
# but reports a cause that has nothing to do with the artifacts.
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

# The manifest is written into the work directory so audit_results can inspect it,
# then install_manifest moves it into place. It still lands last: a directory
# without one is visibly an aborted run and can never be mistaken for evidence.
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
    # The demanded rate is recorded per payload size, one line each, because
    # there is no single non-streaming rate to record and a reader comparing two
    # evidence directories has to see at a glance that a number was taken at a
    # different operating point. The per-cell achieved figures are in
    # achieved-rate.txt.
    local manifest_bytes
    for manifest_bytes in ${PROMPT_SIZES}
    do
      printf 'demanded_rate_nonstreaming_%sB_rps=%s\n' \
        "${manifest_bytes}" "$(rate_for_payload "${manifest_bytes}")"
    done
    # The direct payload cells run at sizes quick mode does not put in
    # PROMPT_SIZES, so their demands are recorded unconditionally.
    printf 'demanded_rate_direct_payload_4096B_rps=%s\n' "$(rate_for_payload 4096)"
    printf 'demanded_rate_direct_payload_32768B_rps=%s\n' "$(rate_for_payload 32768)"
    printf 'demanded_rate_streaming_150B_rps=%s\n' "${RATE_STREAM}"
    printf 'rate_shortfall_tolerance_percent=%s\n' "${RATE_SHORTFALL_TOLERANCE_PERCENT}"
    # Both integrity tolerances, recorded here as well as per cell, because they
    # are the rule the whole matrix was judged under and a reader comparing two
    # directories has to see whether one was held to a different standard.
    printf 'steady_drop_tolerance_basis_points=%s\n' "${STEADY_DROP_TOLERANCE_BASIS_POINTS}"
    printf 'steady_drop_tolerance_floor=%s\n' "${STEADY_DROP_TOLERANCE_FLOOR}"
    printf 'steady_failed_tolerance_basis_points=%s\n' "${STEADY_FAILED_TOLERANCE_BASIS_POINTS}"
    printf 'steady_failed_tolerance_floor=%s\n' "${STEADY_FAILED_TOLERANCE_FLOOR}"
    # The quiescence gate's own settings, for the same reason. The per-cell
    # readings it acted on are in machine-state.txt as cpu_idle_pct.
    printf 'host_cpu_idle_floor_percent=%s\n' "${HOST_IDLE_FLOOR_PERCENT}"
    printf 'host_cpu_idle_sampler=second-sample-of-top-l2-n0-s2\n'
    printf 'host_cpu_idle_startup_gate_enforced=%s\n' \
      "$([ "${MODE}" = "evidence" ] && printf 'refuses-on-one-reading-below-floor' || printf 'warns-below-floor')"
    # The mid-run outcome, so a reader of a published directory can see how close
    # the run came to the sustained-breach rules rather than only that it passed
    # them. A directory with a MANIFEST satisfied both by construction.
    printf 'host_cpu_idle_mid_run_readings=%s\n' "${MID_RUN_READING_COUNT}"
    printf 'host_cpu_idle_mid_run_breaches=%s\n' "${MID_RUN_BREACH_COUNT}"
    printf 'host_cpu_idle_consecutive_breach_limit=%s\n' "${CONSECUTIVE_BREACH_LIMIT}"
    printf 'host_cpu_idle_pervasive_breach_budget_percent=%s\n' "${PERVASIVE_BREACH_FRACTION_PERCENT}"
    printf 'aa_control_payload_bytes=%s\n' "${AA_CONTROL_PAYLOAD_BYTES}"
    printf 'enforcement_gate_payload_bytes=%s\n' "${ENFORCEMENT_GATE_PAYLOAD_BYTES}"
    printf 'warmup_duration=%s\n' "${WARMUP_DURATION}"
    printf 'steady_start=%s\n' "${STEADY_START}"
    printf 'steady_duration=%s\n' "${STEADY_DURATION}"
    printf 'repetitions=%s\n' "${REPETITIONS}"
    printf 'stream_repetitions=%s\n' "${STREAM_REPETITIONS}"
    printf 'prompt_sizes_bytes=%s\n' "${PROMPT_SIZES}"
    printf 'max_vus_all_cells=%s\n' "${BENCH_MAX_VUS}"
    printf 'budget_admission_slots_per_agent=50\n'
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

  # The contention ledger is created empty rather than on first breach, so that a
  # present-and-empty file is a positive statement that no reading breached, and
  # an absent file means the directory predates the marking. Those are different
  # facts and check_bands.py reports them differently.
  CONTENDED_CELLS_FILE="${RESULTS_DIR}/contended-cells.txt"
  {
    printf '# Cells measured while the host CPU idle reading was below the floor, or\n'
    printf '# while it could not be read at all. One line per BREACHING READING, so a\n'
    printf '# cell contended at both edges appears twice with different phases.\n'
    printf '# check_bands.py drops the REPETITION of every cell named here out of every\n'
    printf '# median it computes. Lines beginning with # are comments.\n'
    printf '# format: cell=<name> phase=<before|after> reason=<why> cpu_idle_pct=<value> cpu_idle_floor_pct=<floor>\n'
  } > "${CONTENDED_CELLS_FILE}"

  log "results directory ${RESULTS_DIR}"

  start_mock
  run_matrix
  stop_mock

  capture_microbench
  check_bands
  # The pervasive-contention budget runs after the bands so that bands.txt is
  # written either way, and before the manifest so that a run failing it leaves a
  # directory with no MANIFEST.
  enforce_quiescence_breach_budget
  stage_manifest
  audit_results
  install_manifest
  printf 'attempt 1 completed successfully %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> "${ATTEMPTS_FILE}"
  printf '%s\n' "${RESULTS_DIR}"
}

# A sourced copy defines the functions and runs nothing.
if [ "${LEVEE_BENCH_SOURCED}" = "no" ]
then
  main "$@"
else
  log "sourced as a library, no matrix will run and no results directory will be created, only the functions are defined"
fi
