// Open-loop load script for the Levee overhead benchmark.
//
// One invocation measures one cell. run.sh supplies every parameter through the
// environment. The executor is constant-arrival-rate, which starts iterations on
// a fixed schedule regardless of whether earlier ones finished, so a saturated
// system reports dropped iterations instead of silently stretching its own
// inter-arrival times. How the integrity tolerances below are calibrated is in
// benchmarks/methodology/calibration.md.
import http from 'k6/http';

const TARGET_URL = __ENV.TARGET_URL;
const SUMMARY_PATH = __ENV.SUMMARY_PATH;
const CELL = __ENV.CELL;
const RATE = Number(__ENV.RATE);
const PREALLOCATED_VUS = Number(__ENV.PREALLOCATED_VUS);
const MAX_VUS = Number(__ENV.MAX_VUS);
const WARMUP_DURATION = __ENV.WARMUP_DURATION;
const STEADY_START = __ENV.STEADY_START;
const STEADY_DURATION = __ENV.STEADY_DURATION;
const STEADY_SECONDS = Number(__ENV.STEADY_SECONDS);
const MIN_STEADY_REQUESTS = Number(__ENV.MIN_STEADY_REQUESTS);
// run.sh derives all three tolerances from this cell's own demanded iteration
// count, so no single constant holds every cell to one standard whatever its
// volume.
const DEMANDED_STEADY_REQUESTS = Number(__ENV.DEMANDED_STEADY_REQUESTS);
const MAX_STEADY_DROPPED_ITERATIONS = Number(__ENV.MAX_STEADY_DROPPED_ITERATIONS);
const MAX_STEADY_FAILED_REQUESTS = Number(__ENV.MAX_STEADY_FAILED_REQUESTS);
const PROMPT_BYTES = Number(__ENV.PROMPT_BYTES);
const STREAM = __ENV.STREAM === 'true';

// http_req_failed is a Rate metric, and a k6 threshold on a Rate can express only
// a rate, so an allowed count has to be divided by a population to become an
// expressible ceiling. The population is the demanded steady iteration count, not
// the achieved one: a cell that also drops iterations has a smaller achieved
// count, which would make the same number of failures read as a higher rate.
// Probed at the pinned k6 v2.2.0, six forced steady failures in a 100-iteration
// window reported rate 0.06, ok false against rate<=0.05 and ok true at 0.06.
const MAX_STEADY_FAILED_RATE =
  DEMANDED_STEADY_REQUESTS > 0 ? MAX_STEADY_FAILED_REQUESTS / DEMANDED_STEADY_REQUESTS : 0;

// buildPrompt returns exactly n bytes of deterministic ASCII filler.
function buildPrompt(n) {
  const unit = 'levee benchmark filler text ';
  let out = '';
  while (out.length < n) {
    out += unit;
  }
  return out.slice(0, n);
}

const requestBody = JSON.stringify({
  // Fixture model id, so the pricing table resolves it by longest prefix and the
  // dollar budget accrues real rates rather than the unknown-model maximum.
  model: 'gpt-4o-mini-2024-07-18',
  max_tokens: 16,
  stream: STREAM,
  messages: [{ role: 'user', content: buildPrompt(PROMPT_BYTES) }],
});

const requestParams = {
  headers: {
    'Content-Type': 'application/json',
    'X-Levee-Agent': 'bench',
    // The mock ignores credentials and no real key is used anywhere in this
    // harness. The header exists so the request byte size matches a real agent
    // request.
    Authorization: 'Bearer benchmark-placeholder-not-a-real-key',
  },
};

const scenarioTemplate = {
  executor: 'constant-arrival-rate',
  rate: RATE,
  timeUnit: '1s',
  preAllocatedVUs: PREALLOCATED_VUS,
  maxVUs: MAX_VUS,
};

export const options = {
  // The socket is still drained to EOF, so streaming durations stay correct,
  // but no body is retained.
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(50)', 'p(90)', 'p(99)', 'p(99.9)'],
  scenarios: {
    warmup: {
      ...scenarioTemplate,
      startTime: '0s',
      duration: WARMUP_DURATION,
    },
    steady: {
      ...scenarioTemplate,
      startTime: STEADY_START,
      duration: STEADY_DURATION,
    },
  },
  thresholds: {
    // All three thresholds are scoped to the steady scenario. The committed
    // artifact is steady-scenario rows only, so a gate has to cover exactly the
    // window whose numbers get published.
    //
    // Scoping the drop threshold is load bearing, so do not tighten it back to a
    // bare dropped_iterations. Levee's first enforced request costs about 130ms,
    // dominated by building the o200k_base encoder, against about 4ms once warm.
    // At 500 rps the enforce cells' 40-VU ceiling is entirely blocked for that
    // first 130ms while arrivals keep coming, so k6 drops roughly 25 iterations
    // inside the first 52 milliseconds of warmup. That is deterministic rather
    // than flaky, and unscoped it fails every enforce cell of every run while the
    // steady window is pristine. Warmup drops are recorded separately below, so
    // absorbing cold start here does not hide it.
    'dropped_iterations{scenario:steady}': ['count<=' + MAX_STEADY_DROPPED_ITERATIONS],
    'http_req_failed{scenario:steady}': ['rate<=' + MAX_STEADY_FAILED_RATE],
    // The achieved-throughput floor catches saturation the drop count can miss.
    // Given a VU pool wide enough to hold a growing backlog, a saturated cell
    // drops nothing while its completions still fall short of its demand, and its
    // latency number is then queue residence published as service time.
    'http_reqs{scenario:steady}': ['count>=' + MIN_STEADY_REQUESTS],
  },
};

