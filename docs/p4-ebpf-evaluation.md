# Phase P4 eBPF Evaluation: Grafana Beyla / OpenTelemetry eBPF Instrumentation (OBI)

- **Date:** 2026-10-23
- **Exit Criterion (`CLAUDE.md`):** *"eBPF evidence source, evaluated only after P3. Reuse OpenTelemetry eBPF Instrumentation (OBI) / Grafana Beyla as a source instead of writing a custom collector — Exit criterion: Shown to add accuracy on the benchmark first; otherwise dropped."*
- **Runner Script:** [`lab/ebpf/beyla.sh`](../lab/ebpf/beyla.sh)
- **Recorded Artifacts:** [`eval/ebpf/summary.json`](../eval/ebpf/summary.json) and per-run files under [`eval/ebpf/`](../eval/ebpf/)
- **Pinned Image:** `grafana/beyla:3.39.1@sha256:883f877b2af756f96a73c6790afa5d9133a967f98df09605ba784a37a2a0449f` (Grafana distribution of upstream `go.opentelemetry.io/obi`)

> **Methodological disclaimer:** All measurements in this report come from **faults injected in a single-cluster local `kind` lab** (`kubepilot-lab`) with one CPU-served model (`qwen2.5-0.5b-instruct-q4_k_m.gguf`), one GenAI application (`chat-api`), and synthetic traffic (`loadgen`), **not on real production incidents**.

---

## 1. Executive Summary & Architecture Decision

We evaluated **Grafana Beyla `v3.39.1`** (OpenTelemetry eBPF Instrumentation) deployed as a DaemonSet in `observability` exporting OTLP gRPC traces to `otel-collector -> tempo` under two operating modes across 3 representative cases (`nofault--quiet`, `cpu-throttle--limit-300m`, and `app-delay--delay-2000ms`):

1. **Mode A (`mode-a-sdk-plus-beyla` — Python OpenTelemetry SDK + Beyla eBPF active simultaneously)**:
   - Adding Beyla on top of the existing Python OpenTelemetry SDK (`opentelemetry-instrumentation-fastapi` + `opentelemetry-instrumentation-genai-openai`) yields **`+0.0%` accuracy gain** (KubePilot already achieves **`24 / 24` (`100.0%`)** on the held-out `SPLIT=test` set and **`13 / 13` (`100.0%`)** on `SPLIT=dev` without eBPF).
   - At the same time, socket-level eBPF capture floods Tempo with kubelet probe and Prometheus scrape spans (**73.5% to 78.0%** of `/api/search?limit=50` traces are `GET /healthz`, `GET /health`, or `GET /metrics`).
2. **Mode B (`mode-b-beyla-only` — zero-code eBPF only, Python SDK OTLP export disabled)**:
   - Even with `OTEL_EBPF_HTTP_ENRICHMENT_ENABLED=true`, `OTEL_EBPF_HTTP_OPENAI_ENABLED=true`, and `OTEL_EBPF_HTTP_OPENAI_COMPATIBLE_ENABLED=true`, **`0 / 32` chat traces emitted by Beyla carry `gen_ai.*` attributes** (`scope.name` is `""` and spans only carry generic HTTP attributes such as `http.request.method`, `url.path`, and `http.response.status_code`).
   - Worse, in FastAPI/Uvicorn where synchronous route handlers (`def chat()`) run in a worker threadpool, eBPF context propagation (`OTEL_EBPF_BPF_CONTEXT_PROPAGATION=all`) fails to link the incoming `POST /chat` server span on `:8000` (where the 2000 ms pre-inference delay occurs) to the outgoing `POST /v1/chat/completions` client span toward `:8080`. As a result, **`app-delay--delay-2000ms` regresses from `pass` (`diagnosed`) to `miss` (`unknown`)** (`-100%` on `unknown_no_infrastructure_cause`).

### Decision (Applying the Phase P4 Exit Criterion)

Per `CLAUDE.md` (*"Shown to add accuracy on the benchmark first; otherwise dropped"*):
- **Dropped from KubePilot's core engine (`internal/`):** We **do not add** an eBPF collector or Beyla dependency to KubePilot. The pre-registered diagnosis engine (`internal/rules`, tree `10d3c43a7038bd17aeda385f06b86c9a1ed8722e`) remains 100% unchanged, preserving KubePilot's strictly read-only, zero-privileged-DaemonSet security posture.
- **Documented as an optional upstream zero-code span producer:** For users whose GenAI application cannot be instrumented with an OpenTelemetry SDK, Grafana Beyla / OBI can be deployed upstream of Tempo to provide basic HTTP spans for infrastructure-driven faults (`cpu_throttling`, `oom_killed`, `unschedulable`, `readiness_failing`, `cold_start`, `host_cpu_contention`), with the explicit caveat that `gen_ai.*` attributes and intra-process pre-inference span gaps (`unknown_no_infrastructure_cause`) require SDK instrumentation.

