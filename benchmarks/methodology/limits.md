# What these numbers do not support

Each entry here bounds a published claim. The headline results are in
[../README.md](../README.md) and the matrix that produced them is in
[estimator-and-matrix.md](estimator-and-matrix.md).

## The two number families are not equally well controlled

`controla` and `controlb` both run the passthrough config, so their shift against
the direct baseline reads +0.284 and +0.296ms at P50 where the passthrough arm
itself reads +0.281ms. A reader who notices that the control agrees with the
treatment should not conclude the overhead number is meaningless. Three
identically configured proxied cell groups reading +0.281, +0.284 and +0.296ms
against the same baseline is a reproducibility result. What the control measures is
the noise floor of a PAIRED difference between two proxied arms run back to back
with the same levee restart and TIME_WAIT drain between them. That reading is
`controlb` minus `controla`, -0.011ms with a 0.047ms spread across five
repetitions, against a true value of zero.

The enforcement number has that same paired shape. The proxy hop number does not.
It differences a proxied cell group against direct canary cells that run at the
two ENDS of a 70-minute matrix, so any between-cell offset that every proxied cell
carries and no direct cell does stays inside it instead of cancelling. There is no
direct-versus-direct pair anywhere in the matrix, so nothing here bounds that
offset directly. The two canaries bound the DRIFT of the direct arm across the
whole matrix at 0.020ms at P50, a fourteenth of the 0.281ms hop, and that is the
only handle this run gives on it.

So the enforcement figures are the better controlled of the two families, and the
proxy hop figures are reproducible across three arms without being paired.

## P99.9 is not resolvable on this host

At P99.9 every 150-byte proxy hop shift goes NEGATIVE, -0.207ms for passthrough
and -0.122ms for enforce, and the two A/A control cells straddle zero at -0.223 and
+0.133ms. The direct baseline's own tail, 4.221ms at P99.9, exceeded the proxied
tails. Every one of those intervals spans zero.

That is a limit of the measurement and not a finding about levee. A non-streaming
evidence cell puts roughly 30 observations above P99.9, half that streaming, the
reference host carries resident endpoint-security agents, and at that quantile host
scheduling noise is larger than anything levee contributes, so which arm reads
higher is decided by which cell caught the worse burst. No P99.9 overhead claim is
supported by this run, in either direction.

## Enforced throughput is bounded by tokenizer CPU

This is both a limitation on the methodology and a result in its own right. It was
found by an evidence run failing.

Levee needs about 10.94ms of CPU per enforced 32768B request against 0.233ms for a
passthrough one, a 47-fold spread, almost all of it tokenizer work on the prompt.
That single number is what makes one global arrival rate impossible: a rate that
leaves the passthrough arm idle saturates the enforce arm at the same payload size.
`run.sh` samples both figures every run from `ps` cputime deltas, per-cell values
land in `cpu-seconds.txt`, and the implied busy core count is in `bands.txt`.

The enforced end of that spread reproduces and the passthrough end does not. The
valid evidence run's own sampling reads 10.63ms per enforced 32KB request, which
lands on both earlier figures, against 0.618ms per passthrough 150B request, which
is 2.6 times the 0.241ms recorded before it. The spread it measures is therefore
17.2-fold, and 7.0-fold taken at the SAME payload, 10.63ms enforced against 1.51ms
passthrough at 32768B, which is the comparison that actually sizes an arrival rate.

The movement is in the cheap cells and it is not explained. Every 500 rps cell
except `enforce-nonstream-4096` reads roughly 0.35ms per request MORE CPU in the
60-second evidence regime than in the 20-second quick regime: passthrough 150B goes
from 0.26 to 0.29 up to 0.615, enforce 150B from 0.358 to 0.378 up to 0.742,
passthrough 4096B from 0.314 to 0.330 up to 0.697, while enforce 4096B stays put at
1.66 to 1.76 against 1.648. That is the same regime asymmetry the latency numbers
show, observed independently on CPU, and it has no established mechanism. The
conclusion is unchanged: enforced 32KB work costs an order of magnitude more CPU than
any passthrough cell at any payload, so rates have to be set per payload size.

Measured capacity at a 32KB prompt on the reference host, enforce mode, with the
shipped double tokenizer pass:

| offered concurrency | achieved throughput    |
|---------------------|------------------------|
| 4                   | about 400 rps, the peak |
| 8                   | 295 rps                |
| 40                  | 260 rps                |

Throughput goes RETROGRADE past the knee. More concurrency buys less work, which is
the signature of contention and not of a flat ceiling. Passthrough at the same
payload has capacity above 7100 rps and enforce at 4096B has capacity around 2706
rps, so the collapse belongs to tokenizer CPU at large prompts and not to the proxy
hop. Sizing a rate against the 400 rps peak rather than against the 260 rps a
saturated pool achieved matters, because in the retrograde region a saturated cell
reports a capacity number that is itself a consequence of the saturation.

