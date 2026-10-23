# P3 Baseline Comparison Protocol (`eval/baselines/`)

This directory holds the evaluation protocol, scripts, and recorded outputs
comparing **KubePilot (`why-slow`)** against the two closest open-source tools
identified in the P0 landscape check ([`docs/landscape.md`](../../docs/landscape.md)):

1. **Coroot Community Edition** (Apache-2.0): eBPF + Prometheus + OpenTelemetry
   observability platform with built-in deterministic inspections (`CPU`,
   `Memory`, `Deployments`, `Instances`, `SLO`, `Net`, `Storage`).
2. **HolmesGPT** (CNCF Sandbox, Apache-2.0): agentic LLM troubleshooting
   assistant equipped with Kubernetes, Prometheus, and Grafana Tempo toolsets.

**All results are measured on faults injected in the local `kubepilot-lab`
kind cluster with synthetic traffic, not on real production incidents.**

---

## 1. Evaluation Matrix (12 Representative Cases)

The 12 cases below cover all 8 cause classes from
[`eval/faults/catalog.v1.json`](../faults/catalog.v1.json) (including both
`dev` and `test` mechanisms where the observable signal differs in nature) plus
a no-fault decoy window:

| # | Case ID | Fault / Variant | Target Workload | Expected Cause (`catalog.v1.json`) | Key Observable Signal |
|---|---|---|---|---|---|
| 1 | `cpu-throttle--limit-300m` | `cpu-throttle` / `limit-300m` | `deployment/model-server` | `cpu_throttling` | In-place CPU limit resize to `300m`; ~93–96% CFS periods throttled; latency x6; `ResizeCompleted` event |
| 2 | `oomkill--noload-400mi` | `oomkill` / `noload-400mi` | `deployment/model-server` | `oom_killed` | `--load-mode none` + `400Mi` limit; container terminated `OOMKilled` (exit 137) + `BackOff` |
| 3 | `oomkill--app-oom-40mi` | `oomkill` / `app-oom-40mi` | `deployment/chat-api` | `oom_killed` | `chat-api` (not `model-server`) OOMKilled at `40Mi` limit + `BackOff`; no trace emitted while app is down |
| 4 | `unschedulable--node-taint` | `unschedulable` / `node-taint` | `deployment/model-server` | `unschedulable` | `NoSchedule` maintenance taint on both worker nodes; pod `Pending` with `FailedScheduling` (`3 node(s) had untolerated taint(s)`); pod spec unchanged |
| 5 | `unschedulable--pvc-unbound` | `unschedulable` / `pvc-unbound` | `deployment/model-server` | `unschedulable` | Missing `StorageClass` (`kubepilot-missing`); pod `Pending` with `FailedScheduling` (`unbound immediate PersistentVolumeClaims`) + PVC `ProvisioningFailed` |
| 6 | `readiness-fail--probe-path-404` | `readiness-fail` / `probe-path-404` | `deployment/model-server` | `readiness_failing` | Readiness probe path `/does-not-exist`; pod `Running`, `Ready=False`, `Warning Unhealthy` (HTTP 404) |
| 7 | `readiness-fail--initial-delay-600` | `readiness-fail` / `initial-delay-600` | `deployment/model-server` | `readiness_failing` | Readiness `initialDelaySeconds: 600`; pod `Running`, `Ready=False`, **0 `Unhealthy` events** (visible only in pod spec + `kube_pod_status_ready==0`) |
| 8 | `cold-start--model-download-10mbs` | `cold-start` / `model-download-10mbs` | `deployment/model-server` | `cold_start` | `initContainer` `fetch-model` runs ~47 s before main container starts; pod then becomes `Ready=True` and requests recover to HTTP 200 |
| 9 | `config-regression--threads-1` | `config-regression` / `threads-1` | `deployment/model-server` | `config_change` | Rollout (`ScalingReplicaSet`) sets `--threads 1`; 0 CFS throttling, low PSI waiting, 0 HTTP errors, but token/s drops ~35% and p50 latency rises ~1.6x |
| 10 | `host-contention--neighbor-pod-same-node` | `host-contention` / `neighbor-pod-same-node` | `deployment/model-server` | `host_cpu_contention` | Pod `noise/cpu-neighbor` burns ~7.5 CPU cores on same worker node; `model-server` PSI CPU waiting rises sharply with <1% CFS throttling |
| 11 | `app-delay--delay-2000ms` | `app-delay` / `delay-2000ms` | `deployment/chat-api` | `unknown_no_infrastructure_cause` | 2000 ms pre-inference sleep inside `chat-api` (`HTTP server span start -> GenAI client span start`); 0 K8s events, 0 throttling, normal cluster metrics |
| 12 | `nofault--decoy-same-node-oom` | `nofault` / `decoy-same-node-oom` | *(none — healthy `genai`)* | *(no fault / `unknown`)* | `genai` workloads are healthy and fast; unrelated pod `noise/decoy-oom` is `OOMKilled` + `BackOff` on the same worker node |

---

## 2. Fast Execution Protocol (60–90 s Short Windows)

