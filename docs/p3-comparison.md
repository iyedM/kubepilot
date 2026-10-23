# Phase P3 Baseline Comparison: KubePilot vs Coroot CE vs HolmesGPT

- **Date:** 2026-10-23
- **Protocol & Rubric:** [`eval/baselines/README.md`](../eval/baselines/README.md)
- **Recorded Artifacts:**
  - Coroot Community Edition: [`eval/baselines/coroot/`](../eval/baselines/coroot/) ([`summary.json`](../eval/baselines/coroot/summary.json))
  - HolmesGPT: [`eval/baselines/holmesgpt/`](../eval/baselines/holmesgpt/) ([`summary.json`](../eval/baselines/holmesgpt/summary.json))
- **Runner Scripts:** [`lab/baselines/coroot.sh`](../lab/baselines/coroot.sh) and [`lab/baselines/holmesgpt.sh`](../lab/baselines/holmesgpt.sh)

> **Methodological disclaimer:** All results in this report are measured on **faults injected in a single-cluster local `kind` lab** (`kubepilot-lab`) with one CPU-served model (`qwen2.5-0.5b-instruct-q4_k_m.gguf`), one GenAI application (`chat-api`), and synthetic traffic (`loadgen`), **not on real production incidents**.

---

## 1. Executive Summary

In Phase P0 ([`docs/landscape.md`](landscape.md)), two open-source projects were identified as the closest alternatives to KubePilot (`why-slow`):

1. **Coroot Community Edition** (`ghcr.io/coroot/coroot:1.28.1` + `coroot-node-agent:1.37.0`, Apache-2.0): an eBPF + Prometheus + OpenTelemetry observability platform with a deterministic inspection engine (`CPU`, `Memory`, `Instances`, `Deployments`, `SLO`, `Net`, `Storage`).
2. **HolmesGPT** (`us-central1-docker.pkg.dev/genuine-flight-317411/devel/holmes:0.45.0`, CNCF Sandbox, Apache-2.0): an agentic LLM troubleshooting assistant equipped with Kubernetes, Prometheus, and Grafana Tempo toolsets.

We evaluated all three tools on the same **12 representative cases** covering all 8 cause classes in [`eval/faults/catalog.v1.json`](../eval/faults/catalog.v1.json) (11 fault cases + 1 healthy window with an active same-node OOM decoy in namespace `noise`). To keep the evaluation 100% self-hosted on the lab host and ensure that faults injected into `genai/model-server` did not break HolmesGPT's own LLM backend, HolmesGPT was paired by default with a dedicated host-side `llama-server` container serving the lab's pinned GGUF model (`qwen2.5-0.5b-instruct-q4_k_m.gguf`, 32k context window).

### Aggregate Scores Across the 12 Cases

| Tool | Version / Configuration | Fault Cases (`n = 11`): `pass` | Fault Cases (`n = 11`): `partial` | Fault Cases (`n = 11`): `miss` | Healthy Decoy (`n = 1`): `healthy_ok` / `false_positive` | Typical Query Time |
|---|---|---|---|---|---|---|
| **KubePilot (`why-slow`)** | Pre-registered engine (`17291b1`, tree `10d3c43a7038`) | **`11 / 11`** (`100.0%`) | `0 / 11` (`0.0%`) | `0 / 11` (`0.0%`) | **`1 / 1` `healthy_ok`** (`0` FP) | **`< 1 s`** (`~0.2–0.5 s`) |
| **Coroot Community Edition** | `v1.28.1` + `coroot-node-agent v1.37.0` (15 s Prometheus scrape) | **`4 / 11`** (`36.4%`) | **`4 / 11`** (`36.4%`) | **`3 / 11`** (`27.3%`) | **`1 / 1` `healthy_ok`** (`0` FP) | **`< 0.2 s`** HTTP API (after `~15–45 s` scrape/inspection window) |
| **HolmesGPT (self-hosted 0.5B LLM)** | `v0.45.0` + local `qwen2.5-0.5b-instruct-q4_k_m` (`--max-steps 5`) | **`0 / 11`** (`0.0%`) | **`0 / 11`** (`0.0%`) | **`11 / 11`** (`100.0%`) | **`1 / 1` `healthy_ok`** (`0` FP) | **`31–164 s`** (`~52k` prompt tokens on CPU) |

