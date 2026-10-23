# DevSecOps Threat Model (STRIDE), Empirical Limitations & Fair Tool Positioning

This document details:
1. The **STRIDE Security Threat Model** governing KubePilot's read-only architecture and GitOps remediation flow,
2. The **Empirical Limitations & Out-of-Distribution (OOD) Failure Modes** of the benchmark, and
3. A **Fair Comparative Positioning** explaining when engineers should choose **Coroot CE**, **HolmesGPT**, or **KubePilot**.

---

## 1. DevSecOps STRIDE Threat Model

Diagnostic and AIOps tools deployed inside Kubernetes clusters are high-value targets because they typically request cluster-wide permissions and inspect application telemetry. KubePilot mitigates each STRIDE threat category by design:

| Threat Category | Attack Scenario in Kubernetes AIOps | KubePilot Mitigation & Control | Verification |
|---|---|---|---|
| **S — Spoofing** | An unauthorized service triggers fake alerts or impersonates the Kubernetes API client. | Uses standard `kubeconfig` / in-cluster ServiceAccount bearer tokens with TLS verification; webhook endpoints validate payload structure and never execute cluster mutations. | [`charts/kubepilot/templates/serviceaccount.yaml`](../charts/kubepilot/templates/serviceaccount.yaml) |
| **T — Tampering** | A compromised AIOps agent modifies `Deployments`, deletes `Pods`, or applies hallucinated `kubectl patch` commands during an incident. | **Zero Write RBAC (`get`, `list`, `watch` only)** + `readOnlyRootFilesystem: true`. Remediation patches are generated strictly as read-only text artifacts requiring human GitOps approval. | [`charts/kubepilot/templates/rbac.yaml`](../charts/kubepilot/templates/rbac.yaml) |
| **R — Repudiation** | An LLM-based troubleshooting agent changes its diagnosis between runs, leaving no auditable trail during a post-mortem. | **Deterministic Go Rule Engine** (`internal/rules` tree frozen at `201576af596e12588aa27aa2e3586dd576af5fd8`). Every finding links to timestamped UTC evidence items and can be exported as a signed/portable `.tar.gz` bundle. | `GET /api/v1/incidents/{id}/bundle` |
| **I — Information Disclosure** | Sensitive user prompts, PII, or LLM completions leak into the troubleshooting database or third-party LLM APIs. | **Zero Prompt Capture at Ingestion** (`internal/genai/adapter.go` strips `gen_ai.prompt` and `gen_ai.completion` attributes before constructing `Snapshot`) + automated audit check `prompt_privacy_redaction`. | `GET /api/v1/incidents/{id}/security` |
| **D — Denial of Service** | An observability agent consumes excessive CPU/RAM or floods Tempo with kubelet health-probe spans. | Zero-dependency Go binary (`< 30 MiB` RSS), bounded in-memory incident ring buffer, and **no eBPF DaemonSet** (dropped in Phase P4 after measuring `73.5%–78.0%` probe span noise). | [`docs/p4-ebpf-evaluation.md`](p4-ebpf-evaluation.md) |
| **E — Elevation of Privilege** | A suggested remediation YAML introduces `privileged: true`, `hostNetwork: true`, `hostPath`, or root execution (`runAsUser: 0`). | Built-in **PSS-Restricted Patch Guard (`BuildSecurityAudit`)** statically inspects both the live workload specs and the generated Strategic Merge Patch before display. | [`internal/server/features.go`](../internal/server/features.go) |

---

## 2. Empirical Scope & Out-of-Distribution (OOD) Limitations

We explicitly avoid overclaiming generalization beyond what is measured in this repository:

1. **Sample Size & Confidence Intervals (`n = 45` test runs, `n = 19` dev runs):**
   - While KubePilot achieves `24/24` (`100.0%`) top-1 accuracy and `0/21` (`0.0%`) false positives on the held-out `test` split, the **Wilson 95% confidence interval** on `n = 24` fault runs is **`[86.2%, 100.0%]`**, and on `n = 21` healthy/decoy runs is **`[0.0%, 15.5%]`**.