Unlike P2's 13-minute full benchmark runs (`run.sh`), P3 baseline evaluations
use **short 60–90 s live fault windows** (or query existing windows already
stored in Prometheus and Tempo) so the full 12-case comparison completes in a
few minutes:

1. Inject the fault variant (or start the `nofault` decoy) on `kubepilot-lab`.
2. Wait 60–90 s for Prometheus (15 s scrape interval), Tempo, Kubernetes events,
   and Coroot's node agent / inspection loop to observe the active signal.
3. Query the tool under test (`why-slow`, Coroot HTTP API, HolmesGPT CLI) and
   archive its raw output under `eval/baselines/<tool>/<case_id>/`.
4. Immediately reset the fault (`reapply_declared` + fault-specific `reset`) and
   wait for `genai` pods to be `Ready` before moving to the next case.

---

## 3. Tool Configurations

### 3.1 KubePilot (`why-slow`)
- Invoked in read-only mode on the recorded run or live window (`why-slow --format json`).
- Deterministic pure rule engine (`internal/rules`), zero LLM calls.

### 3.2 Coroot Community Edition
- Deployed into `observability` (pinned image digest) connected to the lab's
  existing Prometheus (`http://prometheus-server.observability:80`)
  and `coroot-node-agent` DaemonSet.
- Queried via its HTTP API (`/api/project/.../overview/applications` and
  `/api/project/.../app/...:genai:Deployment:model-server` / `...:genai:Deployment:chat-api`)
  to record all active inspection statuses (`status`, `checks`, `reports`,
  `instances`, `deployments`).

### 3.3 HolmesGPT
- Executed via the pinned HolmesGPT container image with read-only access to:
  - the cluster (`kubeconfig`),
  - Prometheus (`http://prometheus-server.observability:80`),
  - Grafana Tempo (`http://tempo.observability:3200`).
- **Out-of-cluster self-hosted LLM by default:** To remain 100% self-hosted and
  ensure that faults injected into the in-cluster `genai/model-server` (such as
  `oomkill`, `unschedulable`, or `readiness-fail`) do **not** break HolmesGPT's
  own LLM backend, `lab/baselines/holmesgpt.sh` starts a dedicated host-side
  `llama-server` container serving the already-cached model
  (`.cache/models/qwen2.5-0.5b-instruct-q4_k_m.gguf`).
- **Configurable model override:** Environment variables (`HOLMES_MODEL`,
  `OPENAI_API_BASE`, `OPENAI_API_KEY`) allow running the same script against a
  larger local or hosted tool-calling model when available.
- Standardized prompt for every case:
  > `"Investigate why POST /chat requests to deployment/chat-api (which calls deployment/model-server) in namespace genai were slow or failing between <FROM_UTC> and <TO_UTC>. Ignore unrelated pods in namespace noise. State the root cause, the faulty component, and the supporting Kubernetes events, Prometheus metrics, or Tempo traces. If genai is healthy, state that no fault was found."`

---

## 4. Scoring Rubric

Each tool's output on each of the 11 fault cases is graded on a 4-level scale,
and the 1 no-fault decoy case is graded for false-positive resistance:

| Grade | Meaning |
|---|---|
| **`pass` (Exact Cause + Component)** | Identifies the specific root-cause mechanism (e.g. CFS CPU throttling, OOMKilled exit 137, FailedScheduling taint/PVC, readiness probe failure / `initialDelaySeconds`, initContainer cold-start, post-rollout config change, host/neighbor CPU contention, or application pre-inference delay with healthy infra) on the right `genai` workload. |
| **`partial` (Symptom or Component Only)** | Flags the right workload (`model-server` or `chat-api`) as unhealthy/slow or reports elevated latency / connection errors, but does not distinguish the underlying cause (e.g. reports generic latency or unavailability without naming the cause). |
| **`miss` (Not Detected or Wrong Cause)** | Reports all checks green (`OK`) during an active fault, blames the wrong component/cause, or fails to produce a diagnosis. |
| **`false_positive` (on `nofault` window)** | Blames a `genai` workload or reports an incident on `genai` when only the decoy pod in namespace `noise` is failing. |

Additional operational dimensions recorded in [`docs/p3-comparison.md`](../../docs/p3-comparison.md):
- **Determinism:** Whether repeated runs on the same telemetry snapshot produce identical output.
- **Investigation latency:** Wall-clock time (seconds) per diagnosis.
- **LLM / external dependency:** Whether an LLM with multi-step tool calling is required.
- **Strengths outside the GenAI benchmark ("Where they win"):** Capabilities provided by Coroot CE and HolmesGPT that KubePilot intentionally does not cover.

---

## 5. Directory Layout

```
eval/baselines/
  README.md                 this protocol and rubric
  coroot/
    summary.json            aggregated Coroot CE inspection results across the 12 cases
    <case_id>.json          raw Coroot API application inspection payload for <case_id>
  holmesgpt/
    summary.json            aggregated HolmesGPT results, latency, and tool-call counts
    <case_id>.json          raw HolmesGPT output, tool-call trace, and token counts for <case_id>
```

For the full side-by-side results and analysis, see [`docs/p3-comparison.md`](../../docs/p3-comparison.md).