The failure that produced this is worth stating concretely, because it is what a
reader of an older 32KB number needs in order to distrust it. An evidence attempt
demanded 500 rps at 32768B enforce, dropped 14622 steady iterations of 30001, 48.7
percent, exited 99, and reported a 152.4ms median. The honest service time there is
8.5ms. The remaining 144ms was queue residence, and Little's Law closes the gap
exactly: 40 requests in flight over the 260 rps actually achieved is 154ms, against
152.4ms observed. Nothing was wrong with levee. A latency measurement of an
over-demanded system is a measurement of its queue.

These capacity figures are one host, one payload size per figure, and the shipped
double pass. They are enough to size an arrival rate and to state the shape, and
they are not a capacity model. There is no rate sweep in the matrix, so the knee is
known at 32KB enforce and nowhere else.

## Known asymmetries

- **HTTP/1.1 idle-connection churn on the levee-to-mock leg.** Levee's upstream
  transport keeps the standard library default of two idle connections per host, so
  at the 500 rps the 150B and 4096B cells run at, that leg opens and closes
  connections in a way the direct baseline does not. The default is read from the
  standard library rather than separately probed, so treat the churn magnitude as
  inferred. It is a real asymmetry either way, and it is correctly charged to the
  proxy because it is what levee actually does. The TIME_WAIT count is recorded at
  every cell boundary in `machine-state.txt`, and the next cell waits for it to fall
  so one cell's churn cannot bleed into the next.
- **k6's `http_req_duration` excludes connection acquisition.** Generator-side
  connection setup is invisible in both baselines, which is what makes them
  comparable. Levee-to-mock churn lands inside proxied `http_req_waiting`, so it is
  attributed to the proxy, which is correct.
- **Arrivals are evenly spaced, not Poisson.** A constant-arrival-rate executor
  spaces requests uniformly. Real traffic arrives in bursts, and bursty arrivals
  queue, so the tails here understate what a bursty client population would see.
- **One utilization point per cell.** There is no rate sweep inside the matrix, so a
  published cell says nothing about where its own knee is. The knee at 32KB enforce
  is known from the capacity measurement above. No other cell's knee has been
  measured, and the ones at 150B and 4096B are bounded only from below, by the 2706
  rps figure at 4096B.
- **Levee CPU per request is sampled, so saturation is visible.** `run.sh` reads
  `ps -o cputime` for the levee process at the two edges of every cell's steady
  window and records the delta and the per-request cost in `cpu-seconds.txt`. This is
  a report and not a gate, because the gate on saturation is the achieved-rate check.
  Two limits on it: `ps` reports the process's own time and not that of any reaped
  child, which is correct for a single-process Go binary and would not be for a
  supervisor, and its resolution is a hundredth of a second, which is immaterial
  against the smallest delta in the matrix at roughly 2.3 CPU seconds.
- **macOS only.** The environment capture is `pmset`, `sysctl` and `caffeinate`, and
  there is no CPU pinning because macOS offers none. A Linux reproducer will have to
  rewrite the environment capture and the power and thermal assertions: the sysctl
  names differ, `somaxconn` and the ephemeral port range live elsewhere, and Linux
  binds the whole loopback range without an interface alias where macOS does not. It
  should expect a different tail, most likely a tighter one, since the tail here is
  dominated by host scheduling noise. That expectation is unverified until someone
  runs it.
- **The measurement host is not quiet, and this cost one whole evidence run.** The
  reference machine carries resident endpoint-security agents. Load average during
  runs ranged from roughly 5 to 36 on 12 cores across this session, and the
  machine-state readings in the surviving quick runs span 5.07 to 13.32. The P50 is
  stable at the payload sizes where the signal is large, and quick mode is a lottery
  for the tail, since a single 20-second cell can catch a noise burst that moves its
  P99 by milliseconds. Evidence mode exists for that reason, and it was not enough on
  its own: the first completed 43-cell evidence run measured a 150-byte enforcement
  delta of +123us where every reading from a host that passed the quiescence floor
  falls between +11 and +90us, with every other gate passing. Three consequences now
  live in code: the host quiescence gate, the A/A control, and the primary
  enforcement gate sitting at the payload where the signal is roughly 13 times the
  estimator noise floor rather than comparable to it.

## Considered and rejected

- **wrk2 as a second generator.** Dormant since 2024, does not build on darwin/arm64
  with open pull requests going back years, and has no Homebrew formula. A cross-check
  that cannot be installed is not a cross-check. vegeta and oha are the maintained
  candidates if a second generator is ever wanted.
- **A TLS mock behind a locally installed certificate authority.** It would plant a
  standing CA in the operator's keychain purely to run a benchmark, introduce an
  HTTP/2-versus-HTTP/1.1 asymmetry between the two legs, and make the reproduce story
  heavier for an outside reviewer. Instead a narrowly scoped configuration change
  allows plaintext upstreams only when the host is a literal loopback address, which
  keeps both legs on uniform HTTP/1.1.
- **In-process assembly of levee inside a Go test.** Faster and easier to instrument,
  and it would not test the shipped binary. The harness measures `levee serve` as a
  user runs it, including its flag parsing, its config validation, its logger and its
  real listeners.
