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