---

## 2. Experimental Setup (`lab/ebpf/beyla.sh`)

Beyla `v3.39.1` was deployed as a `DaemonSet` in namespace `observability` targeting pods in namespace `genai` on ports `8000` (`chat-api`) and `8080` (`model-server`) with all OpenTelemetry / OpenAI enrichment features enabled:

```yaml
env:
  - name: BEYLA_OPEN_PORT
    value: "8000,8080"
  - name: OTEL_EBPF_OPEN_PORT
    value: "8000,8080"
  - name: BEYLA_KUBE_METADATA_ENABLE
    value: "true"
  - name: OTEL_EBPF_KUBE_METADATA_ENABLE
    value: "true"
  - name: BEYLA_BPF_CONTEXT_PROPAGATION
    value: "all"
  - name: OTEL_EBPF_BPF_CONTEXT_PROPAGATION
    value: "all"
  - name: OTEL_EBPF_HTTP_ENRICHMENT_ENABLED
    value: "true"
  - name: OTEL_EBPF_HTTP_OPENAI_ENABLED
    value: "true"
  - name: OTEL_EBPF_HTTP_OPENAI_COMPATIBLE_ENABLED
    value: "true"
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "http://otel-collector.observability.svc.cluster.local:4317"
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: "grpc"
```

