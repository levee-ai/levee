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

  # The host quiescence gate, checked here so an evidence run on a busy machine is
  # refused in the first few seconds rather than after 50 minutes of measuring.
  # This costs one sample even in quick mode, deliberately: the warning is how an
  # operator learns what their machine reads before they attempt an evidence run.
  local startup_idle
  startup_idle="$(robust_cpu_idle_percent)"
  log "host CPU idle at startup is ${startup_idle} percent, floor is ${HOST_IDLE_FLOOR_PERCENT}"
  enforce_host_quiescence "startup" "${startup_idle}"
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
# empty path. BENCH_MAX_VUS is deliberately absent from this block: declaring
# it would run at load time and erase an environment override, and overriding it
# is how the concurrency-cap gate gets exercised.
RESULTS_DIR=""
ATTEMPTS_FILE=""
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
# PROMPT_SIZES is deliberately absent from this block for the same reason
# BENCH_MAX_VUS is: declaring it here would run at load time and erase an
# environment override. It is overridable in quick mode only, see configure_mode.

# RATE_SHORTFALL_TOLERANCE_PERCENT sizes the achieved-versus-demanded rate gate
# added on 2026-09-16. A cell that cannot serve its demanded arrival rate is
# reporting QUEUE RESIDENCE, not service time, and a latency number from it is
# a measurement of the harness rather than of levee.
#
# WHY 2 PERCENT. The honest envelope is far tighter than that and the gate is
# deliberately loose against it, because the failure it exists to catch is
# enormous. Measured, not assumed:
#
#   - A healthy cell delivers rate times duration plus one. Observed on this
#     host at 500 rps over a 20 second steady window: 10001 rows against 10000
#     demanded, which is a 0.01 percent OVERSHOOT. A k6 probe at 20 rps over 2
#     seconds delivered exactly 40 of 40.
#   - Window-boundary work in flight when the window closes is bounded by the
#     pool size times one service time. The worst cell in the matrix is 32768B
#     enforce, roughly 8.5ms of service time at 150 rps, so at most a couple of
#     requests of 9000 sit unattributed. That is 0.02 percent.
#
#   - The failure being caught is 51 percent. The evidence attempt that prompted
#     this gate demanded 500 rps at 32768B enforce and achieved about 256 rps.
#
# So 2 percent is roughly 100 times the legitimate envelope and roughly a
# twenty-fifth of the failure. It cannot fire on boundary effects and it cannot
# miss saturation. Tightening it toward the 0.02 percent envelope would start
# failing runs for single-iteration host stalls, which is the drift-gating
# mistake bands 1 and 5 were both amended to stop making.
RATE_SHORTFALL_TOLERANCE_PERCENT=2

