# Evaluation data and rules

This directory holds everything needed to measure KubePilot's diagnosis
accuracy reproducibly: the fault catalog, the recorded runs (ground truth +
exported evidence), fixtures, and the pre-registration records.

**All results are on faults injected in a local lab, not on real
production incidents.**

## Layout

```
eval/
  README.md                  this file
  schema/run.example.json    exact format of a run record (schema_version 1)
  faults/catalog.v1.json     fault variants, their split (dev/test) and why test variants differ
  runs/<split>/<run_id>/     one directory per run (split: dev, test, dryrun, manual)
    run.json                 ground truth, written at injection, completed at reset/end
    events.json              Kubernetes events of the run window (+/- 5 min)
    pods.json                pod snapshot at the end of the fault window, before reset (genai + noise)
    loadgen.jsonl            load generator lines of the window (gives the trace ids)
    traces/<trace_id>.json   Tempo traces of those requests
    metrics.json             key Prometheus series of the window
    check.txt                output of the per-fault check (signal really present)
  preregistration/           committed before any test-set run (see below)
  fixtures/                  recorded telemetry used as test data (e.g. genai-instrumentation/)
```

## Ground truth (`run.json`)

Written by the fault script **at injection time**, before any diagnosis is
run, then completed at reset and at the end of the run. Fields (see
`schema/run.example.json`):

- `run_id`: deterministic, `<split>--<fault or nofault>--<variant>--r<n>`
  (no timestamp), so an interrupted batch (`make run-batch SPLIT=dev`) can
  resume: a run whose `run.json` has `"status": "complete"` is never redone.
- `split`: `dev` or `test` (scored), `dryrun` (validates a test variant's
  injection, never scored) or `manual` (a fault injected by hand with
  `make fault-<name>`, never scored); `kind`: `fault` or `nofault`.
- `expected_causes[].cause` must be one of the **closed list of cause
  names** in `faults/catalog.v1.json` (`causes`). Those names never change
  after a pre-registration; a new name needs `catalog.v2.json`.
- `fault` (null for `nofault`), `decoys` (unrelated faults injected as
  noise), `expected_causes` (what a correct diagnosis must find, with the
  component and the time window the cause was active).
- `expected_causes[].active_from` is `timeline.inject_start`: the cause is
  active from the moment the change is applied, which is also when the
  service becomes unavailable (with `Recreate`, the old pod stops right away).
  For `cold-start/image-not-cached` the injection starts by scaling the model
  server to 0, so **for the diagnosis the unavailability, and the
  `cold_start` cause, start at the scale to 0**, not when the new pod is
  created. `active_to` is `timeline.reset_done`, except for faults whose
  effect ends by itself: cold-start records the time the new pod became
  Ready (`fault.applied.effect_end`).
- `timeline`: UTC timestamps with milliseconds, taken on the host. kind nodes
  share the host clock, so the lab has no clock skew; skew tolerance is
  tested separately with shifted fixtures.
- `target`: pods before and after (name, uid, node, container id, restarts).
- `versions`: git commit and dirty flag, sha256 of `lab/versions.env`, image
  IDs actually running, llama.cpp version.
- `host`: CPU count, load average, available memory and **host CPU busy %
  per phase**, to spot runs disturbed by other workloads on the laptop.
- `status`: `running`, `complete`, `interrupted` or `invalid` (e.g. the
  per-fault check did not see the signal, or the lab was not healthy before
  injection). Only `complete` runs are scored.

**Quiet-period precondition.** Before the baseline, `run.sh` requires that
nothing changed in the `genai` namespace during the last `QUIET_S` = 300 s
(no Kubernetes event, no pod created), so that the exported window (+/- 5
min) does not contain changes unrelated to the injected fault. Because the
previous run's reset creates a pod only 3 minutes before its end, the
script **waits** up to `QUIET_MAX_WAIT_S` = 420 s for the namespace to become
quiet; if it does not, the run is `invalid` and `notes` says why (the last
change seen and the number of changes). The start of the quiet period is
recorded as `timeline.quiet_since`.

**Wall-clock guard (precondition of a valid run).** Each waiting phase
(baseline, fault window, recovery) is timed on the wall clock and recorded in
`phase_seconds` (`planned`, `real`). `sleep` counts monotonic time, which
stops while the machine is suspended, so a suspend makes a phase last much
longer in real time than planned (on 2026-10-20 a lid-close suspend turned a
180 s baseline into 20 907 s). If a phase differs from its planned duration
by more than `PHASE_TOLERANCE` (20 %), the run is `invalid` with the reason
`phase X lasted Ns instead of Ms (machine suspended?)`, the lab is reset if
the fault was already injected, and the run is moved to `eval/runs/invalid/`.

Kubernetes keeps events for 1 hour (`--event-ttl`); every run is far shorter
and exports its events, traces and metrics at the end, so runs can be
re-scored offline after Tempo (24 h) and Prometheus (2 d) retention.

## What is committed (rule proposed 2026-10-20, pending validation)

Tempo keeps traces 24 h and Prometheus 2 days, so a run can only be re-scored
later from what it exported. For every scored run (`dev`, `test`) and every
`dryrun`, these files are committed:

