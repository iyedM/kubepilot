# Independent Verification & External Evaluation Guide

This document enables **recruiters, academic jury members, and external SREs** to verify every technical claim in KubePilot independently in under 3 minutes, without trusting pre-rendered screenshots or reports.

---

## 1. Three-Command Independent Audit (No Cluster Required)

Requirements: **Go `1.26+`** and `git`. Zero external Go dependencies (`go.mod` contains only the Go standard library) and zero API keys are required.

```bash
# 1. Verify that the deterministic rule engine matches the pre-registered git tree hash
git rev-parse HEAD:internal/rules
# Expected: 201576af596e12588aa27aa2e3586dd576af5fd8
# (Matches eval/preregistration/2026-10-22-p2-v1.json)

# 2. Run the full Go unit & race-detector test suite across all packages
make test

# 3. Re-compute the held-out test split (45 runs) and dev split (19 runs) from raw telemetry archives
make eval-score SPLIT=test
make eval-score SPLIT=dev
```

### What `make eval-score SPLIT=test` Actually Executes
Unlike static benchmark tables, `cmd/eval-score` loads all **45 raw telemetry directories** under `eval/runs/test/` (each containing compressed Kubernetes Pod/Node/PVC lists `k8s-pods.json.gz`, Kubernetes Events `k8s-events.json.gz`, Prometheus range query matrices `prom-metrics.json.gz`, and Tempo OpenTelemetry trace payloads `tempo-traces.json.gz`), executes `rules.Diagnose(snap)` from scratch, and computes:
- `top1_accuracy`: **`24 / 24` (`100.0%`)** — Wilson 95% CI `[86.2%, 100.0%]`
- `top3_recall`: **`24 / 24` (`100.0%`)** — Wilson 95% CI `[86.2%, 100.0%]`
- `evidence_precision`: **`93 / 96` (`96.9%`)** — Wilson 95% CI `[91.2%, 98.9%]`
- `false_positive_rate_on_healthy`: **`0 / 21` (`0.0%`)** — Wilson 95% CI `[0.0%, 15.5%]`

---

## 2. Inspecting Raw Telemetry vs. Engine Output

To verify that a run is not hardcoded or mocked, inspect the raw Prometheus/Kubernetes/Tempo captures inside any run folder and run `why-slow` directly:

```bash
# Inspect raw Kubernetes Events and Pod statuses from a recorded run:
zcat eval/runs/test/test--cpu-throttle--limit-200m--r1/k8s-events.json.gz | head -n 30
zcat eval/runs/test/test--cpu-throttle--limit-200m--r1/prom-metrics.json.gz | head -n 30

# Run the CLI against that directory:
./bin/why-slow --run-dir eval/runs/test/test--cpu-throttle--limit-200m--r1 --format text
```

---

## 3. One-Command End-to-End Platform Showcase

To verify the HTTP API, Operational FinOps 2.0 calculations, DevSecOps PSS-Restricted posture audit (`/security`), 5-channel signal resilience matrix & controlled SRE runbook (`/runbook`), and What-If Chaos Simulator (`/simulate`):

```bash
bash scripts/demo-showcase.sh
```

---

## 4. Testing KubePilot on Your Own Kubernetes Cluster

If you have a Kubernetes cluster (`kind`, `k3s`, `EKS`, `GKE`, `AKS`) running a workload in namespace `my-genai-app`:

```bash
# 1. Build the native kubectl plugin
make build
export PATH="$PWD/bin:$PATH"

# 2. Run read-only diagnosis over the last 5 minutes (requires only get/list/watch RBAC)
kubectl why-slow \
  --namespace my-genai-app \
  --prometheus-url http://localhost:9090 \
  --tempo-url http://localhost:3200
```

Or deploy the hardened read-only Helm chart:

```bash
helm upgrade --install kubepilot ./charts/kubepilot \
  --namespace kubepilot-system --create-namespace \
  --set targetNamespace=my-genai-app
```

### Submitting External Evaluation Feedback
After testing KubePilot on your environment, please open an issue using the [**External User Diagnosis Feedback** template](../.github/ISSUE_TEMPLATE/diagnosis-feedback.yml) to record True Positives, Honest Abstentions, or edge cases.
