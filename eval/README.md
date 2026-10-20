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
    pods.json                pod snapshot at the end of the run
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
  (no timestamp), so an interrupted batch can resume: a run whose `run.json`
  has `"status": "complete"` is never redone.
- `split`: `dev` or `test` (scored), `dryrun` (validates a test variant's
  injection, never scored) or `manual` (a fault injected by hand with
  `make fault-<name>`, never scored); `kind`: `fault` or `nofault`.
- `expected_causes[].cause` must be one of the **closed list of cause
  names** in `faults/catalog.v1.json` (`causes`). Those names never change
  after a pre-registration; a new name needs `catalog.v2.json`.
- `fault` (null for `nofault`), `decoys` (unrelated faults injected as
  noise), `expected_causes` (what a correct diagnosis must find, with the
  component and the time window the cause was active).
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

Kubernetes keeps events for 1 hour (`--event-ttl`); every run is far shorter
and exports its events, traces and metrics at the end, so runs can be
re-scored offline after Tempo (24 h) and Prometheus (2 d) retention.

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