2. **Single-Cluster Lab Topology vs. Multi-Tenant Production:**
   - All 64 recorded runs were collected on a 3-node `kind` cluster (`kubepilot-lab`) serving a single quantized CPU model (`qwen2.5-0.5b-instruct-q4_k_m.gguf`) via `llama.cpp`.
   - Production GPU clusters (`vLLM`, `TGI`, `TensorRT-LLM` with `DCGM` metrics, multi-node NCCL tensor parallelism, and Istio/Cilium service meshes) introduce failure modes not present in `catalog.v1.json`.
3. **Closed 8-Cause Catalog & Out-of-Distribution Abstention:**
   - If an incident is caused by a fault **outside** the 8-rule catalog—such as CNI packet drops, CoreDNS latency, upstream S3 rate limiting, or GPU thermal throttling—KubePilot will **not** invent a 9th cause:
     - If infrastructure metrics in `genai` remain healthy while pre-inference application latency spikes, it reports `unknown_no_infrastructure_cause`.
     - Otherwise, it abstains with `status: "unknown"`, signaling that manual or open-ended investigation is required.
4. **Telemetry Partial Outage Behavior (Graceful Degradation):**
   - If **Grafana Tempo** is unreachable, `unknown_no_infrastructure_cause` (which relies on OTel span decomposition) cannot fire, but the 7 Kubernetes + Prometheus rules (`cpu_throttling`, `oom_killed`, `unschedulable`, `readiness_failing`, `cold_start`, `config_change`, `host_cpu_contention`) continue to operate normally.
   - If **Prometheus** is unreachable, rate/ratio rules (`cpu_throttling`, `host_cpu_contention`) degrade gracefully while Kubernetes API/Event rules (`oom_killed`, `unschedulable`, `readiness_failing`, `cold_start`) remain active.
   - This per-channel status is exposed live on every incident via `GET /api/v1/incidents/{id}/runbook` (`signals` matrix).

---

## 3. Fair Positioning: KubePilot vs. Coroot CE vs. HolmesGPT

KubePilot is **not** a replacement for full-cluster APM suites or frontier-LLM chat assistants; each tool excels in a distinct operational regime:

| Operational Need | Recommended Tool | Why |
|---|---|---|
| **Sub-second, zero-hallucination triage of GenAI serving incidents (`CPU throttling`, `OOM`, `Pending`, `Readiness`, `Cold Start`, `PSI contention`, `Pre-Inference App Delay`) with FinOps ROI & PSS-Restricted GitOps patches** | **KubePilot** | Purpose-built correlation of `gen_ai.*` OTel spans + K8s state + Prometheus + OpenCost in `< 0.5 s` with `0` LLM calls and `0` privileged DaemonSets. |
| **Zero-code, continuous cluster-wide service maps, TCP latency heatmaps, database query profiling (Postgres/Redis), and node flamegraphs** | **Coroot Community Edition** | Coroot's eBPF `node-agent` automatically discovers every process and network socket across all namespaces without requiring OpenTelemetry SDK instrumentation. |
| **Open-ended, conversational investigation of novel or multi-service incidents across 30+ external systems (GitHub commits, Confluent Kafka, AWS RDS, ArgoCD, PagerDuty)** | **HolmesGPT (with a frontier LLM such as Claude Sonnet / GPT-4o)** | When paired with a high-capability cloud LLM, tool-calling agents can dynamically query arbitrary runbooks and external APIs outside any fixed rule catalog. |

### Recommended Hybrid Workflow in Production
Use **KubePilot as the deterministic Tier-1 triage and grounding engine**: when an alert fires, KubePilot executes in `< 0.5 s`, eliminates known infrastructure/serving causes with mathematical certainty, computes the FinOps & DevSecOps impact, and—only if further narrative explanation or open-ended exploration is needed—passes its verified `IncidentRecord` JSON to the controlled AI explanation layer (`POST /api/v1/incidents/{id}/explain`).