# HOST_IDLE_FLOOR_PERCENT is the host quiescence gate, ADDED 2026-09-16 after the
# first completed 43-cell evidence run was invalidated by band 3. It is the single
# check that would have caught that run before it spent 50 minutes measuring a
# contended machine.
#
# WHAT IT REPLACES AS A SIGNAL. record_machine_state already captured loadavg, and
# loadavg is PROVEN not to discriminate here. The invalidated run sat at 4.0 to 6.4
# across its cells while the quiet re-measurements that produced the correct answer
# sat at 2.8 to 5.0. Those two ranges OVERLAP, and the numbers they produced were
# 108us apart, so no threshold on loadavg could have separated them. loadavg is
# still recorded below rather than deleted, because a field that demonstrably does
# not discriminate is worth keeping visible beside the one that does.
#
# WHAT THE GATE IS SIZED AGAINST. Two regimes, measured on this host with the exact
# sampler below, 26 readings each, pooled across 2 second and 3 second averaging
# windows and across the paired session described next:
#
#   regime                                 n    min     median   max
#   ambient, browser and agents resident   26   59.28   69.11   76.32
#   ambient plus three busy loops          26   41.87   51.56   58.77
#
# Three busy loops is not an arbitrary load. It is the exact condition the
# investigation used to reproduce the invalidated run: it moved the 150B
# enforcement shift from +15us to +93 and +109us, moved both arms' absolute P50
# onto the invalidated artifact's own values, and still achieved 500.0 rps with
# zero steady drops and zero failed requests. It would have passed every integrity
# gate this harness had before today.
#
# THE PAIRED MEASUREMENT is the one that proves the effect, and it was run that way
# on purpose. Eight same-moment pairs, one reading with the loops absent and one
# with them present a few seconds later, so ambient drift affects both arms of a
# pair equally. Every pair moved in the same direction, by a median of 16.12 points
# of idle and never less than 11.94.
#
# WHY 60. It sits above EVERY ONE of the 26 contended readings, the highest of
# which is 58.77, and below only 2 of the 26 ambient readings, 59.28 and 59.85,
# which are that distribution's low tail and which the median-of-three confirmation
# in robust_cpu_idle_percent removes. It is deliberately NOT the midpoint of the two
# ranges, which would be 59.0 on the pooled figures: the cost of the two errors is
# asymmetric, since a refused run costs one rerun while a contended run that passes
# publishes a wrong number as evidence.
#
# WHAT THE POOLED RANGES DO NOT SHOW, stated because they nearly touch. The lowest
# ambient reading and the highest contended reading are 0.51 points apart, which
# looks like no separation at all until you notice the ambient level itself drifted
# by 17 points across the sampling session. That is exactly why the paired form
# above is the load-bearing evidence and the pooled table is context.
#
# THE AMBIENT REGIME IS NOT A QUIET HOST. It carried a browser, a video-conferencing
# app, resident endpoint-security agents and several concurrent tool sessions. So it
# is an UPPER BOUND on how loaded a host may be and still pass rather than a picture
# of a prepared one, and a host actually prepared for an evidence run reads far
# higher with a correspondingly larger margin.
#
# WHAT IT CANNOT DO, said plainly so it is never mistaken for tight. The paired
# effect of the proven contended regime is 16 points of idle, so this gate resolves
# THAT regime and cannot resolve a milder one. One busy loop costs roughly a third
# as much and would pass. The gate is necessary and not sufficient, exactly like the
# A/A control it ships beside, and neither replaces reading the recorded idle
# figures in machine-state.txt when a number looks wrong.
#
# WHAT IT WAS NEVER ABLE TO CHECK, and this is the honest limit on the whole story.
# The invalidated run recorded no idle figure at all, because this field did not
# exist yet. So contention of this size is proven SUFFICIENT to produce that run's
# numbers and is NOT proven to be what that run actually had. This floor is
# calibrated against the reproduction, not against the failure.
HOST_IDLE_FLOOR_PERCENT=60

# AA_CONTROL_PAYLOAD_BYTES is the payload the A/A control pair runs at. It is the
# small payload deliberately: the control exists to state the estimator's noise
# floor beside the measurement whose signal is closest to that floor, and at 4096B
# the signal is 160 times the floor and needs no such statement.
AA_CONTROL_PAYLOAD_BYTES=150

# ENFORCEMENT_GATE_PAYLOAD_BYTES is the payload check_bands.py runs the PRIMARY
# enforcement gate at, relocated there on 2026-09-16. It is named here because
# quick mode has to include it in PROMPT_SIZES or the gate has no cells to read,
# see configure_mode.
ENFORCEMENT_GATE_PAYLOAD_BYTES=4096

