#!/usr/bin/env bash
# End-to-end check of the observability stack. Runs every check, prints
# PASS/FAIL for each, exits non-zero if any failed.
#
# Tempo and Prometheus are queried through the API server's service proxy, so
# no port-forward and no extra client image are needed.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

CHECK_NS="kp-lab-check"
RUN_ID="$(date -u +%Y%m%d%H%M%S)"
SERVICE_NAME="kp-lab-check-${RUN_ID}"
METRIC_NAME="kp_lab_check"
POLL_SECONDS=90

passed=0
failed=0
pass() { printf 'PASS  %s\n' "$*"; passed=$((passed + 1)); }
fail() { printf 'FAIL  %s\n' "$*"; failed=$((failed + 1)); }

cluster_exists || die "cluster ${CLUSTER_NAME} not found, run 'make lab-up' first"

# svc_get SERVICE:PORT PATH -> response body via the API server proxy.
svc_get() {
  kc get --raw "/api/v1/namespaces/${OBS_NAMESPACE}/services/$1/proxy$2"
}

# --- 1. Stack pods are Ready -------------------------------------------------
if out="$(kc -n "${OBS_NAMESPACE}" wait --for=condition=Ready pod --all --timeout=120s 2>&1)"; then
  pass "all pods in ${OBS_NAMESPACE} are Ready"
else
  fail "pods in ${OBS_NAMESPACE} not Ready: ${out}"
fi

# --- 2. kube-apiserver runs with an explicit --event-ttl ---------------------
ttl="$(kc -n kube-system get pod -l component=kube-apiserver \
  -o jsonpath='{.items[0].spec.containers[0].command}' | tr ',' '\n' | grep -o 'event-ttl=[^"]*' || true)"
if [[ -n "${ttl}" ]]; then
  pass "kube-apiserver flag --${ttl}"
else
  fail "kube-apiserver has no --event-ttl flag"
fi

# --- 3. Send synthetic traces and metrics through the collector --------------
kc create namespace "${CHECK_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null

# run_telemetrygen NAME ARGS... : one Job, waits for completion.
run_telemetrygen() {
  local name="$1"; shift
  local args_json
  args_json="$(printf '%s\n' "$@" | jq -R . | jq -sc .)"
  kc -n "${CHECK_NS}" delete job "${name}" --ignore-not-found >/dev/null
  kc -n "${CHECK_NS}" apply -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 600
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: telemetrygen
          image: ${TELEMETRYGEN_IMAGE}
          args: ${args_json}
EOF
  kc -n "${CHECK_NS}" wait --for=condition=Complete "job/${name}" --timeout=120s >/dev/null 2>&1
}

# Fully qualified on purpose: gRPC also looks up a TXT record
# "_grpc_config.<host>". With a short name ("...svc") that lookup leaves the
# cluster.local zone, CoreDNS forwards it upstream, and in kind the upstream
# times out (~10s), longer than telemetrygen's 10s export timeout.
endpoint="otel-collector.${OBS_NAMESPACE}.svc.cluster.local:4317"
# telemetrygen's traces mode exits 0 even when every export fails (it only
# logs "traces export: ..."), so job completion alone proves nothing: the
# logs must also be free of export errors.
if run_telemetrygen telemetrygen-traces traces \
  --otlp-endpoint "${endpoint}" --otlp-insecure \
  --service "${SERVICE_NAME}" --traces 3; then
  export_errors="$(kc -n "${CHECK_NS}" logs job/telemetrygen-traces 2>&1 | grep -E 'traces export: ' || true)"
  if [[ -z "${export_errors}" ]]; then
    pass "telemetrygen exported 3 traces without error (service.name=${SERVICE_NAME})"
  else
    fail "telemetrygen traces job completed but exports failed: $(head -1 <<<"${export_errors}")"
  fi
else
  fail "telemetrygen traces job did not complete"
  kc -n "${CHECK_NS}" logs job/telemetrygen-traces --tail=20 || true
fi
trace_pod="$(kc -n "${CHECK_NS}" get pods -l job-name=telemetrygen-traces -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"

