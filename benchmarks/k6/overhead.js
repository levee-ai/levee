// Open-loop load script for the Levee overhead benchmark.
//
// One invocation measures one cell. run.sh supplies every parameter through
// the environment. The executor is constant-arrival-rate, which starts
// iterations on a fixed schedule regardless of whether earlier ones
// finished, so a saturated system reports dropped iterations instead of
// silently stretching its own inter-arrival times. The dropped_iterations
// threshold below turns a MATERIAL violation into a failed run rather than a
// quiet caveat in the writeup, where material means past a tolerance run.sh
// derives from the cell's own demanded iteration count.
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
// The three integrity tolerances, all computed by run.sh from this cell's own
// demanded iteration count so the threshold expressions below carry real numbers
// rather than constants that would hold every cell to one standard whatever its
// volume. See the amendment block above the thresholds.
const DEMANDED_STEADY_REQUESTS = Number(__ENV.DEMANDED_STEADY_REQUESTS);
const MAX_STEADY_DROPPED_ITERATIONS = Number(__ENV.MAX_STEADY_DROPPED_ITERATIONS);
const MAX_STEADY_FAILED_REQUESTS = Number(__ENV.MAX_STEADY_FAILED_REQUESTS);
const PROMPT_BYTES = Number(__ENV.PROMPT_BYTES);
const STREAM = __ENV.STREAM === 'true';