# rate_for_payload maps a prompt size to the arrival rate every cell at that
# size runs at, derived from MEASURED capacity rather than from one global
# number.
#
# WHY THIS FUNCTION EXISTS. One global 500 rps was the defect that invalidated
# the first evidence attempt. Levee needs about 10.94ms of CPU per enforced
# 32768B request against 0.233ms for a passthrough one, a 47-fold spread, so no
# single rate can sit at a sane utilization in both arms at every payload size.
# At 32768B the enforce cell was demanded 500 rps against roughly 400 rps of
# capacity, dropped 14622 steady iterations, and reported a 152.4ms median that
# was queue residence rather than service time. Little's Law closes it exactly:
# 40 in flight over the 260 rps actually achieved is 154ms against 152.4ms
# observed. The honest service time there is 8.5ms.
#
# WHY THE RATE IS PER PAYLOAD SIZE RATHER THAN PER CELL. The published estimator
# is a quantile SHIFT, proxied minus direct at the same payload size, so the
# direct, passthrough and enforce arms at one size must share one rate or the
# shift compares two different operating points. overhead_figure.py enforces
# this independently: Group.rate raises when a group mixes rates, and a group is
# keyed on role, stream mode and payload size. So the binding constraint at each
# size is the SLOWEST arm at that size, which is always enforce.
#
# TARGET UTILIZATION is at most roughly 40 percent of measured capacity. Above
# that a queue forms and the cell reports residence time. Below it the cell
# reports service time, which is the quantity every published number claims to
# be, so sitting well under 40 percent is strictly better rather than wasteful.
# Two of the three sizes below are already far under it at their existing rate,
# and their rates are therefore left alone: raising a rate to hit 40 percent
# exactly would move a healthy cell toward saturation for nothing, and would
# also orphan every band calibration table in check_bands.py, all of which was
# measured at 500 rps.
rate_for_payload() {
  case "$1" in
    150)
      # 500 rps. Capacity at 150B enforce is not separately measured and is
      # bounded BELOW by the 4096B figure, because estimation cost is linear in
      # prompt bytes at about 120ns per byte and every other term is shared. So
      # utilization here is at most the 18 percent computed at 4096B. Measured
      # service time is 0.218ms enforce and 0.165ms passthrough at concurrency
      # 1, which puts single-worker capacity alone near 4587 rps.
      #
      # Unchanged deliberately. Every historical reading the bands are
      # calibrated against was taken at this rate and this size.
      printf '500\n'
      ;;
    4096)
      # 500 rps against a measured enforce capacity of 2706 rps, which is 18
      # percent utilization. Already less than half the 40 percent target, so
      # the cell reports service time and the rate is left unchanged.
      printf '500\n'
      ;;
    32768)
      # 150 rps against a measured enforce capacity of about 400 rps peak, which
      # is 37.5 percent utilization. This is the cell that failed.
      #
      # The peak is at concurrency 4 and throughput is RETROGRADE past it: 295
      # rps at concurrency 8 and 260 rps at concurrency 40. So the ceiling here
      # is not a number to be beaten with a bigger VU pool, a bigger pool makes
      # it WORSE, and 150 rps is chosen against the 400 rps peak rather than
      # against the 260 rps the failed cell actually achieved.
      #
      # Passthrough at this size has capacity above 7100 rps and therefore runs
      # at about 2 percent utilization. That asymmetry is deliberate and is the
      # price of a computable shift: both arms must share the rate, so the rate
      # belongs to the slower arm.
      printf '150\n'
      ;;
    *)
      fail "no measured capacity for a ${1}B prompt, measure it and add it to rate_for_payload before benchmarking that size"
      ;;
  esac
}

# min_steady_requests is the floor the k6 achieved-rate threshold enforces for
# one cell. Integer arithmetic throughout, because the shell has no floats and
# a floor is the correct rounding for a minimum.
min_steady_requests() {
  local rate="$1"
  local seconds="$2"
  printf '%s\n' "$(( rate * seconds * (100 - RATE_SHORTFALL_TOLERANCE_PERCENT) / 100 ))"
}

# sample_levee_cpu records levee's consumed CPU time at the two edges of the
# STEADY window, so CPU seconds per request falls out of the artifact instead of
# having to be inferred from a latency curve. Saturation then reads directly off
# a committed file: a cell whose per-request CPU cost approaches one core-second
# divided by its arrival rate is at the knee by arithmetic, no interpretation
# needed.
#
# The window is isolated by sleeping, not by bracketing the whole k6 invocation.
# Bracketing would fold the warmup scenario's CPU into the delta while the
# request count divides only the steady rows, which inflates the per-request
# figure by roughly the warmup-to-steady duration ratio. So this runs as a
# background job alongside k6, wakes at STEADY_START, reads, sleeps exactly the
# steady duration, and reads again.
#
# ps -o cputime reports the process's OWN accumulated user plus system time.
# Probed on this host: a short-lived child printed "  0:00.04" then "  0:00.12"
# two seconds apart, so the field is [DD-][HH:]MM:SS.ss with leading padding and
# hundredth-of-a-second resolution. That resolution is immaterial here, since the
# smallest delta in the matrix is a passthrough cell at roughly 2.3 CPU seconds
# over a 20 second window.
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