---

## 2. Case-by-Case Comparison (12 Cases)

Grades follow the 4-level rubric defined in [`eval/baselines/README.md`](../eval/baselines/README.md):
- **`pass`**: identifies the exact root-cause mechanism and target `genai` workload.
- **`partial`**: flags the right `genai` workload as unhealthy/down or slow, but only reports the symptom without isolating the underlying cause.
- **`miss`**: reports all checks `ok` during an active fault, blames the wrong component, or fails to produce a diagnosis.
- **`healthy_ok` / `false_positive`** (on case #12 `nofault--decoy-same-node-oom`): whether the tool resists blaming `genai` when `noise/decoy-oom` crashes on the same worker node.

| # | Case ID | Expected Cause (`catalog.v1.json`) | KubePilot (`why-slow`) | Coroot CE (`v1.28.1`) | HolmesGPT (`v0.45.0` + local `Qwen2.5-0.5B`) |
|---|---|---|---|---|---|
| 1 | `cpu-throttle--limit-300m` | `cpu_throttling` (`deployment/model-server`) | **`pass`** (`conf=0.92`): `cpu_throttling` on `model-server` (93.3% CFS periods throttled, p50 latency x6.61, `ResizeCompleted`) | **`pass`**: `CPUContainer` check fired (`warning: high CPU utilization of 1 container`), recording `limit=0.3` cores and CFS `Throttled time` chart | **`miss`** (`163.6 s`, 2 LLM calls, 33k prompt tokens): called `tempo_fetch_traces_comparative_sample` with invalid TraceQL syntax (`service_name="chat-api" and ...`), got HTTP 400 parse error and stopped |
| 2 | `oomkill--noload-400mi` | `oom_killed` (`deployment/model-server`) | **`pass`** (`conf=0.98`): `oom_killed` on `model-server` (terminated `OOMKilled` exit 137 at 400 MiB limit + `BackOff`) | **`partial`**: `InstanceAvailability` fired (`warning: no instances available`) and `Instances` reported `down (waiting:CrashLoopBackOff)`, but `MemoryOOM` check stayed `ok` | **`miss`** (`70.5 s`, 5 LLM calls, 52k prompt tokens): called `fetch_pod_logs` 4 times with Deployment names (`pod_name="chat-api"`, `"model-server"`), got `NotFound` and hit duplicate-call guard |
| 3 | `oomkill--app-oom-40mi` | `oom_killed` (`deployment/chat-api`) | **`pass`** (`conf=0.98`): `oom_killed` on `chat-api` (terminated `OOMKilled` exit 137 at 40 MiB limit + `BackOff`) | **`partial`**: `InstanceAvailability` fired on `chat-api` and `Instances` reported `down (waiting:CrashLoopBackOff)`, but `MemoryOOM` stayed `ok` | **`miss`** (`31.4 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`pods "chat-api" not found`) |
| 4 | `unschedulable--node-taint` | `unschedulable` (`deployment/model-server`) | **`pass`** (`conf=0.96`): `unschedulable` on `model-server` (`Pending` + `FailedScheduling: 3 node(s) had untolerated taint(s)`) | **`pass`**: `InstanceAvailability` fired (`no instances available`) and `Instances` reported `down (pending, not scheduled)` on the new `model-server` pod | **`miss`** (`71.1 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 5 | `unschedulable--pvc-unbound` | `unschedulable` (`deployment/model-server`) | **`pass`** (`conf=0.96`): `unschedulable` on `model-server` (`Pending` + `FailedScheduling` / unbound PVC `model-scratch`) | **`pass`**: `InstanceAvailability` fired (`no instances available`) and `Instances` reported `down (pending, not scheduled)` | **`miss`** (`70.3 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 6 | `readiness-fail--probe-path-404` | `readiness_failing` (`deployment/model-server`) | **`pass`** (`conf=0.94`): `readiness_failing` on `model-server` (`Running`, `Ready=False`, `Unhealthy: HTTP probe failed with statuscode: 404`) | **`partial`**: `InstanceAvailability` fired and `Instances` reported `down (readiness probe failed)`, without surfacing the HTTP 404 probe error message | **`miss`** (`70.1 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 7 | `readiness-fail--initial-delay-600` | `readiness_failing` (`deployment/model-server`) | **`pass`** (`conf=0.94`): `readiness_failing` on `model-server` (`Running`, `Ready=False`, pod spec `readinessProbe.initialDelaySeconds=600`) | **`partial`**: `InstanceAvailability` fired and `Instances` reported `down (readiness probe failed)`, without identifying `initialDelaySeconds: 600` | **`miss`** (`70.7 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 8 | `cold-start--model-download-10mbs` | `cold_start` (`deployment/model-server`) | **`pass`** (`conf=0.93`): `cold_start` on `model-server` (`initContainer` `fetch-model` delayed main container start by 49 s before becoming `Ready`) | **`miss`**: all checks returned `ok` once the pod became `Ready`; no inspection attributed the startup outage window to `initContainer` `fetch-model` | **`miss`** (`70.8 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 9 | `config-regression--threads-1` | `config_change` (`deployment/model-server`) | **`pass`** (`conf=0.88`): `config_change` on `model-server` (new rollout with `--threads 1`, 0 throttling, low PSI, token/s drop, p50 latency x1.63) | **`miss`**: all checks returned `ok`; `Deployments` inspection skipped (`"Not enough data due to the lifetime < 30 minutes"`) and ignores `--threads 1` / `llamacpp:predicted_tokens_seconds` | **`miss`** (`68.6 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 10 | `host-contention--neighbor-pod-same-node` | `host_cpu_contention` (`deployment/model-server`) | **`pass`** (`conf=0.90`): `host_cpu_contention` on `model-server` (PSI CPU waiting surge, CFS throttled `< 0.01`, neighbor `noise/cpu-neighbor` burning 7.56 cores on same node) | **`pass`**: `CPUNode` check fired (`warning: high CPU utilization of 1 node`) and `CPU consumers on kubepilot-lab-worker2` chart highlighted `cpu-neighbor` | **`miss`** (`70.1 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 11 | `app-delay--delay-2000ms` | `unknown_no_infrastructure_cause` (`deployment/chat-api`) | **`pass`** (`conf=0.85`): `unknown_no_infrastructure_cause` on `chat-api` (pre-inference trace gap `HTTP server span -> GenAI client span` p50=2001 ms, 51/51 traces >= 500 ms, healthy infra) | **`miss`**: all checks on `chat-api` and `model-server` returned `ok`; no inspection compares HTTP server span start to `gen_ai.*` client span start | **`miss`** (`71.7 s`, 5 LLM calls, 52k prompt tokens): 4 failed `fetch_pod_logs` calls (`NotFound`) |
| 12 | `nofault--decoy-same-node-oom` | `none` (healthy `genai`, OOM decoy in `noise`) | **`healthy_ok`**: `status: healthy` (`causes: []`), explicitly ignoring `noise/decoy-oom` on the same node | **`healthy_ok`**: all checks on `genai/model-server` and `genai/chat-api` stayed `ok` (isolated `noise/decoy-oom` to its own application) | **`healthy_ok`** (`71.1 s`): did not blame `genai` (though its 4 `fetch_pod_logs` calls also failed with `NotFound`) |

---

## 3. Operational Dimensions Comparison

| Dimension | KubePilot (`why-slow`) | Coroot Community Edition (`v1.28.1`) | HolmesGPT (`v0.45.0`) |
|---|---|---|---|
| **Core diagnosis mechanism** | Deterministic pure functions (`internal/rules`) over a point-in-time snapshot (`Snapshot`) | Deterministic per-application health checks (`SLO`, `CPU`, `Memory`, `Instances`, `Deployments`, `Net`, `Storage`) | ReAct-style LLM agent loop invoking Kubernetes, Prometheus, and Tempo toolsets |
| **Determinism** | **100% deterministic** (identical snapshot always produces identical causes, confidence scores, and evidence) | **Deterministic** for a given Prometheus / node-agent time window | **Non-deterministic** (depends on LLM sampling, prompt wording, and model capability) |
| **Diagnosis latency** | **`0.2–0.5 s`** offline or live CLI | **`< 0.2 s`** API response once Prometheus / `coroot-node-agent` series are scraped (`15–45 s` lag) | **`31–164 s`** per investigation on 12-thread CPU (`~33k–52k` prompt tokens across 2–5 turns) |
| **Cluster footprint & privileges** | **Zero in-cluster footprint** for CLI (`why-slow`); strictly read-only RBAC (`get`/`list` on Pods, Events, Nodes, PVCs) | Stateful server (`coroot` Deployment) + **privileged `coroot-node-agent` DaemonSet** (`hostPID`, `sysfs`, `debugfs`/`tracefs` mounts for eBPF) | Stateless CLI/Pod; read-only `kubectl`/Prometheus/Tempo access, plus an external or self-hosted **LLM inference server** |
| **LLM dependency** | **None** in the diagnosis path | **None** in Community Edition checks (Coroot's AI RCA is a paid Cloud/Enterprise feature) | **Mandatory** (requires a tool-calling LLM capable of handling `~16.4k` tokens of tool schemas per turn) |
| **GenAI telemetry awareness (`gen_ai.*`)** | **Native** (versioned adapter in `internal/genai` for `gen_ai.*` spans/metrics, `llamacpp:predicted_tokens_seconds`, and HTTP-to-GenAI pre-inference span gap) | **None** in CE `v1.28.1` (treats `chat-api` and `model-server` as generic HTTP services) | **Generic** (can query Tempo/Prometheus if the LLM constructs the right TraceQL/PromQL queries, but has no built-in `gen_ai.*` rules) |

---

## 4. Technical Deep-Dive on Observed Baseline Behavior

### 4.1 Coroot Community Edition (`4 pass`, `4 partial`, `3 miss`, `1 healthy_ok`)

Coroot CE was deployed into `observability` alongside its `coroot-node-agent` DaemonSet with Prometheus `scrape_interval` set to `15s` ([`lab/baselines/coroot.sh`](../lab/baselines/coroot.sh)). Querying `/api/project/<id>/app/<app_id>` returned Coroot's structured inspection reports:

1. **Where Coroot CE succeeded (`4 pass`, `1 healthy_ok`)**:
   - **`cpu-throttle--limit-300m` (`pass`)**: Coroot's `CPU` report fired `CPUContainer` (`warning: high CPU utilization of 1 container`), reported `99% (limit: 0.3 cores)` on `model-server`, and included the `Throttled time, seconds/second` chart.
   - **`unschedulable--node-taint` and `unschedulable--pvc-unbound` (`pass`)**: Coroot's `Instances` report fired `InstanceAvailability` (`warning: no instances available`) and its `Instances` status table explicitly reported `down (pending, not scheduled)` on the pending `model-server` pod (though the check summary itself does not state whether scheduling failed due to a node taint or an unbound PVC).
   - **`host-contention--neighbor-pod-same-node` (`pass`)**: Coroot's `CPU` report fired `CPUNode` (`warning: high CPU utilization of 1 node`) and the `CPU consumers on kubepilot-lab-worker2` chart clearly identified `cpu-neighbor` consuming the node's CPU cores alongside `model-server`.
   - **`nofault--decoy-same-node-oom` (`healthy_ok`)**: Coroot cleanly scoped the `decoy-oom` crash to `noise:Deployment:decoy-oom`, leaving all checks on `genai:Deployment:model-server` and `genai:Deployment:chat-api` at `ok`.

2. **Why `oomkill` and `readiness-fail` scored `partial` (`4 partial`)**:
   - **`oomkill--noload-400mi` and `oomkill--app-oom-40mi`**: Coroot flagged `InstanceAvailability` (`warning: no instances available`) and showed `down (waiting:CrashLoopBackOff)` on the right deployment (`model-server` or `chat-api`). However, its `MemoryOOM` check (`"Out of memory events: 0"`) remained `ok` (`status: "ok"`). In `kind` (nested containers sharing the host kernel cgroups), `coroot-node-agent`'s `container_oom_kills_total` counter did not attribute the cgroup OOM event before the pod entered `CrashLoopBackOff`, whereas KubePilot inspects `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}` and the pod's `lastState.terminated.reason == "OOMKilled"` (`exitCode: 137`).
   - **`readiness-fail--probe-path-404` and `readiness-fail--initial-delay-600`**: Coroot's `Instances` table reported `down (readiness probe failed)`, identifying the right symptom and workload, but did not inspect Kubernetes `Unhealthy` events or the Pod spec's `readinessProbe.initialDelaySeconds: 600` to explain *why* readiness was failing.

3. **Why `cold-start`, `config-regression`, and `app-delay` scored `miss` (`3 miss`)**:
   - **`cold-start--model-download-10mbs`**: Once the `fetch-model` `initContainer` finished and `model-server` became `Ready`, Coroot's `InstanceAvailability` returned to `ok`. Coroot CE has no inspection that attributes an earlier availability gap to slow `initContainer` completion (`state.terminated.finishedAt - state.terminated.startedAt`).
   - **`config-regression--threads-1`**: Passing `--threads 1` to `llama-server` drops token generation speed (`llamacpp:predicted_tokens_seconds`) by ~35% and increases latency ~1.6x while keeping CPU usage well below the 4-core limit, with 0 CFS throttling and 0 HTTP errors. All Coroot infrastructure checks stayed `ok`, and its `Deployments` inspection reported `"Not enough data due to the lifetime < 30 minutes"`.
   - **`app-delay--delay-2000ms`**: Injecting a 2000 ms pre-inference sleep inside `chat-api` before calling `model-server` produces zero infrastructure anomalies. Coroot CE's `SLOLatency` check was not configured with a custom GenAI latency threshold and does not compute the intra-trace gap between the HTTP server span start and the `gen_ai.*` client span start.

### 4.2 HolmesGPT (`0 pass`, `0 partial`, `11 miss`, `1 healthy_ok` with self-hosted 0.5B LLM)

HolmesGPT (`v0.45.0`) was run with four read-only toolsets enabled (`kubernetes/core`, `kubernetes/logs`, `prometheus/metrics` with `tool_calls_return_data: true`, and `grafana/tempo`) against a dedicated host-side `llama-server` (`kp-holmes-llm`) serving `qwen2.5-0.5b-instruct-q4_k_m.gguf` ([`lab/baselines/holmesgpt.sh`](../lab/baselines/holmesgpt.sh)). Every run's full tool-call trace and token usage are recorded in [`eval/baselines/holmesgpt/summary.json`](../eval/baselines/holmesgpt/summary.json):

1. **16,371 prompt tokens on Turn 1**:
   - HolmesGPT's system prompt plus the JSON schemas of the ~35 tools across `kubernetes/core`, `kubernetes/logs`, `prometheus/metrics`, and `grafana/tempo` consume **~16,371 prompt tokens** on the initial LLM call alone.
   - Across a 2-to-5-turn investigation, each case required **33,031 to 52,167 prompt tokens** and took **31.4 to 163.6 seconds** on 8 CPU threads (even with `llama.cpp` KV prefix caching active across turns).
2. **Multi-step tool-calling breakdown on a small self-hosted model (`Qwen2.5-0.5B-Instruct`)**:
   - In **`cpu-throttle--limit-300m`**, the model chose `tempo_fetch_traces_comparative_sample` (`grafana/tempo`), but generated an invalid TraceQL filter (`service_name="chat-api" and name="model-server" and status="Running"` instead of `{ resource.service.name = "chat-api" }`). Tempo returned HTTP 400 (`parse error`), and the model immediately concluded that the failure was a TraceQL syntax error without querying Kubernetes or Prometheus.
   - In the **other 11 cases**, instead of first calling `kubectl_get_by_kind_in_namespace` (`kubernetes/core`) to list pods in `genai` and discover their ReplicaSet pod names (`model-server-<rs>-<hash>`, `chat-api-<rs>-<hash>`), the 0.5B model jumped directly to `fetch_pod_logs` (`kubernetes/logs`) passing the Deployment names `"chat-api"` and `"model-server"` as `pod_name`.
   - Kubernetes returned `error from server (NotFound): pods "chat-api" not found in namespace "genai"`. Unable to recover from the tool error, the 0.5B model repeated the exact same `fetch_pod_logs` calls until HolmesGPT's loop guard (`Refusing to run this tool call because it has already been called during this session with the exact same parameters`) and `--max-steps 5` terminated the investigation.
3. **Why this matters for KubePilot's design thesis**:
   - HolmesGPT is engineered for frontier tool-calling LLMs (e.g. GPT-4o, Claude 3.5/4 Sonnet) that can attend over 16k+ tokens of tool definitions, plan a multi-step discovery workflow (`list pods -> describe pod / events -> query Prometheus / Tempo`), and write syntactically valid PromQL and TraceQL on the fly.
   - In an air-gapped or CPU-only self-hosted environment where only a small local model is available, an LLM agent loop fails at tool orchestration and takes over a minute per query—whereas KubePilot's deterministic rules engine (`why-slow`) diagnoses all 12 cases in **`< 1 second`** with zero LLM dependency.

---

## 5. Honest Assessment: Where Each Tool Wins

### Where KubePilot (`why-slow`) Wins
- **Deterministic, sub-second GenAI request diagnosis (`< 1 s`)**: Correlates OpenTelemetry `gen_ai.*` traces, Kubernetes events, pod specs, and Prometheus/`llama.cpp` metrics in pure Go without an LLM or a privileged DaemonSet.
- **GenAI-specific and post-rollout root causes**: Accurately diagnoses `cold_start` (`initContainer` model fetch delay even after the pod recovers), `config_change` (`--threads 1` rollout degrading `llamacpp:predicted_tokens_seconds` without resource saturation), `readiness_failing` via spec misconfiguration (`initialDelaySeconds: 600` with zero `Unhealthy` events), and `unknown_no_infrastructure_cause` (isolating a 2000 ms pre-inference delay inside `chat-api` via the HTTP-to-GenAI span gap).
- **Zero-LLM, strictly read-only self-hosting**: Runs anywhere a standard `kubeconfig` and Prometheus/Tempo HTTP endpoints are reachable, with no API keys, no GPU requirement, and a pre-registered benchmark (`24/24` on `SPLIT=test`).

### Where Coroot Community Edition Wins
- **Zero-code, cluster-wide observability via eBPF**: Through `coroot-node-agent`, Coroot automatically discovers service-to-service topology, TCP latency, DNS errors, storage I/O latency, and CPU/memory profiles across **every** workload in the cluster (databases, message queues, caches, third-party binaries) without requiring OpenTelemetry SDK instrumentation.
- **Continuous monitoring and turnkey Web UI**: Provides always-on service maps, SLO dashboards, flamegraphs, and node/container resource charts out of the box—capabilities that a point-in-time CLI (`why-slow`) does not provide in P2/P3.
- **Strong out-of-the-box infrastructure checks**: Directly caught `cpu-throttle--limit-300m`, both `unschedulable` variants, and `host-contention--neighbor-pod-same-node` (including identifying the noisy neighbor pod on the worker node) with zero custom rule configuration.

### Where HolmesGPT Wins (When Paired with a Frontier or Large Tool-Calling LLM)
- **Open-ended, cross-domain troubleshooting**: Ships with 30+ toolsets covering ecosystems that KubePilot does not inspect (application logs via Loki/`kubectl logs`, Helm releases, ArgoCD sync states, Cilium/Istio network policies, Alertmanager, PagerDuty, cloud provider APIs, and GitHub commit history).
- **Unanticipated failure modes**: KubePilot's rule registry only diagnoses its closed catalog of 8 cause classes (`catalog.v1.json`) and returns `unknown` otherwise. When backed by a frontier model (e.g. GPT-4o or Claude Sonnet via `HOLMES_MODEL` / `OPENAI_API_KEY`), HolmesGPT can reason ad hoc over novel failure modes, read stack traces in container logs, and answer interactive follow-up questions in natural language.

---

## 6. Reproducing the P3 Evaluation

Both baseline runners are automated, linted (`make lint`), and clean up their resources via `down`:

```sh
# 1. Coroot Community Edition (deploys Coroot CE + node-agent, evaluates 12 cases, tears down)
bash lab/baselines/coroot.sh eval
bash lab/baselines/coroot.sh down

# 2. HolmesGPT with default self-hosted host-side llama-server (qwen2.5-0.5b-instruct)
bash lab/baselines/holmesgpt.sh eval
bash lab/baselines/holmesgpt.sh down

# 3. Optional: HolmesGPT with an external or larger OpenAI-compatible tool-calling model
HOLMES_USE_LOCAL_LLM=0 \
  HOLMES_MODEL="openai/gpt-4o" \
  OPENAI_API_KEY="<your-key>" \
  bash lab/baselines/holmesgpt.sh eval
```
