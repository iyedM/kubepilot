#!/usr/bin/env bash
# Phase P4 evaluation runner for Grafana Beyla / OpenTelemetry eBPF
# Instrumentation (OBI).
#
# Usage:
#   lab/ebpf/beyla.sh up     # deploy Beyla DaemonSet into observability
#   lab/ebpf/beyla.sh eval   # run Mode A (SDK + Beyla) and Mode B (Beyla-only zero-code) tests
#   lab/ebpf/beyla.sh down   # remove Beyla DaemonSet and RBAC from cluster
#
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/../faults/lib.sh"

EBPF_OUT_DIR="${ROOT_DIR}/eval/ebpf"

load_image_into_kind() {
  local pinned="$1" repo_tag="${1%%@*}"
  if ! docker image inspect "${repo_tag}" >/dev/null 2>&1; then
    log "pulling ${pinned}"
    docker pull --quiet "${pinned}" >/dev/null
    docker tag "${pinned}" "${repo_tag}"
  fi
  local node
  for node in $("${KIND}" get nodes --name "${CLUSTER_NAME}"); do
    if ! docker exec "${node}" ctr -n k8s.io images ls -q | grep -qx "${repo_tag}"; then
      log "importing ${repo_tag} into ${node}"
      docker save "${repo_tag}" | docker exec -i "${node}" ctr -n k8s.io images import - >/dev/null
    fi
  done
}