| File | Why |
|---|---|
| `run.json` | ground truth |
| `check.txt`, `precheck.txt` | proof that the injection produced its signal and that the lab was healthy before |
| `events.json.gz` | Kubernetes events of the window (gone after 1 h) |
| `traces.jsonl.gz` | Tempo traces of the window (gone after 24 h) |
| `metrics.json.gz` | Prometheus series of the window (gone after 2 days) |
| `loadgen.jsonl.gz` | client view of every request, including requests without a trace (app down) |
| `pods.json.gz` | pod specs/status at the end of the fault window, before reset (`genai` + `noise`) |

`manual` runs, `*.prev-*` attempts and `eval/runs/invalid/` are never
committed. A run that ends `invalid` (precondition or signal check failed)
or `interrupted` is moved by `run.sh` from its split directory to
`eval/runs/invalid/<run_id>--<UTC time>`, so split directories only contain
runs that are complete (or still running), and the run can be redone.

Measured sizes (bytes) on the first 5 exported runs (2 dev runs of the full
length, 3 shorter manual runs), before the export compressed everything:

| Run | run.json | events.json | events.gz | traces.gz | metrics.json | metrics.gz |
|---|---|---|---|---|---|---|
| dev cpu-throttle | 4 148 | 4 880 | 748 | 29 061 | 203 930 | 10 000 |
| dev oomkill | 4 254 | 30 545 | 2 988 | 39 402 | 231 347 | 11 066 |
| manual cpu-throttle | 3 775 | 47 631 | 4 030 | 7 352 | 198 587 | 9 000 |
| manual oomkill | 3 777 | 33 350 | 2 913 | 15 015 | 160 760 | 7 600 |
| manual oomkill | 3 778 | 48 291 | 3 804 | 15 617 | 210 242 | 9 351 |

The committed set of the two full-length dev runs, as stored by the export
(default gzip level, all files above): **53 647 bytes** (cpu-throttle) and
**68 877 bytes** (oomkill). About 60 runs (dev, test, no-fault) would add
about 4 MB to the repository; uncompressed `metrics.json` alone would have
been about 12 MB.

## Splits

- **dev**: variants used while writing and tuning the diagnosis rules.
- **test**: variants that are **never** run, looked at or used while
  developing rules. Reported accuracy comes from the test split only. At
  least one test variant per fault differs **in nature** from the dev
  variants (not only by a nearby value); the catalog states why, and whether
  its observable signal is identical to a dev variant (then it tests the
  cause, not the distinction between causes).
- **No-fault windows** (`kind: nofault`) exist in both splits: same duration
  and traffic, no injected fault, some with decoys (unrelated pod
  restarts/OOM on the same node as the model server and on another node).
  They measure false positives; the expected answer is no cause (or
  "unknown"). The false-positive rate is reported on the test split only.

## Dry-run of test variants

No test variant enters a pre-registration without a **dry-run**: it is
executed once to check that the injection really produces the signal
announced in the catalog (the per-fault check passes). The dry-run validates
the **injection**, never the diagnosis: its data must not be used to write or
tune diagnosis rules, it is not a test run and it is not scored. Dry-run
records are kept under `eval/runs/dryrun/` with `"split": "dryrun"`.

## Pre-registration rule (mandatory)

Before the **first** run of the test split, the following must be
**committed to this repository**, in a commit of its own:

1. `eval/preregistration/<YYYY-MM-DD>-<name>.json` containing:
   - the date;
   - the git commit of the diagnosis engine and the git tree hash of its
     source directory (`git rev-parse HEAD:<engine dir>`);
   - the sha256 of the fault catalog and the list of test variant ids;
   - the sha256 of the scoring script and the exit criterion being tested
     (e.g. ">= 70% correct cause in top-3 on >= 20 injected faults and
     <= 10% false positives on >= 20 no-fault windows").
2. The commit message says explicitly what it is, e.g.
   `eval: pre-register P2 test set (engine tree <hash>)`.

Then:

- Test runs are scored with exactly the pre-registered engine and scoring
  script. The test results are published as they come out, including
  failures.
- If the engine is changed after seeing test results, the old results stay
  published, and a new claim needs a **new** pre-registration and new test
  runs (new variants or new repetitions).
- Rule development reads only `eval/runs/dev/`.

This is a **process rule, not a technical lock**: nothing prevents someone
from reading `eval/runs/test/`. The commit history is what makes it
verifiable: the pre-registration commit must precede every commit that adds
test-run data.

## Limits

- **No clock skew in the lab.** All kind nodes are containers on one host and
  share its clock, so evidence from different sources is never shifted in
  time. Tolerance to clock skew between nodes and backends must be tested
  separately in P2, with fixtures whose timestamps are deliberately shifted.
- **One cluster, injected faults.** Every result comes from faults injected
  into a single kind cluster on one laptop, with one model server, one app
  and synthetic traffic. Nothing here is measured on real incidents,
  multi-node production clusters, GPUs or real user traffic.
- **Statistics.** 20 injections are not 20 independent cases: several are
  variants of the same fault and windows are repeated under the same
  conditions. Every published accuracy or false-positive rate states `n`, the
  number of successes and the **95% Wilson confidence interval**, and says
  that the cases are correlated (so the interval is optimistic).
- **Shared host.** The laptop also runs other workloads; each run records
  the host CPU load per phase so that disturbed runs can be identified and
  excluded or re-run, but the noise is not controlled.
