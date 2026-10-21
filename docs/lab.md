# Lab

Local, reproducible environment used to inject faults and measure
KubePilot. Status: P1 increments 1 (cluster + observability stack), 2a
(model server) and 2b (instrumented GenAI app + load generator). Fault
injection comes next.

## Usage

```sh
make models      # download the pinned GGUF model (sha256 checked)
make lab-up      # create cluster + observability stack + model server
make lab-check   # end-to-end check, non-zero exit on any failure
make bench-model # latency benchmark of the model server (50 requests)
make test-app    # pytest suite of chat-api, in the pinned Python image
make compat-matrix # record what the OpenAI instrumentation packages emit
make lab-stats   # memory/CPU of the kind node containers
make lab-down    # delete the cluster
make lint        # shellcheck the lab scripts
```

The lab uses its own kubeconfig (`.tools/kubeconfig`) and its own pinned
`kind`/`kubectl` (`.tools/bin`); it does not modify `~/.kube/config`.

```sh
export KUBECONFIG=$PWD/.tools/kubeconfig
```

## What runs where

| Component | Namespace | Role |
|---|---|---|
| kind (1 control-plane, 2 workers) | | Kubernetes 1.37 |
| OpenTelemetry Collector | `observability` | OTLP entry point; adds `k8s.*` attributes (`k8sattributes`); traces → Tempo, metrics → Prometheus OTLP receiver |
| Tempo (single binary) | `observability` | Trace storage and search (24 h retention) |
| Prometheus + kube-state-metrics | `observability` | cAdvisor (CPU throttling, memory), object state (phase, readiness, restarts, termination reasons), OTLP metrics |
| llama.cpp `llama-server` | `genai` | OpenAI-compatible API serving Qwen2.5-0.5B-Instruct (Q4_K_M) on CPU; `llamacpp:*` metrics |
| `chat-api` (FastAPI) | `genai` | Instrumented GenAI app: `POST /chat {prompt_id}` → model server; traces + metrics over OTLP |
| `loadgen` | `genai` | Steady background traffic: one request every 5 s, one at a time |

All versions and digests are pinned in [`lab/versions.env`](../lab/versions.env)
and checked by `lab/scripts/verify-pins.sh` before anything is created.

## Model server (increment 2a)

| Item | Value |
|---|---|
| Model | `qwen2.5-0.5b-instruct-q4_k_m.gguf` (468 MiB), Apache-2.0 |
| Model source | `Qwen/Qwen2.5-0.5B-Instruct-GGUF`, pinned to repo commit `9217f5db79a29953eb74d5343926648285ec7e67` |
| Model sha256 | `74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db` |
| Image | `ghcr.io/ggml-org/llama.cpp@sha256:143926008cbbbab8fae8fd87809875af3e42d3d676c30c11c55c0f3f683a5a07` |
| `llama-server --version` (in the pod) | `version: 0.7.0-dev (build 11723, commit 31288c9c6)` |
| Settings | `--threads 2 --parallel 1 -c 2048 --metrics`; CPU request = limit = 2; memory 768Mi / 1536Mi |
| Security | runs as uid 65532 (the image defaults to root; it works unprivileged), read-only root filesystem, all capabilities dropped |

The Deployment uses the **`Recreate`** strategy (P1 increment 3 decision):
the old pod is stopped before the new one starts, as for a single-instance
model server that cannot hold two copies of the model. Consequence: **any
change that `make lab-up` applies to the `model-server` Deployment briefly
cuts the model** (requests fail with connection errors until the new pod is
Ready: 3.5 s measured once with an idle server, up to ~30 s observed in
fault runs while the old pod stops, see "Observations for P2").

The model file is downloaded once on the host (`.tools/models`) and mounted
read-only into every kind node as `/models` (kind `extraMounts`), then into the
pod through a `hostPath` volume.

### Measured latency

One run on 2026-10-20 (raw data:
[`lab/measurements/2a-model-latency.jsonl`](../lab/measurements/2a-model-latency.jsonl),
summary: [`2a-model-latency-summary.json`](../lab/measurements/2a-model-latency-summary.json)).

