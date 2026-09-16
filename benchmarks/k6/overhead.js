// Open-loop load script for the Levee overhead benchmark.
//
// One invocation measures one cell. run.sh supplies every parameter through
// the environment. The executor is constant-arrival-rate, which starts
// iterations on a fixed schedule regardless of whether earlier ones
// finished, so a saturated system reports dropped iterations instead of
// silently stretching its own inter-arrival times. The dropped_iterations
// threshold below turns any such violation into a failed run rather than a
// quiet caveat in the writeup.
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
const PROMPT_BYTES = Number(__ENV.PROMPT_BYTES);
const STREAM = __ENV.STREAM === 'true';

// buildPrompt returns exactly n bytes of deterministic ASCII filler. Prompt
// size is a controlled variable because the enforcement path tokenizes the
// whole prompt and that cost grows linearly with size, so a single small
// prompt would measure the cheapest possible case and imply a claim that
// does not hold at realistic context sizes.
function buildPrompt(n) {
  const unit = 'levee benchmark filler text ';
  let out = '';
  while (out.length < n) {
    out += unit;
  }
  return out.slice(0, n);
}

const requestBody = JSON.stringify({
  // Fixture model id, so the pricing table resolves it by longest prefix and
  // the dollar budget accrues real rates rather than the unknown-model
  // maximum.
  model: 'gpt-4o-mini-2024-07-18',
  max_tokens: 16,
  stream: STREAM,
  messages: [{ role: 'user', content: buildPrompt(PROMPT_BYTES) }],
});

const requestParams = {
  headers: {
    'Content-Type': 'application/json',
    'X-Levee-Agent': 'bench',
    // The mock ignores credentials and no real key is ever used anywhere in
    // this harness. The header exists so the request byte size matches a
    // real agent request.
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
    // Both integrity thresholds are scoped to the steady scenario. The
    // committed artifact is steady-scenario rows only, so a gate has to cover
    // exactly the window whose numbers get published, no more and no less.
    //
    // Scoping the DROP threshold is load bearing, so do not "tighten" it back
    // to a bare dropped_iterations. Levee's first enforced request costs about
    // 130ms, dominated by building the o200k_base encoder, against about 4ms
    // once warm. At 500 rps the enforce cells' 40-VU ceiling is entirely
    // blocked for that first 130ms while arrivals keep coming, so k6 drops
    // roughly 25 iterations inside the first 52 milliseconds of warmup. That is
    // deterministic, not flaky. Unscoped, it fails every enforce cell of every
    // run while the steady window is pristine, measured in the same run at zero
    // steady drops and a 4.341ms steady maximum. Absorbing cold start is
    // precisely what the warmup scenario exists for.
    'dropped_iterations{scenario:steady}': ['count==0'],
    'http_req_failed{scenario:steady}': ['rate==0'],
    // The achieved-rate gate, ADDED 2026-09-16. It is stronger and more general
    // than the drop count and it SUBSUMES it as a validity signal.
    //
    // A dropped iteration means the VU pool ran out of workers. That is one
    // symptom of a cell demanding more than capacity, not the condition itself.
    // With a pool large enough to absorb the growing backlog, a saturated cell
    // drops NOTHING while its completions inside the window still fall short of
    // its demand, and its latency number is then queue residence published as
    // service time. Throughput catches the condition directly: whether the
    // backlog drops or merely waits, the work finished in the window is short.
    //
    // The failed evidence attempt makes the difference concrete. Its 32768B
    // enforce cell demanded 500 rps, achieved about 256, and reported a 152.4ms
    // median that Little's Law attributes entirely to 40 requests waiting. It
    // was caught by the drop count only because the 40-slot pool was too small
    // to hold the backlog. Widen the pool and the same cell publishes an even
    // larger number with every drop gate clean.
    //
    // run.sh computes the floor as rate times steady seconds less its tolerance,
    // so the expression carries the real demand rather than a fixed number, and
    // the drop threshold above is left exactly as it was because it did its job.
    //
    // Probed at the pinned k6: a steady window demanding 40 requests reported
    // count 40 with this threshold ok true and exit 0, and the same window
    // against a floor of 999 reported ok false and exit 99. So the gate fires,
    // and defining it also materializes the tagged sub-metric the summary below
    // reads.
    'http_reqs{scenario:steady}': ['count>=' + MIN_STEADY_REQUESTS],
  },
};