# cpu_delta_seconds prints the CPU seconds between two ps cputime readings, or
# "na" when either reading is absent.
#
# The parse folds colon-separated fields from the left at 60 each, so it handles
# MM:SS.ss and HH:MM:SS.ss without caring which it got, and adds any DD- prefix
# that a process older than a day would carry. levee never lives that long
# inside one cell, and the prefix is handled anyway so an unexpected format
# cannot silently parse to a wrong number.
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

# record_cpu_per_request turns one cell's CPU delta into the published figure,
# CPU milliseconds per steady request, and appends it to cpu-seconds.txt.
#
# The divisor is the STEADY request count from the cell summary, which is the
# same population the sampling window covers and the same population every
# published percentile comes from.
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

# record_achieved_rate is the validity signal that SUBSUMES the drop count. A
# cell can miss its demanded rate without k6 dropping a single iteration, if the
# VU pool is large enough to absorb the growing queue instead of running out of
# workers, and in that case the latency number is queue residence while every
# drop gate reads clean. Achieved throughput catches both shapes: whether the
# backlog drops or merely waits, the completions inside the window still fall
# short of the demand.
#
# The drop gate stays exactly as it is. It did its job correctly on the failed
# attempt by refusing to publish a queue-time number as a latency number.
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

  # The pool is now IDENTICAL in every cell rather than 50 to 100 for
  # passthrough and 40 to 40 for enforce.
  #
  # WHY THE ASYMMETRY HAD TO GO. The published estimator subtracts one arm's
  # quantile from another's. Under any queueing at all the pool size is part of
  # what is being measured, since the pool bounds how much backlog can form
  # before k6 drops instead of waiting, so two arms with different pools were not
  # comparable. The 40-slot cap is also exactly what turned the failed 32768B
  # enforce cell into a 152.4ms median: 40 in flight over 260 rps achieved is
  # 154ms of residence by Little's Law. A larger pool would not have fixed that
  # cell, it would have reported a LARGER number, and a smaller one a smaller
  # number, which is the clearest possible demonstration that a saturated cell
  # measures the pool rather than the proxy.
  #
  # WHY 40 AND NOT 100. internal/budget/store.go hardcodes DefaultStreamLimit at
  # 50 admission slots per agent, a slot is held until the deferred reconcile
  # runs after the response bytes have already reached k6, and an exhausted slot
  # answers 429 rather than queueing. A 429 fails the http_req_failed threshold
  # AND perturbs levee-side state, so every enforce pool must stay strictly under
  # 50. Equalizing therefore means bringing passthrough DOWN to 40, not enforce
  # up. That direction costs nothing: with per-payload rates now set from
  # measured capacity, the worst cell in the matrix needs about 1.3 concurrent
  # workers, so 40 is roughly 30 times the requirement.
  #
  # That headroom is MEASURED rather than asserted, and it is measured after the
  # fact because service time is not known before the cell runs. check_bands.py
  # prints a vus_need column, Little's Law on each cell's own measured P50, and
  # the pool divided by it, in the COST table of every bands.txt.
  #
  # The pool is fully preallocated. k6 validates preAllocatedVUs against maxVUs
  # and exits 104 on "maxVUs can't be less than preAllocatedVUs", and a pool that
  # grows mid-run would initialize VUs inside the steady window.
  local preallocated_vus="${max_vus}"

  local steady_seconds="${STEADY_SECONDS}"
  local minimum_requests
  minimum_requests="$(min_steady_requests "${rate}" "${steady_seconds}")"

  log "cell ${cell}: rate ${rate}, stream ${stream}, prompt ${prompt_bytes}B, vus ${preallocated_vus} to ${max_vus}, minimum steady requests ${minimum_requests}"
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
  PROMPT_BYTES="${prompt_bytes}" \
  STREAM="${stream}" \
    k6 run --out "csv=${raw}" "${BENCH_DIR}/k6/overhead.js" >&2 || k6_status=$?

  wait "${cpu_sampler_pid}" 2>/dev/null || true

  record_machine_state "${cell}" "after"
  printf '%s k6_exit=%s\n' "${cell}" "${k6_status}" >> "${RESULTS_DIR}/k6-exit-codes.txt"
  record_dropped_iterations "${cell}" "${summary}"
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