Conditions: fixed prompt (41 prompt tokens), `max_tokens 64`, temperature 0,
prompt cache disabled, 1 warm-up + 50 sequential requests from a curl pod in
the same cluster. Host: 12 threads, shared with other workloads (another k3d
cluster, a browser, an IDE); average host CPU busy during the run: 25.5%,
load average 1.50 before and 2.14 after.

| Metric | Value |
|---|---|
| Requests OK | 50 / 50 |
| Latency p50 | 0.862 s |
| Latency p95 (indicative, n=50) | 0.914 s |
| Latency min / max | 0.835 s / 0.933 s |
| Warm-up request | 0.875 s (not slower: the server had already answered lab-check) |
| Generated tokens per request | 36 (the model stops before 64) |
| Generation speed (llama.cpp `timings`) | 55.0 tokens/s mean |
| Prompt processing | 229 ms mean |
| CFS throttling during the run | 1 throttled period out of 527 |
| Start time (pod created → Ready, image cached, model on hostPath) | 3 s |

Limits of these numbers: a single run on one laptop with other workloads
running; p95 over 50 samples is indicative only; with temperature 0 every
answer is identical, so this measures one fixed workload, not a realistic mix.
Answer quality is irrelevant here (the model is only a latency source).

### Memory accounting caveat

The container's measured memory is small (RSS max 111 MiB, working set max
115 MiB during the benchmark) although the model file is 468 MiB: llama.cpp
`mmap`s the model, and those file pages are page cache already charged
elsewhere on the host, not anonymous memory of the container. Consequence for
the `oomkill` fault (increment 3): lowering the memory limit alone will
probably not trigger an OOM kill while the model is mmapped; loading it with
`--no-mmap` is the likely way. Not verified yet.

## GenAI app and load generator (increment 2b)

**Instrumentation:** `opentelemetry-instrumentation-genai-openai==1.2b0` with
`openai==3.35.1` and `opentelemetry-util-genai==1.2b0`, locked with hashes in
`lab/apps/chat-api/requirements.txt`. **Pre-release (beta): attribute and
metric names may still change.** It was chosen with `make compat-matrix`;
`opentelemetry-instrumentation-openai-v2` is deprecated upstream and fails at
import with `openai` 3.x (details in
[`genai-conventions.md`](genai-conventions.md)).

**`chat-api`** (`lab/apps/chat-api`, image `kubepilot/chat-api:<content hash>`,
built locally and loaded with `kind load`):

- `POST /chat {"prompt_id": ...}` accepts only ids of a fixed synthetic
  catalog (`short-16`, `medium-64`, `long-256`, `check`, `privacy-canary`);
  free text is rejected (404). It returns token counts, latency and the
  **trace id**, never the completion text; checks fetch that exact trace.
- Calls the model with `temperature 0`, a fixed `seed`, `max_retries=0` (a
  retry would hide failures) and `cache_prompt: false` (prompt cache hits
  would skew token counts and latencies).
- `/healthz` does not depend on the model server, so a model outage does not
  make the app unready (verified in negative test N1).
- Pod identity is set at the source through the Downward API in
  `OTEL_RESOURCE_ATTRIBUTES` (`k8s.pod.name`, `k8s.pod.uid`, namespace, node);
  the collector then matches the pod by uid and adds `k8s.deployment.name`.
- Privacy: `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=no_content` is
  set explicitly, and the app configures **no log exporter** (with capture on,
  content is also emitted as log records; without a log pipeline it cannot
  leave the pod).
- Runs as uid 65532, read-only root filesystem, no capabilities.

**CPU limit decision for `chat-api`.** Measured with the load generator
running and a provisional 500m CPU limit: average 7m CPU, at most 21m over a
2-minute rate, yet **25 of 616 CFS periods throttled (~4%)** in 10 minutes:
millisecond bursts exceed the quota. Throttling adds latency to the app that
P2 could mistake for a model problem, so `chat-api` has **no CPU limit** and a
50m request (memory stays limited: request 128Mi, limit 256Mi; measured
working set ≈ 70 MiB). After the change cAdvisor exports no CFS throttling
series for the container (no quota). The model server keeps its CPU limit on
purpose: CPU throttling of the model is one of the faults to inject.