export default function () {
  http.post(TARGET_URL, requestBody, requestParams);
}

// handleSummary writes the per-cell summary, including the integrity
// outcomes, so a reader of a committed results directory never has to trust
// that the harness enforced its own gates. Defining this function replaces
// the default console summary, so a short stdout line is returned too.
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

  // Achieved rate is derived from the STEADY sub-metric over the steady
  // duration, not from the whole-invocation http_reqs count, because the warmup
  // scenario contributes requests at the same rate over a different duration and
  // mixing them would dilute exactly the shortfall this number exists to expose.
  //
  // The k6-reported rate on the sub-metric is not used. It is computed over k6's
  // own view of elapsed time for the whole run rather than over the steady
  // window, so it reads low by roughly the warmup and gap duration even on a
  // perfectly healthy cell.
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
    // The achieved-versus-demanded record. Every cell carries it whether it
    // passed or failed, so a reader of a committed directory can see the
    // operating point each latency number was taken at rather than trusting that
    // the demanded rate was the rate actually served.
    demanded_rate: RATE,
    steady_request_count: steadyRequests,
    achieved_rate_steady: achievedRate,
    achieved_rate_fraction_of_demanded: RATE > 0 ? achievedRate / RATE : 0,
    min_steady_requests: MIN_STEADY_REQUESTS,
    // These aggregates cover the whole invocation including warmup. The
    // committed per-request CSV is filtered to the steady scenario and the
    // plot scripts recompute steady-only percentiles from it. These values
    // are for a quick eyeball and for the mechanical band check, which
    // compares like against like across cells.
    http_req_duration: duration.values || {},
    http_req_waiting: waiting.values || {},
    request_count: (data.metrics.http_reqs && data.metrics.http_reqs.values.count) || 0,
    iteration_count: (data.metrics.iterations && data.metrics.iterations.values.count) || 0,
    // Dropped iterations are reported three ways on purpose. Only the steady
    // count gates the run, but a warmup-only drop must stay VISIBLE rather than
    // silently tolerated, otherwise a real steady-window problem could later
    // hide behind an unexplained nonzero total. A nonzero total with a passing
    // threshold means every drop landed in warmup, which is cold start being
    // absorbed as designed. A nonzero steady count fails the run.
    dropped_iterations: (dropped.values && dropped.values.count) || 0,
    dropped_iterations_steady: (droppedSteady.values && droppedSteady.values.count) || 0,
    dropped_iterations_warmup:
      ((dropped.values && dropped.values.count) || 0) -
      ((droppedSteady.values && droppedSteady.values.count) || 0),
    http_req_failed_rate: (failed.values && failed.values.rate) || 0,
    // http_req_failed is a Rate metric whose true observations mean "this
    // request failed", and k6 files true observations under passes and false
    // ones under fails. So passes is the failed-request count and fails is
    // the successful-request count, the opposite of what the field names
    // suggest. Probed at k6 v2.2.0: 201 responses of 404 reported
    // rate 1, passes 201, fails 0.
    http_req_failed_count: (failed.values && failed.values.passes) || 0,
    thresholds: thresholdOutcomes,
  };

  const p99 = (duration.values && duration.values['p(99)']) || 0;
  const out = {};
  out[SUMMARY_PATH] = JSON.stringify(summary, null, 2);
  out.stdout = 'cell ' + CELL + ': ' + summary.request_count + ' requests, p99 ' +
    p99.toFixed(3) + 'ms, dropped ' + summary.dropped_iterations_steady + ' steady and ' +
    summary.dropped_iterations_warmup + ' warmup, achieved ' +
    achievedRate.toFixed(1) + ' of ' + RATE + ' rps demanded\n';
  return out;
}
