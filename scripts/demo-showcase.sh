#!/usr/bin/env bash
# KubePilot v0.2.0 — 60-Second End-to-End Offline & API Showcase
# Demonstrates:
#   1. Deterministic CLI (why-slow) & Native kubectl Plugin (kubectl-why_slow)
#   2. Honest Abstention on Healthy + Decoy Noise Windows (0 False Positives)
#   3. Baseline vs. Incident Comparator (/compare)
#   4. Operational FinOps 2.0 (Monthly Waste Projection, Unit Economics & Patch ROI)
#   5. DevSecOps & Pod Security Standards (PSS-Restricted) Posture Audit (/security)
#   6. 5-Channel Signal Coverage Resilience & Controlled SRE Runbook (/runbook)
#   7. Interactive What-If Chaos Simulator (/simulate) & Prometheus /metrics

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

DEMO_PORT="${DEMO_PORT:-18095}"
BASE_URL="http://127.0.0.1:${DEMO_PORT}"
SERVER_PID=""

cleanup() {
  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

banner() {
  printf "\n\033[1;36m================================================================================\033[0m\n"
  printf "\033[1;36m  %s\033[0m\n" "$1"
  printf "\033[1;36m================================================================================\033[0m\n"
}

banner "1/6 · Building KubePilot Binaries (why-slow, kubectl-why_slow, kubepilot-server, eval-score)"
make build

banner "2/6 · CLI Diagnosis: CFS CPU Throttling vs. Healthy Decoy Window (0 False Positives)"
./bin/why-slow --run-dir eval/runs/dev/dev--cpu-throttle--limit-300m--r1 --format text
printf "\n--- Healthy Window with Background Noise OOM Decoy ---\n"
./bin/kubectl-why_slow --run-dir eval/runs/test/test--nofault--decoy-same-node-oom--r1 --format text

banner "3/6 · Starting Ephemeral kubepilot-server on ${BASE_URL} (Preloaded with Dev Split)"
./bin/kubepilot-server --listen "127.0.0.1:${DEMO_PORT}" --seed-runs-dir eval/runs/dev --seed-limit 19 >/tmp/kubepilot-demo-showcase.log 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 25); do
  if curl -fsS "${BASE_URL}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl -fsS "${BASE_URL}/healthz" && printf "\n"

banner "4/6 · DevSecOps & Pod Security Standards (PSS-Restricted) Audit (/security)"
curl -fsS "${BASE_URL}/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/security" | sed -n '1,48p'

banner "5/6 · 5-Channel Signal Coverage Matrix & Controlled SRE Runbook (/runbook)"
curl -fsS "${BASE_URL}/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/runbook" | sed -n '1,58p'

banner "6/6 · FinOps 2.0 Post-Mortem Excerpt & What-If Chaos Simulator (/simulate)"
curl -fsS "${BASE_URL}/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/postmortem" | sed -n '/## 3. OpenCost/,/## 5. DevSecOps/p'
printf "\n--- What-If Chaos Simulation (Synthetic Linux PSI Host CPU Contention) ---\n"
curl -fsS -X POST "${BASE_URL}/api/v1/simulate" \
  -H "Content-Type: application/json" \
  -d '{"scenarioName":"demo-psi-contention","hostCpuFullMsPerSec":260,"cfsThrottledPct":4}' | sed -n '1,28p'

printf "\n\033[1;32m[OK] KubePilot v0.2.0 showcase completed successfully.\033[0m\n"