**Concurrency and context.** The model server runs `--parallel 1`: requests
that arrive while another is being served are **queued, not run in
parallel**, so each request always gets the full 2048-token context. The
longest catalog request (576 prompt tokens + 256 generated = 832 tokens) was
verified to fit (`make compat-matrix`). Queueing is real in the lab: the
load generator and lab-check can overlap, and a 2-token `check` request took
5.4 s while a `long-256` request was being served. The
`llamacpp:requests_deferred` gauge stayed at 0 over 15 minutes because it is
only sampled once a minute; queueing must be read from trace timings, not
from that gauge. If `--parallel` is ever raised, llama.cpp may split the
context between slots: re-check that 832 tokens still fit.

**Observed latencies** (load generator client side, 15 minutes, 40 requests
per prompt, 1 run, same shared laptop as above):

| Prompt | `max_tokens` | p50 | max |
|---|---|---|---|
| `short-16` | 16 | 498 ms | 994 ms |
| `medium-64` | 64 | 1 457 ms | 1 733 ms |
| `long-256` | 256 | 5 160 ms | 5 886 ms |

These latencies were taken over a window that overlaps lab-check runs and the
negative tests, so the max values are inflated by queueing behind
`--parallel 1`; the P2 latency baseline will be measured in a quiet window
(load generator only, no checks running).

**Lab footprint** after 2b (`make lab-stats`, one sample): ≈ 1.91 GiB for the
three kind nodes (750 + 577 + 626 MiB); `free -h` still reports 10 GiB
available.

**Known oddity, not explained:** `kubectl create --raw` POSTs through the API
server proxy reach `chat-api` as a JSON *string* (FastAPI answers 422), while
the same method works against llama.cpp. `check-genai.sh` therefore sends its
requests from inside the `chat-api` pod to the `chat-api` Service with
`kubectl exec` (the same path as an in-cluster client).

## Fault mechanisms verified before implementation (P1 increment 3a)

Measured on 2026-10-20 before writing any fault script.