// http_req_failed is a Rate metric, and k6 thresholds on a Rate expose the rate
// alone rather than a count, so an allowed COUNT has to be divided by a population
// to become an expressible ceiling. The population used is the DEMANDED steady
// iteration count rather than the achieved one, for two reasons. It is known before
// the run rather than after it, and when a cell also drops iterations its achieved
// count is smaller, which makes the same number of failures read as a HIGHER rate.
// So this denominator makes the effective allowance slightly stricter on a cell that
// is already in trouble, which is the correct direction for a validity gate.
//
// Probed at the pinned k6 v2.2.0. Six forced steady failures in a 100-iteration
// window reported rate 0.06 and ok false against rate<=0.05, and ok true against
// rate<=0.06, so the boundary lands exactly where this arithmetic puts it.
const MAX_STEADY_FAILED_RATE =
  DEMANDED_STEADY_REQUESTS > 0 ? MAX_STEADY_FAILED_REQUESTS / DEMANDED_STEADY_REQUESTS : 0;

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
    // All three integrity thresholds are scoped to the steady scenario. The
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
    // precisely what the warmup scenario exists for. Warmup drops are recorded
    // separately below and the TOLERANCE amended in 2026-09-17 does not touch
    // them, so cold-start cost still lands where it is expected and visible.
    //
    // AMENDED 2026-09-17. BOTH OF THESE WERE ABSOLUTE-ZERO GATES, count==0 and
    // rate==0, AND BOTH ARE NOW TOLERANCES SIZED FROM THE CELL'S OWN DEMAND. The
    // standard did not soften. The arithmetic of aggregation was wrong.
    //
    // WHAT AN ABSOLUTE ZERO COSTS AT MATRIX SCALE. An evidence run is 53 cells. Its
    // steady windows demand 1,224,053 iterations in total, 33 cells at 30000, 11 at
    // 9000 and 9 at 15000, and 1,428,053 counting warmup. A rule requiring EXACTLY
    // zero occurrences across that many independent opportunities is a lottery rather
    // than a quality bar, and it had already been lost twice on the sibling
    // quiescence gate before it was lost here.
    //
    // THE RUN IT KILLED. The third evidence attempt, at commit 5b2128c, died at
    // roughly cell 38 of 53 on cell passthrough-nonstream-4096-r5. That cell dropped
    // ONE steady iteration of 30001 scheduled, 0.003 percent, while reporting p99
    // 2.239ms, 500.0 of 500 rps demanded, zero warmup drops, zero failed requests and
    // a host that never once fell below the CPU idle floor in 76 readings. k6 exited
    // 99, run.sh invalidated the whole matrix, and 52 minutes of a person's machine
    // time went with it. Its own recorded threshold block reads
    // dropped_iterations{scenario:steady}:count==0 fail beside
    // http_reqs{scenario:steady}:count>=29400 pass, which is the whole story in two
    // lines.
    //
    // THE DROP TOLERANCE, 1 percent of demanded steady iterations with an absolute
    // floor of 25. Both numbers are calibrated against the 151 cells in this
    // repository's results tree, which carry 1,438,074 steady requests between them.
    // Seven of those cells recorded nonzero steady drops, and as a fraction of each
    // cell's own demand they are 0.003, 0.030, 0.130, 0.130, 0.130, 0.460 and 0.490
    // percent. The worst two are the load-bearing ones: 46 drops in a DIRECT cell with
    // no levee in its path at all, and 49 in a 150-byte enforce cell running at 11
    // percent of its measured capacity. Neither can be saturation, so both are host
    // scheduling stalls, and 0.490 percent is therefore the measured benign envelope
    // on this host rather than a guess. The tolerance sits 2.0 times above it.
    //
    // AND IT STILL CATCHES THE FAILURE IT WAS WRITTEN FOR BY 48.7 TIMES. That failure
    // is the 32768-byte capacity problem: a cell demanded 500 rps against roughly 400
    // rps of measured capacity, dropped 14622 steady iterations of 30001, and
    // published a 152.4ms median that Little's Law attributes entirely to 40 requests
    // waiting. 14622 of 30001 is 48.7 percent against an allowance of 1 percent.
    // Verified rather than argued: at the pinned k6, a deliberately saturated steady
    // window dropped 2754 of 4000 and reported ok false with exit 99 against
    // count<=30, and ok true against count<=99999, so the tolerance is what decides
    // and the mechanism fires.
    //
    // THE FLOOR OF 25 exists so a low-volume cell is not held to a tighter standard
    // than a high-volume one. Four separate recorded cells dropped EXACTLY 13
    // iterations, which is the observed size of one host stall on this box, so 25 is
    // just under two of those. It binds only below 2500 demanded iterations, which no
    // cell in today's matrix reaches, so it is a guard for a future low-rate cell
    // rather than an active allowance.
    //
    // THE FAILED-REQUEST TOLERANCE, 0.05 percent of demanded steady iterations with an
    // absolute floor of 5, is deliberately 20 times tighter in relative terms than the
    // drop tolerance, because the two mean different things. A dropped iteration is
    // the load generator giving up and says nothing about levee. A failed request is
    // levee refusing with a 429, erroring with a 5xx, or the loopback stack breaking,
    // and any of those is a fact about the system under test.
    //
    // ZERO failed requests have ever been recorded here, 0 in 1,438,074 steady
    // requests across 151 cells, so unlike the drop case there is no observed benign
    // envelope to size against. That absence is exactly why the absolute form had to
    // go anyway: zero events in 1,438,074 trials bounds the per-request failure rate
    // at 2.083e-6 at one-sided 95 percent confidence, which over the 1,224,053 steady
    // requests of an evidence run is up to 2.55 expected failures. So the data does
    // NOT rule out that rate==0 loses a 52 minute run more often than not.
    //
    // WHAT THE 0.05 PERCENT TOLERANCE STILL CATCHES. Every failure shape worth
    // catching here is SUSTAINED rather than singular. An exhausted per-agent
    // admission slot answers 429 for as long as the cell stays over the cap, so one
    // second of that at 500 rps is 500 failures against an allowance of 15. An
    // exhausted budget answers 429 for the whole remainder of the cell, which is tens
    // of thousands. Verified at the pinned k6: a window where every request failed
    // reported rate 1 and ok false with exit 99 against rate<=0.05. The singleton
    // transient this tolerates and the storm it catches are three orders of magnitude
    // apart.
    //
    // NEITHER TOLERANCE HIDES ANYTHING. The raw counts, the demanded count and both
    // allowances go into this cell's summary.json below, into dropped-iterations.txt
    // and failed-requests.txt, and into a run-total line in bands.txt with its
    // percentages, so a reader who wants the old absolute rule can apply it by hand to
    // any committed directory.
    'dropped_iterations{scenario:steady}': ['count<=' + MAX_STEADY_DROPPED_ITERATIONS],
    'http_req_failed{scenario:steady}': ['rate<=' + MAX_STEADY_FAILED_RATE],
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
    // so the expression carries the real demand rather than a fixed number.
    //
    // IS THE DROP THRESHOLD NOW REDUNDANT. Asked and answered on 2026-09-17, because
    // the honest thing to do with a gate that adds nothing is delete it rather than
    // give it a tolerance.
    //
    // A dropped iteration never becomes a request, so drops subtract from completions
    // ONE FOR ONE. That is measured, not assumed: the cell that died on this gate
    // demanded 30001 steady iterations, dropped 1, and recorded exactly 30000 steady
    // requests, and a historical cell that dropped 49 steady plus 18 warmup of 15001
    // scheduled recorded exactly 14934 iterations. So a cell dropping more than 2
    // percent of its demand ALREADY fails the achieved-rate floor below, which means
    // any drop tolerance at or above 2 percent would be strictly redundant with it.
    //
    // At 1 percent it is not redundant, and the reason is sensitivity rather than the
    // narrow band between 1 and 2 percent. A drop means the VU pool had NO free slot at
    // a scheduled arrival, so at that instant concurrency was at the 40-slot cap and
    // the pool had become part of what the cell measures. A momentary stall of that
    // kind inside an otherwise healthy cell costs a few hundredths of a percent of
    // throughput and is invisible to a 2 percent floor, while the drop count sees it
    // directly. The two gates therefore fail in opposite blind spots: a pool wide
    // enough to hold a backlog drops nothing and only the rate gate catches it, and a
    // brief pool exhaustion inside a cell that still serves its demand is seen only by
    // the drop count. Keeping both, each with a tolerance sized to its own measured
    // envelope, is what covers both shapes.
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
  // The steady-scoped failure sub-metric, materialized by the threshold above and
  // read here for the same reason the drop breakdown is. Before 2026-09-17 only the
  // whole-invocation failure count was recorded while the threshold covered the
  // steady window alone, so a warmup-only failure would have passed k6 and then been
  // re-failed by check_bands.py reading the wider field. Recording both closes that.
  const failedSteady = data.metrics['http_req_failed{scenario:steady}'] || {};

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
    // The integrity ALLOWANCES this cell was actually held to, recorded beside the
    // raw counts, ADDED 2026-09-17 with the tolerance amendment. Two reasons, and the
    // second is the important one. A reader can reproduce the verdict without
    // rerunning anything, and a reader can see which RULE a directory was judged
    // under, which matters because these two numbers replaced an absolute zero and a
    // future amendment could move them again.
    demanded_steady_requests: DEMANDED_STEADY_REQUESTS,
    max_steady_dropped_iterations: MAX_STEADY_DROPPED_ITERATIONS,
    max_steady_failed_requests: MAX_STEADY_FAILED_REQUESTS,
    max_steady_failed_rate: MAX_STEADY_FAILED_RATE,
    // Dropped iterations are reported three ways on purpose. Only the steady
    // count gates the run, and since 2026-09-17 it gates against a tolerance rather
    // than against zero, but a warmup drop must stay VISIBLE rather than
    // silently tolerated, otherwise a real steady-window problem could later
    // hide behind an unexplained nonzero total. A nonzero total with a passing
    // threshold means the drops either landed in warmup, which is cold start being
    // absorbed as designed, or stayed inside the steady allowance recorded above.
    // Which of the two it was is readable from these three fields alone.
    dropped_iterations: (dropped.values && dropped.values.count) || 0,
    dropped_iterations_steady: (droppedSteady.values && droppedSteady.values.count) || 0,
    dropped_iterations_warmup:
      ((dropped.values && dropped.values.count) || 0) -
      ((droppedSteady.values && droppedSteady.values.count) || 0),
    // http_req_failed is a Rate metric whose true observations mean "this
    // request failed", and k6 files true observations under passes and false
    // ones under fails. So passes is the failed-request count and fails is
    // the successful-request count, the opposite of what the field names
    // suggest. Probed at k6 v2.2.0: 201 responses of 404 reported
    // rate 1, passes 201, fails 0, and the same probe confirmed the tagged
    // sub-metric behaves identically, six forced steady failures reading
    // passes 6 and rate 0.06.
    //
    // The whole-invocation pair is kept for continuity with directories written
    // before the steady split existed. The STEADY pair is what the threshold and
    // check_bands.py act on.
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