export default function () {
  http.post(TARGET_URL, requestBody, requestParams);
}

// handleSummary writes the per-cell summary, including the integrity outcomes, so
// a reader of a committed directory never has to trust that the harness enforced
// its own gates. Defining it replaces the default console summary, so a short
// stdout line is returned too.
export function handleSummary(data) {
  const duration = data.metrics.http_req_duration || {};
  const waiting = data.metrics.http_req_waiting || {};
  const dropped = data.metrics.dropped_iterations || {};
  // Defining a threshold on a tagged sub-metric makes k6 materialize that
  // sub-metric in the summary, so scoping the drop threshold to steady also
  // buys the per-scenario breakdown below at no cost.
  const droppedSteady = data.metrics['dropped_iterations{scenario:steady}'] || {};
  const requestsSteady = data.metrics['http_reqs{scenario:steady}'] || {};
  const failed = data.metrics.http_req_failed || {};
  const failedSteady = data.metrics['http_req_failed{scenario:steady}'] || {};

  // Achieved rate is derived from the steady sub-metric over the steady duration.
  // The k6-reported rate on that sub-metric is not used: it is computed over k6's
  // own view of whole-run elapsed time rather than over the steady window, so it
  // reads low by roughly the warmup and gap duration even on a healthy cell.
  const steadyRequests = (requestsSteady.values && requestsSteady.values.count) || 0;
  const achievedRate = STEADY_SECONDS > 0 ? steadyRequests / STEADY_SECONDS : 0;

  const thresholdOutcomes = {};
  for (const [metricName, metric] of Object.entries(data.metrics)) {
    if (metric.thresholds) {
      for (const [expression, outcome] of Object.entries(metric.thresholds)) {
        thresholdOutcomes[metricName + ':' + expression] = outcome.ok === true ? 'pass' : 'fail';
      }
    }
  }

  const summary = {
    cell: CELL,
    target_url: TARGET_URL,
    rate: RATE,
    stream: STREAM,
    prompt_bytes: PROMPT_BYTES,
    warmup_duration: WARMUP_DURATION,
    steady_duration: STEADY_DURATION,
    steady_seconds: STEADY_SECONDS,
    preallocated_vus: PREALLOCATED_VUS,
    max_vus: MAX_VUS,
    demanded_rate: RATE,
    steady_request_count: steadyRequests,
    achieved_rate_steady: achievedRate,
    achieved_rate_fraction_of_demanded: RATE > 0 ? achievedRate / RATE : 0,
    min_steady_requests: MIN_STEADY_REQUESTS,
    // These two aggregates cover the whole invocation including warmup. The
    // committed per-request CSV is steady-only, and the plot scripts recompute
    // steady percentiles from that instead.
    http_req_duration: duration.values || {},
    http_req_waiting: waiting.values || {},
    request_count: (data.metrics.http_reqs && data.metrics.http_reqs.values.count) || 0,
    iteration_count: (data.metrics.iterations && data.metrics.iterations.values.count) || 0,
    demanded_steady_requests: DEMANDED_STEADY_REQUESTS,
    max_steady_dropped_iterations: MAX_STEADY_DROPPED_ITERATIONS,
    max_steady_failed_requests: MAX_STEADY_FAILED_REQUESTS,
    max_steady_failed_rate: MAX_STEADY_FAILED_RATE,
    // Only the steady count gates the run. The warmup figure is recorded so a
    // nonzero total cannot later hide a real steady-window problem.
    dropped_iterations: (dropped.values && dropped.values.count) || 0,
    dropped_iterations_steady: (droppedSteady.values && droppedSteady.values.count) || 0,
    dropped_iterations_warmup:
      ((dropped.values && dropped.values.count) || 0) -
      ((droppedSteady.values && droppedSteady.values.count) || 0),
    // http_req_failed is a Rate metric whose true observations mean "this request
    // failed", and k6 files true observations under passes and false ones under
    // fails. So passes is the failed-request count and fails is the successful
    // one, the opposite of what the field names suggest. Probed at k6 v2.2.0: 201
    // responses of 404 reported rate 1, passes 201, fails 0, and the tagged
    // sub-metric behaves identically.
    http_req_failed_rate: (failed.values && failed.values.rate) || 0,
    http_req_failed_count: (failed.values && failed.values.passes) || 0,
    http_req_failed_steady_rate: (failedSteady.values && failedSteady.values.rate) || 0,
    http_req_failed_steady_count: (failedSteady.values && failedSteady.values.passes) || 0,
    http_req_failed_warmup_count:
      ((failed.values && failed.values.passes) || 0) -
      ((failedSteady.values && failedSteady.values.passes) || 0),
    thresholds: thresholdOutcomes,
  };

  const p99 = (duration.values && duration.values['p(99)']) || 0;
  const out = {};
  out[SUMMARY_PATH] = JSON.stringify(summary, null, 2);
  out.stdout = 'cell ' + CELL + ': ' + summary.request_count + ' requests, p99 ' +
    p99.toFixed(3) + 'ms, dropped ' + summary.dropped_iterations_steady + ' steady of ' +
    MAX_STEADY_DROPPED_ITERATIONS + ' allowed and ' +
    summary.dropped_iterations_warmup + ' warmup, failed ' +
    summary.http_req_failed_steady_count + ' steady of ' + MAX_STEADY_FAILED_REQUESTS +
    ' allowed, achieved ' +
    achievedRate.toFixed(1) + ' of ' + RATE + ' rps demanded\n';
  return out;
}