beyla_up() {
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist"
  load_image_into_kind "${BEYLA_IMAGE}"
  local beyla_tag="${BEYLA_IMAGE%%@*}"

  log "deploying Grafana Beyla / OBI DaemonSet (${beyla_tag}) in ${OBS_NAMESPACE}"
  kc apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: beyla
  namespace: ${OBS_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: beyla
rules:
  - apiGroups: [""]
    resources: ["pods", "services", "endpoints", "nodes", "namespaces"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["replicasets", "deployments", "statefulsets", "daemonsets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: beyla
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: beyla
subjects:
  - kind: ServiceAccount
    name: beyla
    namespace: ${OBS_NAMESPACE}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: beyla
  namespace: ${OBS_NAMESPACE}
  labels:
    app.kubernetes.io/name: beyla
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: beyla
  template:
    metadata:
      labels:
        app.kubernetes.io/name: beyla
    spec:
      serviceAccountName: beyla
      hostPID: true
      tolerations:
        - operator: Exists
      containers:
        - name: beyla
          image: ${beyla_tag}
          imagePullPolicy: IfNotPresent
          securityContext:
            privileged: true
          env:
            - name: BEYLA_OPEN_PORT
              value: "8000,8080"
            - name: OTEL_EBPF_OPEN_PORT
              value: "8000,8080"
            - name: OTEL_EBPF_KUBE_METADATA_ENABLE
              value: "true"
            - name: OTEL_EBPF_BPF_CONTEXT_PROPAGATION
              value: "all"
            - name: OTEL_EBPF_HTTP_ENRICHMENT_ENABLED
              value: "true"
            - name: OTEL_EBPF_HTTP_OPENAI_ENABLED
              value: "true"
            - name: OTEL_EBPF_HTTP_OPENAI_COMPATIBLE_ENABLED
              value: "true"
            - name: OTEL_EXPORTER_OTLP_ENDPOINT
              value: "http://otel-collector.${OBS_NAMESPACE}.svc.cluster.local:4317"
            - name: OTEL_EXPORTER_OTLP_PROTOCOL
              value: "grpc"
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 512Mi
EOF

  kc -n "${OBS_NAMESPACE}" rollout status daemonset/beyla --timeout=120s
  log "Grafana Beyla DaemonSet is Ready"
}

beyla_down() {
  cluster_exists || return 0
  log "removing Grafana Beyla DaemonSet and RBAC"
  kc -n "${OBS_NAMESPACE}" delete daemonset beyla --ignore-not-found >/dev/null
  kc -n "${OBS_NAMESPACE}" delete serviceaccount beyla --ignore-not-found >/dev/null
  kc delete clusterrolebinding beyla --ignore-not-found >/dev/null
  kc delete clusterrole beyla --ignore-not-found >/dev/null
  log "Grafana Beyla removed from cluster"
}

start_port_forwards() {
  kc -n "${OBS_NAMESPACE}" port-forward svc/prometheus-server 19092:80 >/dev/null 2>&1 &
  PF_PROM_PID=$!
  kc -n "${OBS_NAMESPACE}" port-forward svc/tempo 13202:3200 >/dev/null 2>&1 &
  PF_TEMPO_PID=$!
  sleep 2
}

stop_port_forwards() {
  if [[ -n "${PF_PROM_PID:-}" ]]; then
    kill "${PF_PROM_PID}" 2>/dev/null || true
  fi
  if [[ -n "${PF_TEMPO_PID:-}" ]]; then
    kill "${PF_TEMPO_PID}" 2>/dev/null || true
  fi
}

# run_window_case MODE CASE_ID FAULT VARIANT WAIT_S [SKIP_APP_REDEPLOY]
run_window_case() {
  local mode="$1" case_id="$2" fault="$3" variant="$4" wait_s="$5" skip_redeploy="${6:-0}"
  local out_file="${EBPF_OUT_DIR}/${mode}--${case_id}.json"
  if [[ "${FORCE:-0}" != "1" && -s "${out_file}" ]]; then
    log "[${mode}] skip ${case_id}: ${out_file} already exists"
    return 0
  fi
  log "[${mode}] running case ${case_id} (${fault}/${variant}, ${wait_s}s)"

  if [[ "${fault}" == "app-delay" && "${skip_redeploy}" != "1" ]]; then
    kc -n "${GENAI_NAMESPACE}" set env deployment/chat-api LAB_FAULT_HOOKS=1 >/dev/null
    kc -n "${GENAI_NAMESPACE}" rollout status deployment/chat-api --timeout=120s >/dev/null
    sleep 3
  fi

  local kind="fault"
  [[ "${fault}" == "nofault" ]] && kind="nofault"
  local tmp_dir
  tmp_dir="$(mktemp -d "/tmp/kp-p4-${case_id}.XXXXXX")"
  init_run "${tmp_dir}" "p4--${case_id}" manual "${kind}" "${fault}" "${variant}"
  mark "${tmp_dir}" inject_start

  local from_epoch from_utc to_epoch to_utc
  from_epoch="$(date -u +%s)"
  from_utc="$(now_ms)"

  "${FAULTS_DIR}/${fault}.sh" inject "${tmp_dir}" "${variant}"

  sleep "${wait_s}"

  to_epoch="$(date -u +%s)"
  to_utc="$(now_ms)"

  # Query Tempo for traces in [from_epoch, to_epoch]
  local search_json trace_summary_json why_slow_json
  search_json="$(curl -fsSL "http://127.0.0.1:13202/api/search?start=${from_epoch}&end=${to_epoch}&limit=50" 2>/dev/null || echo '{"traces":[]}')"

  trace_summary_json="$(python3 - "${search_json}" <<'PY'
import json
import sys
import urllib.request

search = json.loads(sys.argv[1])
traces = search.get("traces") or []
all_inspected = []
probe_or_scrape_count = 0
chat_trace_count = 0
gen_ai_span_traces = 0

for item in traces:
    tid = item.get("traceID")
    if not tid:
        continue
    try:
        req = urllib.request.Request(
            f"http://127.0.0.1:13202/api/traces/{tid}",
            headers={"Accept": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except Exception:
        continue

    batches = payload.get("batches") or payload.get("resourceSpans") or []
    spans_info = []
    for b in batches:
        res_attrs = {
            kv.get("key"): (kv.get("value") or {}).get("stringValue")
            for kv in (b.get("resource") or {}).get("attributes", [])
        }
        for ss in b.get("scopeSpans") or []:
            scope_name = (ss.get("scope") or {}).get("name", "")
            for sp in ss.get("spans") or []:
                sp_attrs = {}
                for kv in sp.get("attributes") or []:
                    v = kv.get("value") or {}
                    val = v.get("stringValue")
                    if val is None:
                        val = v.get("intValue")
                    if val is None:
                        val = v.get("doubleValue")
                    sp_attrs[kv.get("key")] = val
                start_ns = int(sp.get("startTimeUnixNano") or 0)
                end_ns = int(sp.get("endTimeUnixNano") or 0)
                spans_info.append({
                    "service_name": res_attrs.get("service.name"),
                    "k8s_pod_name": res_attrs.get("k8s.pod.name"),
                    "scope_name": scope_name,
                    "span_name": sp.get("name"),
                    "kind": sp.get("kind"),
                    "duration_ms": round((end_ns - start_ns) / 1e6, 2) if end_ns >= start_ns else 0.0,
                    "start_ns": start_ns,
                    "end_ns": end_ns,
                    "has_gen_ai_attrs": any(k.startswith("gen_ai.") for k in sp_attrs),
                    "attribute_keys": sorted(sp_attrs.keys()),
                })

    span_names = {s["span_name"] for s in spans_info}
    has_genai = any(s["has_gen_ai_attrs"] for s in spans_info)
    is_chat = any(
        n in ("POST /chat", "POST /v1/chat/completions") or (n or "").startswith("chat ")
        for n in span_names
    )
    if is_chat:
        chat_trace_count += 1
    else:
        probe_or_scrape_count += 1
    if has_genai:
        gen_ai_span_traces += 1

    all_inspected.append({
        "trace_id": tid,
        "root_service": item.get("rootServiceName"),
        "root_trace_name": item.get("rootTraceName"),
        "duration_ms": item.get("durationMs"),
        "is_chat_trace": is_chat,
        "has_gen_ai_span": has_genai,
        "span_count": len(spans_info),
        "scopes": sorted({s["scope_name"] for s in spans_info}),
        "spans": spans_info,
    })

print(json.dumps({
    "tempo_search_count": len(traces),
    "chat_trace_count": chat_trace_count,
    "probe_or_scrape_trace_count": probe_or_scrape_count,
    "traces_with_gen_ai_attributes": gen_ai_span_traces,
    "sample_chat_traces": [t for t in all_inspected if t["is_chat_trace"]][:4],
    "sample_probe_traces": [t for t in all_inspected if not t["is_chat_trace"]][:2],
}))
PY
)"

  PATH="${TOOLS_DIR}/bin:${PATH}" why_slow_json="$(go run "${ROOT_DIR}/cmd/why-slow" \
    --kubeconfig "${KUBECONFIG}" \
    --prometheus-url "http://127.0.0.1:19092" \
    --tempo-url "http://127.0.0.1:13202" \
    --from "${from_utc}" \
    --to "${to_utc}" \
    --format json 2>/dev/null || echo '{"status":"error"}')"

  "${FAULTS_DIR}/${fault}.sh" reset "${tmp_dir}" || true
  rm -rf "${tmp_dir}"

  python3 - "${mode}" "${case_id}" "${fault}" "${variant}" "${from_utc}" "${to_utc}" "${trace_summary_json}" "${why_slow_json}" "${out_file}" <<'PY'
import json
import sys

mode, case_id, fault, variant, from_utc, to_utc = sys.argv[1:7]
trace_summary = json.loads(sys.argv[7])
why_slow = json.loads(sys.argv[8])
out_file = sys.argv[9]

top1_cause = "none"
causes = why_slow.get("topCauses") or why_slow.get("causes") or []
if causes:
    top1_cause = causes[0].get("cause", "none")

doc = {
    "mode": mode,
    "case_id": case_id,
    "fault": fault,
    "variant": variant,
    "window": {
        "from_utc": from_utc,
        "to_utc": to_utc,
    },
    "tempo_telemetry": trace_summary,
    "kubepilot_live": {
        "status": why_slow.get("status"),
        "top1_cause": top1_cause,
        "top_causes": causes,
        "reason": why_slow.get("reason"),
    },
}
with open(out_file, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
  log "[${mode}] saved ${out_file}"
}

beyla_eval() {
  mkdir -p "${EBPF_OUT_DIR}"
  beyla_up

  trap 'stop_port_forwards' EXIT
  start_port_forwards

  # --- Mode A: OTel Python SDK active + Grafana Beyla eBPF active -------------
  log "=== Mode A: OTel Python SDK active + Grafana Beyla eBPF active ==="
  kc -n "${GENAI_NAMESPACE}" set env deployment/chat-api LAB_FAULT_HOOKS=0 >/dev/null
  reapply_declared
  # Restart Beyla pods so they rescan /proc in kind's nested PID namespace
  kc -n "${OBS_NAMESPACE}" delete pods -l app.kubernetes.io/name=beyla --grace-period=0 --force >/dev/null 2>&1 || true
  kc -n "${OBS_NAMESPACE}" rollout status daemonset/beyla --timeout=60s >/dev/null
  sleep 10

  run_window_case "mode-a-sdk-plus-beyla" "nofault--quiet" "nofault" "quiet" 35
  run_window_case "mode-a-sdk-plus-beyla" "app-delay--delay-2000ms" "app-delay" "delay-2000ms" 45
  run_window_case "mode-a-sdk-plus-beyla" "cpu-throttle--limit-300m" "cpu-throttle" "limit-300m" 45

  # --- Mode B: Zero-code Beyla-only (disable in-process SDK OTLP export) ------
  log "=== Mode B: Zero-code Beyla eBPF only (chat-api SDK export disabled) ==="
  kc -n "${GENAI_NAMESPACE}" set env deployment/chat-api LAB_FAULT_HOOKS=0 >/dev/null
  reapply_declared
  kc -n "${GENAI_NAMESPACE}" set env deployment/chat-api \
    OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:1" >/dev/null
  kc -n "${GENAI_NAMESPACE}" rollout status deployment/chat-api --timeout=120s >/dev/null
  # Restart Beyla pods so they rescan /proc in kind's nested PID namespace
  kc -n "${OBS_NAMESPACE}" delete pods -l app.kubernetes.io/name=beyla --grace-period=0 --force >/dev/null 2>&1 || true
  kc -n "${OBS_NAMESPACE}" rollout status daemonset/beyla --timeout=60s >/dev/null
  sleep 10

  run_window_case "mode-b-beyla-only" "nofault--quiet" "nofault" "quiet" 35
  run_window_case "mode-b-beyla-only" "cpu-throttle--limit-300m" "cpu-throttle" "limit-300m" 45

  # Keep OTEL_EXPORTER_OTLP_ENDPOINT disabled while enabling LAB_FAULT_HOOKS=1 for app-delay
  kc -n "${GENAI_NAMESPACE}" set env deployment/chat-api \
    OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:1" \
    LAB_FAULT_HOOKS="1" >/dev/null
  kc -n "${GENAI_NAMESPACE}" rollout status deployment/chat-api --timeout=120s >/dev/null
  kc -n "${OBS_NAMESPACE}" delete pods -l app.kubernetes.io/name=beyla --grace-period=0 --force >/dev/null 2>&1 || true
  kc -n "${OBS_NAMESPACE}" rollout status daemonset/beyla --timeout=60s >/dev/null
  sleep 10

  run_window_case "mode-b-beyla-only" "app-delay--delay-2000ms" "app-delay" "delay-2000ms" 45 "1"

  # Restore clean declared state
  kc -n "${GENAI_NAMESPACE}" set env deployment/chat-api LAB_FAULT_HOOKS=0 >/dev/null
  reapply_declared

  # Build summary.json
  python3 - "${EBPF_OUT_DIR}" "${BEYLA_IMAGE}" <<'PY'
import glob
import json
import os
import sys

out_dir = sys.argv[1]
beyla_image = sys.argv[2]

files = sorted(glob.glob(os.path.join(out_dir, "mode-*.json")))
runs = []
for p in files:
    with open(p, encoding="utf-8") as f:
        runs.append(json.load(f))

summary = {
    "tool": "Grafana Beyla / OpenTelemetry eBPF Instrumentation (OBI)",
    "image": beyla_image,
    "evaluated_at_utc": runs[-1]["window"]["to_utc"] if runs else "",
    "runs": runs,
}
with open(os.path.join(out_dir, "summary.json"), "w", encoding="utf-8") as f:
    json.dump(summary, f, indent=2)
    f.write("\n")
PY
  log "wrote ${EBPF_OUT_DIR}/summary.json"
}

cmd="${1:-}"
case "${cmd}" in
  up)
    beyla_up
    ;;
  eval)
    beyla_eval
    ;;
  down)
    beyla_down
    ;;
  *)
    die "usage: $0 {up|eval|down}"
    ;;
esac
