# KubePilot 5-Minute Reproducible Demo Guide

This guide walks through two ways to try KubePilot:
1. **Instant Offline Demo (< 60 seconds, no cluster required)**: run the `why-slow` CLI, the `kubepilot-server` Web UI, and the pre-registered benchmark scorer directly against the 64 recorded lab runs in [`eval/runs/`](../eval/runs/).
2. **Full Live Cluster Demo (`kind`)**: spin up the local `kubepilot-lab` cluster, inject real faults into `genai/model-server` and `genai/chat-api`, and diagnose them live via CLI, HTTP API, Web UI, and the `GenAIIncident` CRD.

---

## Part 1: Instant Offline Demo (No Cluster Needed)

### 1. Build the Binaries

```sh
make build
```

This compiles three self-contained Go 1.26 binaries into `./bin/`:
- `./bin/why-slow` (CLI / `kubectl` plugin)
- `./bin/kubepilot-server` (HTTP API + embedded Web UI + OpenCost estimator + CRD controller)
- `./bin/eval-score` (benchmark scorer)

### 2. Diagnose Recorded Fault Runs with `why-slow`

```sh
# 1. CFS CPU throttling (300m in-place resize on model-server)
./bin/why-slow --run-dir eval/runs/dev/dev--cpu-throttle--limit-300m--r1

# 2. Container OOMKilled (exit code 137 on chat-api at 40 MiB limit)
./bin/why-slow --run-dir eval/runs/test/test--oomkill--app-oom-40mi--r1

# 3. Unschedulable pod due to worker node taints (pod spec unchanged)
./bin/why-slow --run-dir eval/runs/test/test--unschedulable--node-taint--r1

# 4. Application pre-inference delay (2000 ms gap before GenAI client span, healthy infra)
./bin/why-slow --run-dir eval/runs/dev/dev--app-delay--delay-2000ms--r1

# 5. Healthy window with an active OOM decoy in namespace 'noise' on the same worker node
./bin/why-slow --run-dir eval/runs/test/test--nofault--decoy-same-node-oom--r1
```

Pass `--format json` to emit the structured JSON report (`status`, `window`, `topCauses`, `evidence`).

### 3. Launch the Embedded Web UI & REST API Preloaded with Recorded Incidents

```sh
./bin/kubepilot-server \
  --listen 127.0.0.1:8090 \
  --seed-runs-dir eval/runs/dev \
  --seed-limit 19
```

Open **`http://127.0.0.1:8090`** in your browser (or query the REST API):

```sh
# List diagnosed incidents filtered by cause
curl -s http://127.0.0.1:8090/api/v1/incidents?cause=cpu_throttling | jq .

# Inspect a single incident (including OpenCost resource waste estimation)
curl -s http://127.0.0.1:8090/api/v1/incidents/dev--cpu-throttle--limit-300m--r1 | jq .

# Trigger an on-demand diagnosis via POST /api/v1/diagnose
curl -s -X POST http://127.0.0.1:8090/api/v1/diagnose \
  -H 'Content-Type: application/json' \
  -d '{"runDir":"eval/runs/test/test--cold-start--model-download-hf--r1"}' | jq .
```

### 4. Reproduce the Official Benchmark Scores

```sh
# Held-out test set (45 runs: 24 fault injections + 21 no-fault windows)
make eval-score SPLIT=test

# Development set (19 runs: 13 fault injections + 6 no-fault windows)
make eval-score SPLIT=dev
```

---

## Part 2: Live Cluster Demo (`kind`)

### 1. Bring Up the Local Lab

```sh
make tools
make models
make verify-pins
make lab-up
make lab-check
```

### 2. Inject a Live Fault and Diagnose It in Real Time

```sh
# Inject CPU throttling (300m limit) on genai/model-server
make fault-cpu-throttle VARIANT=limit-300m

# Wait ~45 seconds for loadgen traffic and Prometheus scrape, then run live diagnosis:
./bin/why-slow --kubeconfig .tools/kubeconfig --namespace genai

# Reset the cluster back to healthy state
make fault-reset
```

### 3. Deploy KubePilot In-Cluster with Helm (`GenAIIncident` CRD Enabled)

```sh
helm upgrade --install kubepilot charts/kubepilot \
  --namespace observability \
  --set operator.enabled=true
```

Once installed, you can create a `GenAIIncident` custom resource to request a read-only cluster diagnosis:

```yaml
apiVersion: kubepilot.io/v1alpha1
kind: GenAIIncident
metadata:
  name: investigate-slow-chat
  namespace: genai
spec:
  targetNamespace: genai
  workload: deployment/model-server
```

And inspect the populated diagnosis status with `kubectl`:

```sh
kubectl get genaiincidents -n genai
kubectl describe genaiincident investigate-slow-chat -n genai
```