**In-place CPU resize (for `cpu-throttle`).** `InPlacePodVerticalScaling` is
enabled (GA) on Kubernetes 1.37. `kubectl patch pod … --subresource resize
--type strategic` changed the model-server CPU from 2 to 300m without
recreating anything: same pod uid, same containerID, same start time, 0
restarts; the cgroup went from `cpu.max 200000 100000` to `30000 100000`.
cAdvisor counters kept counting (`resets()` = 0 over 10 minutes for usage,
periods and throttled periods), so deltas across the resize are valid.
Notes:
- `--type merge` (JSON merge patch) is **rejected**: it replaces the whole
  `containers` list, which would remove the memory values ("resource requests
  cannot be removed"). Use a strategic merge patch.
- The resize **does emit events**: `Normal ResizeStarted` and
  `ResizeCompleted` with the new values (the plan wrongly assumed none).
- Effect at 300m, from the load generator: throttled periods went from ~4 to
  ~547 per minute (~98% of periods); `short-16` 0.48 s → 3.1–3.9 s,
  `medium-64` 1.5 s → 9.2 s, and `long-256` **failed with 502 after the 30 s
  client timeout**. CPU throttling therefore produces errors (timeouts), not
  only slowness.

**Memory limit and mmap (for `oomkill`).** Same image and model, Docker with
cgroup v2, then confirmed in the cluster:

| Case | Memory limit | Load mode | Result |
|---|---|---|---|
| E1 | 300 MB | mmap (default) | runs and answers; anon ≈ 110 MB, file-mapped 15 MB in one run and 0 MB in another |
| E2 | 300 MB | `--load-mode none` | **OOMKilled** (exit 137) while loading |
| E3 | 1 GB | `--load-mode none` | runs; anon ≈ 547 MB, peak ≈ 555 MB |

With mmap, what is charged to the container depends on which cgroup first
read the model file (15 MB vs 0 MB above), so an mmap-based OOM fault would
not be deterministic. The fault will load the model with `--load-mode none`
(this build no longer accepts `--no-mmap`: "invalid argument") and a limit
below the measured ~555 MB. In the cluster the kubelet records
`lastState.terminated.reason=OOMKilled`, exit code 137, with a
`Warning BackOff` event; **there is no OOM-specific event** on the pod or the
node. kube-state-metrics exposes
`kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}` and
`kube_pod_container_status_last_terminated_exitcode` = 137.

**Image pull on a node without the image:** pulling the llama.cpp image took
80 s on a worker that did not have it, a natural cold-start mechanism.

**`Recreate` strategy, observed with a pod watch (ms timestamps):** old pod
deletion requested at 30.086, old container terminated at 30.350, new pod
created at 30.362 (only after), new pod Ready at 33.603: a ≈ 3.5 s outage.

## Fault injection (P1 increment 3)

Faults are injected by `lab/faults/` and recorded in `eval/runs/` (format and
rules: [`eval/README.md`](../eval/README.md)).

```sh
make run-fault SPLIT=dev FAULT=cpu-throttle VARIANT=limit-300m REP=1   # full recorded run
make fault-cpu-throttle VARIANT=limit-300m && make fault-reset          # by hand, never scored
make fault-check FAULT=cpu-throttle                                     # is the signal there?
make baseline-stability                                                 # 5 quiet windows
```

A run takes about 12 minutes: precheck, 3 min baseline, injection, 5 min
fault window with the per-fault check at its end, reset to the declared
manifests, wait until chat-api answers again, 3 min recovery, export.

**Lab fault hooks of chat-api.** `chat-api` has a lab-only endpoint
`PUT /lab/delay {"ms": N}` that sleeps before the model call (used by the
proposed `app-delay` fault). It only exists when the container runs with
`LAB_FAULT_HOOKS=1`; the declared state is `LAB_FAULT_HOOKS=0` (the endpoint
answers 404). Switch it with `make lab-hooks-on` / `make lab-hooks-off`:
**each switch changes the chat-api Deployment and therefore redeploys
chat-api** (rolling update, a new pod), so it is only allowed **between
runs**: the script refuses while a fault is active or a run holds the lock,
and fault resets keep the current hook state instead of changing it.
`lab-check` (`check-genai`) fails if the hooks are on while no `app-delay`
run is active.

**Suspend during a run.** A run needs the laptop awake for its whole
duration. On 2026-10-20 the machine was suspended (lid closed) from 17:19:28
to 23:05:04 UTC during the baseline of a cold-start run: the baseline lasted
20 907 s instead of 180 s and the cluster was frozen in between. `run.sh` now
times every phase on the wall clock and marks the run `invalid` if a phase
differs from its planned duration by more than 20 % (`PHASE_TOLERANCE`),
resetting the lab first if the fault was injected (tested by freezing the
fault-window `sleep` with SIGSTOP: "phase fault lasted 48s instead of 20s").
To prevent suspends during a batch, `make run-fault` and
`make baseline-stability` accept `INHIBIT_SLEEP=1`, which wraps the command
in `systemd-inhibit --what=sleep:idle:handle-lid-switch --mode=block`
(off by default).

**Validation thresholds are not diagnosis rules.** `check-fault` uses fixed
thresholds, for example `MIN_THROTTLED_FRACTION=0.20` and
`MIN_LATENCY_RATIO=2.0` for cpu-throttle. They only answer "did the
injection produce the signal the catalog announces?", so that a run whose
injection silently failed is marked `invalid` instead of being scored. They
must **not** be copied into the P2 rule engine, because:

- they were chosen knowing which fault was injected and how strongly; a rule
  that reuses them would be tuned on the answer and would inflate accuracy;
- they are deliberately loose to accept every variant of the catalog, which
  says nothing about where the boundary between "normal" and "abnormal"
  lies in a quiet window (the baseline-stability measurement does);
- a rule must also handle evidence the check ignores (other pods, other
  nodes, decoys, clock skew) and be able to answer "unknown".

P2 rules are written from the dev runs and the quiet-window statistics, and
validated on the test split only.

**Baseline stability** (`make baseline-stability`, 2026-10-20, file
`eval/baseline/stability-20261020T132611Z.json`): 5 consecutive quiet
windows of 300 s (load generator only, no fault, no check), host CPU busy
22-24%, 0 errors, 19-20 requests per prompt and window.

| Prompt | p50 min | p50 max | max/min | CV of p50 |
|---|---|---|---|---|
| `short-16` | 487 ms | 501 ms | x1.03 | 1.1% |
| `medium-64` | 1 410 ms | 1 476 ms | x1.05 | 1.6% |
| `long-256` | 5 007 ms | 5 272 ms | x1.05 | 1.9% |

The window-to-window spread of the p50 (at most x1.05) is far below the
effects measured for the faults (cpu-throttle at 300m x6.6, host CPU
contention x1.85-2.2, one inference thread x1.6-1.7). Limits: this is the
p50 only (tails were not studied), on a quiet host, in one session; it does
not say how stable the baseline is across days or under other host loads.

**Cross negative tests of the checks** (2026-10-20, 150 s after injection):
the cpu-throttle check fails during an oomkill fault (0 throttled periods, no
successful `short-16` request), and the oomkill check fails during a
cpu-throttle fault (no pod created after the injection); each fault's own
check passes on the same injection. Both checks also fail when no fault is
active. Limit: the oomkill check expects a new pod (all catalog variants
change the Deployment); an OOM kill that restarts the container inside the
same pod would not satisfy it.

## Observations for P2

Facts seen in the recorded runs that a diagnosis engine must not get wrong.

- **"untolerated taint" without any injected taint.** In the
  `unschedulable/nodeselector-gpu` run the FailedScheduling message was
  `0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 node(s)
  didn't match Pod's node affinity/selector`. The taint is the standard
  control-plane taint of the kind cluster. An engine that looks for the word
  "taint" would blame a taint; it must read the **count of nodes per reason**
  (here 1 control-plane taint + 2 selector mismatches, whereas the
  `node-taint` variant should give 3 nodes with untolerated taints).
- **Same cause, different FailedScheduling messages.** Both `unschedulable`
  dev runs left the new pod `Pending` with `PodScheduled=False/Unschedulable`
  and `kube_pod_status_unschedulable == 1`, but the scheduler message depends
  on the mechanism (messages as recorded in `check.txt` and `events.json.gz`):

  | Run | Mechanism | FailedScheduling message |
  |---|---|---|
  | `dev--unschedulable--nodeselector-gpu--r1` | nodeSelector matching no node | `0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 node(s) didn't match Pod's node affinity/selector. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.` |
  | `dev--unschedulable--cpu-request-64--r1` | CPU request of 64 cores | `0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 Insufficient cpu. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.` |

- **Rule for P2: one cause, several messages; the control-plane taint is not
  the cause.** Both messages are the same cause (`unschedulable`); only the
  reason given for the 2 workers differs (`didn't match Pod's node
  affinity/selector` vs `Insufficient cpu`). The `1 node(s) had untolerated
  taint(s)` part is present in **both** messages: it is the control-plane
  taint of the kind cluster, and it is the cause in neither run. An engine
  must not key on one exact message text, and must not blame a taint unless
  the taint accounts for the nodes that could otherwise run the pod.
- **The fault window contains the recovery events of the reset.** The window
  of an exported run (and `expected_causes[].active_to = reset_done`) also
  holds the events of the reset itself (ScalingReplicaSet, Killing,
  SuccessfulCreate, Scheduled, Pulled, Created, Started of the new pod), a
  few seconds before `reset_done`. They are the lab putting things back, not
  a second fault; an engine must not report them as another cause.
- **"Readiness probe failed" events that are not a readiness fault.** In
  the oomkill run (`dev--oomkill--noload-400mi--r1`) the export contains two
  kinds of Unhealthy events:
  - on the **old pod being stopped** by the Recreate rollout
    (`model-server-6c974f9667-57p7c`, x5 at 13:09:29):
    `Readiness probe failed: Get "http://10.244.2.11:8080/health": dial tcp
    10.244.2.11:8080: connect: connection refused`;
  - on the **new crash-looping pod** (`model-server-7d785c95dc-6slrm`, x3 at
    13:10:58): `Startup probe failed: Get "http://10.244.2.12:8080/health":
    dial tcp 10.244.2.12:8080: connect: connection refused`.

  The same "Readiness probe failed ... connection refused" appears on the
  terminating old pod in the readiness-fail run (x5 at 16:51:59), before the
  injected 404 failures. Every Recreate rollout of the model server can
  produce it. **This is a possible confusion with `readiness_failing`**: the
  injected readiness fault gives `HTTP probe failed with statuscode: 404` on
  the *new* pod, while these come from the *old* pod while it stops.
- **Stopping the old pod can take up to ~30 s.** Time between the injection
  (old pod Killing) and the creation of the new pod: about 29 s in the
  oomkill run, 30 s in the readiness-fail run, under 1 s in the
  unschedulable run. The 3.5 s measured once in increment 3a (D1) was the
  favourable case; the unavailability caused by a Recreate rollout lasts up
  to the termination grace period (30 s by default).
- **Rule for P2: a "Readiness probe failed ... connection refused" on the old
  pod of a Recreate rollout is NOT `readiness_failing`.** An engine must
  look at three things before blaming readiness: the **pod** (the old pod
  that is being stopped, with a Killing event, or the new pod that should
  serve), the **failure** (`connection refused` while the server shuts down
  vs an HTTP status such as `statuscode: 404` from a running server) and the
  **time** (during the termination of the old pod vs after the new pod has
  started).
- **CPU throttling also produces errors.** At 300m, `long-256` requests time
  out (502 after the 30 s client timeout): throttling is not only slowness.

## What `make lab-check` proves, and what it does not

- The trace path is proven end to end: a synthetic trace sent to the collector
  is found in Tempo with `k8s.pod.name` equal to the sending pod.
- The OTLP metric path is proven: a synthetic metric reaches Prometheus with
  a `k8s_pod_name` label.
- **CPU throttling check: limited.** It only proves that cAdvisor exports
  `container_cpu_cfs_throttled_periods_total` (the series exists because the
  collector has a CPU limit). It does **not** prove that throttling happens,
  nor that the series belongs to a running container: during the negative
  test the series of the previous collector pod was still returned. Actual
  throttling is verified by the `cpu-throttle` fault in a later increment.
- **Model server checks** (`check-model.sh`): pod Ready, `/health` ok, a real
  chat completion, the scrape target and a `llamacpp:*` counter, and the CFS
  throttling series. All Prometheus checks are pinned to the pod running now
  (name or IP): Prometheus keeps returning the last samples of a vanished
  target for up to 5 minutes, and an unpinned check passed during the
  negative test because of that. The throttling check still only proves that
  the series exists (the benchmark measured 1 throttled period out of 527).

### Negative test

To check that lab-check can fail, scale the collector to zero and run it:

```sh
export KUBECONFIG=$PWD/.tools/kubeconfig
.tools/bin/kubectl -n observability scale deploy/otel-collector --replicas=0
make lab-check   # must fail on the trace and OTLP metric checks
.tools/bin/kubectl -n observability scale deploy/otel-collector --replicas=1
make lab-check   # must pass again
```

Same procedure for the model server
(`.tools/bin/kubectl -n genai scale deploy/model-server --replicas=0`): all 6
`check-model` checks must fail, then pass again at 1 replica.

`check-genai.sh` has two negative tests (results of 2026-10-20):

- **N1, model server at 0 replicas:** `/chat` returns 502, the GenAI span has
  no tokens and the load generator has no successful request (3 FAIL); the
  failure trace carries `error.type=openai.APIConnectionError`, and chat-api
  stays Ready. This proves the checks depend on a real end-to-end model call.
- **N2, content capture switched on**
  (`kubectl -n genai set env deploy/chat-api OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=span_only`):
  the privacy check fails and names what it found
  (`gen_ai.input.messages`, `gen_ai.output.messages`, the marker). This
  proves the privacy check detects content. Restore with `make lab-up`
  (re-applies `no_content`). The canary traces stored in Tempo during N2
  contain only the synthetic canary prompt and expire with the 24 h retention.

**Do not run `make lab-up` during a negative test.** `lab-up` runs
`helm upgrade`, which resets the collector to its declared replica count (1)
and silently invalidates the test. Check `replicas=0` before trusting a
failing run.

## Kubernetes event retention

Kubernetes Events are kept by the API server for `--event-ttl`, **1 hour by
default upstream**. The lab keeps that default so it behaves like a real
cluster. It is configurable at cluster creation:

```sh
make lab-down && make lab-up EVENT_TTL=24h
```

(see [`lab/kind/cluster.yaml`](../lab/kind/cluster.yaml)).

Consequence for the benchmark: an evaluation run cannot rely on events still
being in the API server when it is scored later. **The benchmark will export
the Kubernetes Events of the relevant namespaces at the end of each run** and
store them with the run's ground truth in `eval/`, so every run can be
re-scored offline. This also reflects a real limitation KubePilot has on live
clusters: evidence older than the event TTL is gone unless an exporter kept
it.