Required container privileges on every node:
- `hostPID: true`
- `securityContext.privileged: true` (`CAP_BPF`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`, `CAP_NET_RAW`, `CAP_PERFMON`)
- Host `/sys/fs/cgroup` and `/sys/kernel/security` volume mounts

---

## 3. Measured Results (`eval/ebpf/summary.json`)

| Mode | Case ID | Tempo `/api/search?limit=50` Total | Chat Traces (`/chat` or `/v1/chat/completions`) | Probe / Scrape Noise Traces (`/health*`, `/metrics`) | Traces with `gen_ai.*` Attributes | KubePilot Live Diagnosis (`why-slow`) | Accuracy Delta vs SDK-Only Baseline |
|---|---|---:|---:|---:|---:|---|---|
| **Mode A (`mode-a-sdk-plus-beyla`)** | `nofault--quiet` | `50` | `13` (`26.0%`) | `37` (`74.0%`) | `4 / 13` (from Python SDK) | `status: unknown` (`0` false positives) | **`+0.0%`** |
| **Mode A (`mode-a-sdk-plus-beyla`)** | `cpu-throttle--limit-300m` | `50` | `11` (`22.0%`) | `39` (`78.0%`) | `4 / 11` (from Python SDK) | `status: diagnosed`, `top1: cpu_throttling` (`conf=0.92`, 47.7% CFS periods throttled, p50 x6.50) | **`+0.0%`** |
| **Mode A (`mode-a-sdk-plus-beyla`)** | `app-delay--delay-2000ms` | `34` | `9` (`26.5%`) | `25` (`73.5%`) | `3 / 9` (from Python SDK) | `status: diagnosed`, `top1: unknown_no_infrastructure_cause` (`conf=0.85`, HTTP->GenAI gap p50=2001 ms, 6/7 traces >= 500 ms) | **`+0.0%`** |
| **Mode B (`mode-b-beyla-only`)** | `nofault--quiet` | `42` | `12` (`28.6%`) | `30` (`71.4%`) | **`0 / 12`** (`0.0%`) | `status: unknown` (`0` false positives) | **`+0.0%`** |
| **Mode B (`mode-b-beyla-only`)** | `cpu-throttle--limit-300m` | `50` | `7` (`14.0%`) | `43` (`86.0%`) | **`0 / 7`** (`0.0%`) | `status: diagnosed`, `top1: cpu_throttling` (`conf=0.92`, 77.4% CFS periods throttled, p50 x3.29) | **`+0.0%`** |
| **Mode B (`mode-b-beyla-only`)** | `app-delay--delay-2000ms` | `50` | `13` (`26.0%`) | `37` (`74.0%`) | **`0 / 13`** (`0.0%`) | **`status: unknown` (`MISS`)** — pre-inference gap lost | **`-100.0%`** on `app-delay` |

---

## 4. Four Technical Findings from the Telemetry

### Finding 1: Zero Accuracy Gain Over the Pre-Registered P2 Engine (`+0.0%`)
On the pre-registered P2 benchmark ([`eval/README.md`](../eval/README.md)), KubePilot's 8 deterministic rules already achieve **`24 / 24` (`100.0%`)** on `SPLIT=test` and **`13 / 13` (`100.0%`)** on `SPLIT=dev` using:
- Kubernetes Pod status, spec, and Events (`oom_killed`, `unschedulable`, `readiness_failing`, `cold_start`),
- Prometheus cgroup/node/application metrics (`cpu_throttling`, `host_cpu_contention`, `config_change`), and
- OpenTelemetry SDK traces (`unknown_no_infrastructure_cause`).

Adding Beyla in Mode A provides no additional causal signal for any of the 8 fault classes.

### Finding 2: High Probe & Scrape Span Noise in Tempo (`71.4%`–`86.0%`)
Because Beyla attaches at the socket/kernel layer on ports `8000` and `8080`, every kubelet readiness/liveness probe (`GET /healthz`, `GET /health` every 5 s) and every Prometheus scrape (`GET /metrics`) generates a 3-span trace (`GET /health`, `in queue`, `processing`, duration `0.2–1.8 ms`). When querying Tempo's search API (`/api/search?limit=50`) over a 45 s window, **71.4% to 86.0%** of returned traces are health-check or scrape noise unless additional route exclusion rules are configured and maintained in Beyla.

### Finding 3: Absence of `gen_ai.*` Semantic Attributes on eBPF Spans (`0 / 32` Chat Traces)
In Mode B (`mode-b-beyla-only`), inspecting the full OTLP trace payloads in [`eval/ebpf/summary.json`](../eval/ebpf/summary.json) (e.g. trace `7e9eec4e589c5472952bfcccf1cd8c89` on `POST /v1/chat/completions`) shows that Beyla emits spans with `scope.name: ""` and only generic HTTP/network attributes:

```json
[
  "http.request.method",
  "http.response.status_code",
  "network.peer.address",
  "network.peer.port",
  "network.protocol.version",
  "server.address",
  "server.port",
  "url.full",
  "url.scheme"
]
```

None of the OpenTelemetry GenAI semantic attributes (`gen_ai.operation.name`, `gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`) are populated by Beyla `v3.39.1` on the unencrypted HTTP `POST /v1/chat/completions` call between `chat-api` and `model-server`.

### Finding 4: Broken Parent-Child Trace Correlation Across FastAPI Worker Threads (`app-delay` Miss)
In `chat-api`, FastAPI runs the synchronous endpoint `def chat()` in an AnyIO worker thread, which executes `time.sleep(2.0)` (`CHAT_PRE_DELAY_MS=2000`) before invoking the OpenAI HTTP client toward `model-server:8080`:
- **With the Python OpenTelemetry SDK (Mode A, trace `a45841bed378d1796cf1b256f186a0f2`)**: OpenTelemetry's Python `contextvars` propagation links the root `POST /chat` server span (`start_ns = 1792720385786026071`, duration `2483 ms`) to the child `chat Qwen2.5-0.5B-Instruct-Q4_K_M.gguf` client span (`start_ns = 1792720387786790566`, duration `481 ms`). KubePilot computes `clientStart - serverStart = 2000.8 ms` (`p50 = 2001 ms` across 6/7 traces) and accurately diagnoses `unknown_no_infrastructure_cause` on `deployment/chat-api`.
- **With Beyla eBPF only (Mode B, trace `7e9eec4e589c5472952bfcccf1cd8c89`)**: Beyla emits the incoming `POST /chat` trace (`5319 ms` total) and the outgoing `POST /v1/chat/completions` trace (`4875 ms`, consisting only of `chat-api` `SPAN_KIND_CLIENT` -> `model-server` `SPAN_KIND_SERVER` with `0.15 ms` network gap) as **separate unlinked traces**. Because no single trace contains both the incoming `:8000` server span and the outgoing `:8080` client span, the 2000 ms pre-inference delay inside `chat-api` is invisible to trace-gap analysis, causing a **`miss` (`status: unknown`)**.

---

## 5. Reproducing the Phase P4 Evaluation

```sh
# Run both Mode A (SDK + Beyla) and Mode B (Beyla only) across the 3 cases,
# write eval/ebpf/summary.json, and tear down the Beyla DaemonSet:
bash lab/ebpf/beyla.sh eval
bash lab/ebpf/beyla.sh down
```