# sample_cpu_idle_percent prints one system-wide CPU idle percentage, or "na"
# when it cannot be read. An unreadable sensor is never reported as a passing
# number, because the gate below treats an unknown host as a refused host.
#
# THE SOURCE, verified on this host rather than assumed. top is the suggested
# source and its CPU line parses cleanly:
#
#   $ top -l 1 -n 0 | grep '^CPU usage'
#   CPU usage: 15.95% user, 18.9% sys, 65.95% idle
#
# So the parse is: split on commas, take the field containing "idle", strip
# everything that is not a digit or a dot. The percentage is printed with one or
# two decimals depending on the value, which is why the strip is a character class
# rather than a fixed-width cut.
#
# WHY NOT top -l 1. A single -l 1 sample DOES respond to load, which was worth
# checking because the first sample of some top implementations reports an average
# since boot and would have been useless as a per-cell gate. Probed here: five
# -l 1 samples read 60.96, 41.86, 59.75, 57.51 and 58.16 with the machine
# untouched, then 34.32, 43.10, 44.26, 41.66 and 42.63 with three busy loops
# added. It tracks load, and it is NOISY: that 41.86 arrived with no load added.
# The gate reads the SECOND sample of top -l 2 -n 0 -s 2 instead, which is a true
# 2 second interval average rather than whatever window top's startup happens to
# cover, and whose spread over 10 readings was 59.85 to 72.76 against the 41.86 to
# 65.50 of the instantaneous form. It costs about 2.5 seconds per sample.
#
# top -l 2 prints TWO "CPU usage" lines, and the awk below keeps the last one,
# which is the interval sample. Keeping the first would silently reintroduce the
# noisy startup reading this function exists to avoid.
sample_cpu_idle_percent() {
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

# idle_below_floor prints yes or no for one reading against the floor. The
# comparison lives in awk because the shell has no floats and the readings carry
# two decimals.
idle_below_floor() {
  awk -v value="$1" -v floor="${HOST_IDLE_FLOOR_PERCENT}" 'BEGIN {
    print (value + 0 < floor + 0) ? "yes" : "no"
  }'
}

# robust_cpu_idle_percent is the reading the gate acts on. It takes one sample,
# and RE-SAMPLES TWICE MORE ONLY WHEN THAT SAMPLE FALLS BELOW THE FLOOR, returning
# the median of the three.
#
# WHY THE CONFIRMATION EXISTS. An evidence run makes 106 of these checks. The
# ambient regime's single-sample minimum was 59.28 against a floor of 60, so one
# isolated dip in a hundred readings is entirely expected, and a gate that aborts a
# 50 minute run on one such dip would be abandoned within a week. The re-sample
# costs 5 extra seconds and only on the readings that are about to refuse a run.
#
# WHY IT CANNOT WEAKEN THE GATE. The confirmation is two more real measurements of
# the same quantity, not a retry until success. A host with sustained contention
# reads below the floor on all three, since the contended regime's single-sample
# MAXIMUM was 53.62 against the floor of 60, so its median is below the floor too.
# Only a transient can be voted out by this, which is the whole point.
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

# enforce_host_quiescence is the gate. In EVIDENCE mode a host below the floor
# refuses the run, before the first cell and again at both edges of every cell
# after it, so a machine that becomes busy mid-matrix stops the run where it
# happened rather than producing 40 more cells of numbers nobody can use. In quick
# mode it warns and continues, because quick mode is a disposable local check whose
# job is to exercise the code path rather than to publish a number.
#
# An UNREADABLE reading refuses an evidence run as well. A gate whose sensor is
# broken has to fail closed, or the first sw_vers-style tool change turns the whole
# check into a silent pass that still prints reassuring text.
enforce_host_quiescence() {
  local where="$1"
  local idle="$2"

  if [ "${idle}" = "na" ]
  then
    if [ "${MODE}" = "evidence" ]
    then
      fail "host CPU idle could not be read at ${where}, so host quiescence is UNKNOWN. An evidence run refuses an unreadable gate rather than treating it as a pass, because the contention this gate exists to catch is invisible to every other check in the harness"
    fi
    log "warning: host CPU idle could not be read at ${where}, quick mode continues, an evidence run would refuse here"
    return 0
  fi

  if [ "$(idle_below_floor "${idle}")" = "no" ]
  then
    return 0
  fi

  if [ "${MODE}" = "evidence" ]
  then
    fail "host CPU idle is ${idle} percent at ${where}, below the ${HOST_IDLE_FLOOR_PERCENT} percent floor an evidence run requires. The machine is not quiet enough to measure on. Contention of roughly this size was proven SUFFICIENT to move the 150B enforcement shift from +15us to over +100us while every integrity gate still read clean, which is what invalidated the first completed 43-cell run. Quit the browser, the video-conferencing app and any background build, wait for the endpoint-security agents and the file indexer to settle, and start again"
  fi
  log "warning: host CPU idle is ${idle} percent at ${where}, below the ${HOST_IDLE_FLOOR_PERCENT} percent floor, quick mode records it and continues, an evidence run would refuse here"
}