if run_telemetrygen telemetrygen-metrics metrics \
  --otlp-endpoint "${endpoint}" --otlp-insecure \
  --service "${SERVICE_NAME}" --metrics 3 \
  --otlp-metric-name "${METRIC_NAME}" \
  --telemetry-attributes "kp.run.id=\"${RUN_ID}\""; then
  pass "telemetrygen sent metric ${METRIC_NAME} (kp.run.id=${RUN_ID})"
else
  fail "telemetrygen metrics job did not complete"
  kc -n "${CHECK_NS}" logs job/telemetrygen-metrics --tail=20 || true
fi

# --- 4. The trace is in Tempo and carries k8s.pod.name -----------------------
query="$(jq -rn --arg s "${SERVICE_NAME}" '"{ resource.service.name = \"" + $s + "\" }" | @uri')"
now="$(date +%s)"
trace_id=""
for _ in $(seq 1 $((POLL_SECONDS / 5))); do
  resp="$(svc_get tempo:3200 "/api/search?q=${query}&start=$((now - 900))&end=$((now + 300))&limit=5" 2>/dev/null || true)"
  trace_id="$(jq -r '.traces[0].traceID // empty' <<<"${resp}" 2>/dev/null || true)"
  [[ -n "${trace_id}" ]] && break
  sleep 5
done

if [[ -z "${trace_id}" ]]; then
  fail "no trace for ${SERVICE_NAME} in Tempo after ${POLL_SECONDS}s (last response: ${resp:-<empty>})"
else
  pass "Tempo returned trace ${trace_id}"
  trace="$(svc_get tempo:3200 "/api/v2/traces/${trace_id}" 2>&1 || true)"
  pod_attr="$(jq -r '
      [.. | objects | select(.key? == "k8s.pod.name") | .value.stringValue] | first // empty
    ' <<<"${trace}" 2>/dev/null || true)"
  if [[ -n "${pod_attr}" && "${pod_attr}" == "${trace_pod}" ]]; then
    pass "trace has k8s.pod.name=${pod_attr} (matches the sender pod)"
  else
    fail "trace k8s.pod.name='${pod_attr}', expected sender pod '${trace_pod}'"
  fi
fi

# --- 5. Prometheus scrapes cAdvisor and kube-state-metrics -------------------
# prom_count EXPR -> number of series returned by an instant query.
prom_count() {
  local q
  q="$(jq -rn --arg q "$1" '$q | @uri')"
  svc_get prometheus-server:80 "/api/v1/query?query=${q}" 2>/dev/null \
    | jq -r '.data.result | length' 2>/dev/null || echo 0
}

# check_series DESCRIPTION EXPR : PASS if at least one series, polling.
check_series() {
  local desc="$1" expr="$2" n=0
  for _ in $(seq 1 $((POLL_SECONDS / 5))); do
    n="$(prom_count "${expr}")"
    [[ "${n}" -gt 0 ]] && break
    sleep 5
  done
  if [[ "${n}" -gt 0 ]]; then
    pass "${desc}: ${n} series for ${expr}"
  else
    fail "${desc}: no series for ${expr} after ${POLL_SECONDS}s"
  fi
}

check_series "cAdvisor scraped on every node" 'up{job="kubernetes-nodes-cadvisor"} == 1'
check_series "cAdvisor CPU throttling series" 'container_cpu_cfs_throttled_periods_total{namespace="observability"}'
check_series "cAdvisor memory working set" 'container_memory_working_set_bytes{namespace="observability"}'
check_series "kube-state-metrics pod info" 'kube_pod_info'
check_series "kube-state-metrics readiness" 'kube_pod_status_ready{condition="true"}'
check_series "kube-state-metrics restarts" 'kube_pod_container_status_restarts_total'

# --- 6. The OTLP metric reached Prometheus with Kubernetes labels ------------
check_series "OTLP metric via collector, with k8s_pod_name" \
  "${METRIC_NAME}{kp_run_id=\"${RUN_ID}\", k8s_pod_name=~\".+\"}"

# --- Summary -----------------------------------------------------------------
echo
echo "lab-check: ${passed} passed, ${failed} failed"
[[ "${failed}" -eq 0 ]]