# record_machine_state captures the environment conditions that bound the
# drift story. A cell polluted by a background spike or a power-source change
# is otherwise indistinguishable from a real regression.
#
# cpu_idle_pct is the GATING field, added 2026-09-16, and it is sampled at both
# edges of every cell. It is read while k6 is not running, so it measures the
# AMBIENT host rather than the benchmark's own load, which is the quantity the
# floor is about.
#
# The thermal reading is a placeholder when pmset has nothing to report, which
# is the normal case on Apple Silicon: pmset -g therm answers "No CPU power
# status has been recorded" and never prints the CPU_Speed_Limit line that Intel
# hosts do. An empty value would read as a broken capture rather than an absent
# one.
record_machine_state() {
  local cell="$1"
  local phase="$2"
  local thermal idle
  thermal="$(pmset -g therm 2>/dev/null | sed -n 's/.*CPU_Speed_Limit *= *\([0-9]*\).*/\1/p' | head -1)"
  idle="$(robust_cpu_idle_percent)"
  {
    printf 'cell=%s phase=%s ' "${cell}" "${phase}"
    printf 'cpu_idle_pct=%s ' "${idle}"
    printf 'cpu_idle_floor_pct=%s ' "${HOST_IDLE_FLOOR_PERCENT}"
    # loadavg is kept and is NOT a gate. It is recorded precisely because it was
    # proven not to discriminate between the invalidated run and the quiet
    # re-measurements, so a reader can see the two fields disagree rather than
    # having to take that finding on trust.
    printf 'loadavg=%s ' "$(sysctl -n vm.loadavg | tr -d '{}' | tr -s ' ' '_')"
    printf 'thermal=%s ' "${thermal:-none-reported}"
    printf 'power=%s ' "$(pmset -g ps | head -1 | tr ' ' '_')"
    printf 'timewait=%s\n' "$(count_timewait)"
  } >> "${RESULTS_DIR}/machine-state.txt"
  enforce_host_quiescence "cell ${cell} phase ${phase}" "${idle}"
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
# AMENDED 2026-09-16 a second time, for the arrival rate and the VU pool. Every
# cell's rate now comes from rate_for_payload, so the 32768B cells run at 150 rps
# where the rest still run at 500, and every cell shares ONE 40-slot VU pool
# where passthrough and direct previously ran 50 to 100 against enforce's 40 to
# 40. Both reasons are argued at rate_for_payload and inside run_cell.
#
# AMENDED 2026-09-16 a third time, for the A/A CONTROL PAIR. Two extra cells per
# repetition at the small payload, both running the PASSTHROUGH config, so their
# repetition-matched P50 shift has a KNOWN TRUE VALUE OF ZERO. check_bands.py prints
# it beside the enforcement numbers.
#
# WHY IT IS HERE. Band 3 published a 15us enforcement signal without ever measuring
# what its own estimator reads when the answer is zero. That is the missing number:
# a reader looking at a +123us reading had no way to tell how much of it the
# estimator could invent. Measured on a quiet host the A/A shift is -1, +4 and 0us,
# so the noise floor is about 4us and a 15us signal really is above it.
#
# WHY IT IS NOT A GATE. A CONTENDED A/A pair still read 13us, which is a true zero
# reported as 13us, so a passing control does not certify a quiet host. It is
# necessary and not sufficient. The gate against contention is the host quiescence
# floor at the top of this file.
#
# WHY IT RESTARTS LEVEE BETWEEN THE TWO ARMS. The pair it calibrates does, and a
# control that skipped the restart would measure a different estimator. Everything
# else is identical too: the same payload, the same rate, the same VU pool, the same
# TIME_WAIT drain, the same adjacency in the matrix.
#
# WHY ONCE PER REPETITION rather than once per run. The published quantity is the
# MEDIAN of the per-repetition shifts, so the noise floor that matters is the noise
# floor of that median, not of a single pair. Matching REPETITIONS exactly is what
# makes the control the same estimator applied to a known zero. It costs 2 cells per
# repetition, 2 in quick mode and 10 in evidence mode.
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

      # The A/A control, passthrough against passthrough, argued above. Placed
      # immediately after the pair it calibrates so it sees the same host
      # conditions, and only at the small payload, which is the size whose signal
      # is small enough to need a noise floor stated beside it.
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

# configure_mode sets every knob that differs between a fast local check and a
# publishable run.
#
# Non-streaming arrival rates are NOT set here any more. They belong to
# rate_for_payload, because there is no longer one of them: each payload size
# carries its own rate derived from that size's measured capacity.
#
# BENCH_MAX_VUS is a variable rather than a literal so the concurrency-cap gate
# can be exercised deliberately, and it governs EVERY cell rather than the
# enforce cells alone, which is what makes the arms comparable. 40 keeps it
# strictly below the 50 admission slots internal/budget/store.go hardcodes per
# agent, so a tail pileup surfaces as dropped iterations rather than as a 429
# that also leaves levee-side state altered.
#
# Durations are held as integer SECONDS and the k6 duration strings are derived
# from them, because the CPU sampler and the achieved-rate arithmetic both need
# the number and neither should be parsing a "20s" string back apart.
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
      # Overridable in QUICK MODE ONLY, so one payload size can be verified on
      # its own after its rate changes without paying for a whole evidence
      # matrix. Verifying that the 32768B cells now serve their demanded rate
      # otherwise costs the full 43-cell run.
      #
      # Evidence mode deliberately IGNORES the override, see below. A published
      # run must cover the whole pre-registered payload set, and an environment
      # variable that could quietly trim it would let a published matrix drop the
      # size that was inconvenient.
      #
      # THE DEFAULT GAINED 4096 on 2026-09-16, when the primary enforcement gate
      # moved to that size. Quick mode used to run 150B alone, which after the
      # relocation would leave the one gate that decides whether enforcement cost
      # is sane with no cells to read. A local check that cannot exercise the
      # primary gate is a local check nobody should trust, so quick mode pays two
      # more cells, about 70 seconds, to run it.
      #
      # An override that DROPS 4096 still runs. It ends in VERDICT INVALID, from
      # check_bands.py naming the missing pair, and that is the correct outcome
      # rather than a harness bug: a payload-restricted run is a capacity check and
      # is not publishable evidence by construction.
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
    # The DEMANDED rate is recorded per payload size, one line each, because
    # there is no longer a single non-streaming rate to record. A reader
    # comparing two evidence directories has to be able to see at a glance that
    # a number was taken at a different operating point, and a single
    # rate_nonstreaming_rps field would have hidden exactly that.
    #
    # The rate is per payload size rather than per cell because the published
    # estimator subtracts arms at one size, so all arms at a size share one rate
    # by construction. The per-cell ACHIEVED figures, against these demands, are
    # in achieved-rate.txt.
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
    # The quiescence gate's own settings, recorded so a reader of two directories
    # can see whether they were held to the same floor. The per-cell readings it
    # acted on are in machine-state.txt as cpu_idle_pct.
    printf 'host_cpu_idle_floor_percent=%s\n' "${HOST_IDLE_FLOOR_PERCENT}"
    printf 'host_cpu_idle_sampler=second-sample-of-top-l2-n0-s2\n'
    printf 'host_cpu_idle_gate_enforced=%s\n' \
      "$([ "${MODE}" = "evidence" ] && printf 'refuses-below-floor' || printf 'warns-below-floor')"
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
