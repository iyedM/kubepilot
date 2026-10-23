#!/usr/bin/env bash
# Deploy, evaluate, and tear down Coroot Community Edition (P3.2 baseline).
#
#   lab/baselines/coroot.sh up     deploy Coroot CE + node-agent in observability
#   lab/baselines/coroot.sh eval   run the 12-case evaluation into eval/baselines/coroot/
#   lab/baselines/coroot.sh down   remove Coroot CE + node-agent and restore scrape interval
#
# jq programs use single quotes on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/../faults/lib.sh"

OUT_DIR="${ROOT_DIR}/eval/baselines/coroot"
COROOT_SVC="coroot:8080"

# Load a multi-arch pinned image into all kind nodes using the local amd64 image.
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

set_prom_scrape_interval() {
  local interval="$1"
  log "setting prometheus-server global.scrape_interval=${interval}"
  kc -n "${OBS_NAMESPACE}" get cm prometheus-server -o json \
    | jq --arg iv "${interval}" '.data["prometheus.yml"] |= sub("scrape_interval: (1m|15s)"; "scrape_interval: " + $iv)' \
    | kc -n "${OBS_NAMESPACE}" apply -f - >/dev/null
  # Wait briefly for kubelet configmap projection, then reload Prometheus.
  sleep 2
  kc -n "${OBS_NAMESPACE}" exec deploy/prometheus-server -c prometheus-server -- \
    wget -qO- --post-data='' http://127.0.0.1:9090/-/reload >/dev/null 2>&1 || true
}

coroot_project_id() {
  svc_get "${OBS_NAMESPACE}" "${COROOT_SVC}" "/api/user" 2>/dev/null \
    | jq -r '.projects[0].id // empty'
}

# In kind (nested PID namespaces), coroot-node-agent's sched_process_exec
# tracepoint receives host root PIDs rather than kind node PIDs, so containers
# started after node-agent startup are only discovered when node-agent rescans
# /proc on startup. Force-recreating the DaemonSet pods takes <1s.
rescan_node_agent() {
  kc -n "${OBS_NAMESPACE}" delete pods -l app.kubernetes.io/name=coroot-node-agent --grace-period=0 --force >/dev/null 2>&1 || true
  kc -n "${OBS_NAMESPACE}" rollout status ds/coroot-node-agent --timeout=60s >/dev/null 2>&1 || true
}

wait_coroot_cache_update() {
  local before now deadline=$(( $(date +%s) + 75 ))
  before="$(kc -n "${OBS_NAMESPACE}" logs deploy/coroot 2>/dev/null | grep -c 'updater.go:208' || true)"
  while (( $(date +%s) < deadline )); do
    now="$(kc -n "${OBS_NAMESPACE}" logs deploy/coroot 2>/dev/null | grep -c 'updater.go:208' || true)"
    if (( now > before )); then
      sleep 1
      return 0
    fi
    sleep 2
  done
  log "WARNING: timed out waiting for Coroot updater.go cycle"
}

up() {
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist"
  load_image_into_kind "${COROOT_IMAGE}"
  load_image_into_kind "${COROOT_NODE_AGENT_IMAGE}"

  set_prom_scrape_interval "15s"

  local coroot_tag="${COROOT_IMAGE%%@*}"
  local agent_tag="${COROOT_NODE_AGENT_IMAGE%%@*}"

  log "deploying coroot-node-agent (${agent_tag}) and coroot (${coroot_tag})"
  kc -n "${OBS_NAMESPACE}" apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: coroot-node-agent
  namespace: ${OBS_NAMESPACE}
  labels:
    app.kubernetes.io/name: coroot-node-agent
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: coroot-node-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: coroot-node-agent
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "80"
    spec:
      hostPID: true
      tolerations:
        - operator: Exists
      containers:
        - name: node-agent
          image: ${agent_tag}
          imagePullPolicy: IfNotPresent
          args:
            - "--cgroupfs-root=/host/sys/fs/cgroup"
            - "--min-container-age=0"
            - "--scrape-interval=15s"
          securityContext:
            privileged: true
          ports:
            - containerPort: 80
              name: http
          volumeMounts:
            - mountPath: /host/sys/fs/cgroup
              name: cgroupfs
              readOnly: true
            - mountPath: /sys/kernel/tracing
              name: tracing
            - mountPath: /sys/kernel/debug
              name: debugfs
      volumes:
        - name: cgroupfs
          hostPath:
            path: /sys/fs/cgroup
        - name: tracing
          hostPath:
            path: /sys/kernel/tracing
        - name: debugfs
          hostPath:
            path: /sys/kernel/debug
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: coroot
  namespace: ${OBS_NAMESPACE}
  labels:
    app.kubernetes.io/name: coroot
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: coroot
  template:
    metadata:
      labels:
        app.kubernetes.io/name: coroot
    spec:
      containers:
        - name: coroot
          image: ${coroot_tag}
          imagePullPolicy: IfNotPresent
          args:
            - "--data-dir=/data"
            - "--bootstrap-prometheus-url=http://prometheus-server.${OBS_NAMESPACE}:80"
            - "--bootstrap-refresh-interval=15s"
            - "--auth-anonymous-role=Admin"
            - "--do-not-check-for-updates"
            - "--disable-usage-statistics"
          ports:
            - containerPort: 8080
              name: http
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: coroot
  namespace: ${OBS_NAMESPACE}
spec:
  selector:
    app.kubernetes.io/name: coroot
  ports:
    - name: http
      port: 8080
      targetPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: kube-prometheus-stack-prometheus
  namespace: ${OBS_NAMESPACE}
spec:
  selector:
    app.kubernetes.io/component: server
    app.kubernetes.io/instance: prometheus
    app.kubernetes.io/name: prometheus
  ports:
    - name: http
      port: 9090
      targetPort: 9090
EOF

  kc -n "${OBS_NAMESPACE}" rollout status ds/coroot-node-agent --timeout=90s
  kc -n "${OBS_NAMESPACE}" rollout status deploy/coroot --timeout=90s

  local deadline=$(( $(date +%s) + 120 )) pid nodes
  while (( $(date +%s) < deadline )); do
    pid="$(coroot_project_id)"
    if [[ -n "${pid}" ]]; then
      nodes="$(svc_get "${OBS_NAMESPACE}" "${COROOT_SVC}" "/api/project/${pid}/status" 2>/dev/null \
        | jq -r '.data.node_agent.nodes // 0')"
      if (( nodes >= 3 )); then
        log "Coroot CE ready (project=${pid}, node_agent.nodes=${nodes})"
        return 0
      fi
    fi
    sleep 5
  done
  die "Coroot CE did not see 3 node-agents within 120s"
}

down() {
  cluster_exists || return 0
  log "removing Coroot CE and coroot-node-agent from ${OBS_NAMESPACE}"
  kc -n "${OBS_NAMESPACE}" delete deploy/coroot svc/coroot ds/coroot-node-agent svc/kube-prometheus-stack-prometheus --ignore-not-found >/dev/null
  set_prom_scrape_interval "1m"
  log "Coroot CE removed and prometheus-server scrape_interval restored to 1m"
}

# query_coroot_window CASE_ID FAULT VARIANT EXPECTED TARGET FROM_UTC TO_UTC FROM_MS TO_MS KP_JSON
query_coroot_window() {
  local case_id="$1" fault="$2" variant="$3" expected="$4" target="$5"
  local from_utc="$6" to_utc="$7" from_ms="$8" to_ms="$9" kp_json="${10}"
  local pid t0 t1 query_ms ov_apps ov_deps ms_app ca_app
  pid="$(coroot_project_id)"
  [[ -n "${pid}" ]] || die "cannot resolve Coroot project ID"

  t0="$(date +%s%3N)"
  ov_apps="$({ svc_get "${OBS_NAMESPACE}" "${COROOT_SVC}" "/api/project/${pid}/overview/applications?from=${from_ms}&to=${to_ms}" 2>/dev/null || echo '{"data":{}}'; } \
    | jq -c '[.data.applications[]? | select(.id | test(":(genai|noise):")) | {
        id, category, status,
        instances: {status: .instances.status, value: .instances.value},
        restarts: {status: .restarts.status, value: .restarts.value},
        cpu: {status: .cpu.status, value: .cpu.value},
        memory: {status: .memory.status, value: .memory.value},
        latency: {status: .latency.status, value: .latency.value},
        errors: {status: .errors.status, value: .errors.value}
      }]')"
  ov_deps="$({ svc_get "${OBS_NAMESPACE}" "${COROOT_SVC}" "/api/project/${pid}/overview/deployments?from=${from_ms}&to=${to_ms}" 2>/dev/null || echo '{"data":{}}'; } \
    | jq -c '[.data.deployments[]? | select(.application.id | contains(":genai:")) | {
        app_id: .application.id, version, deployed, age, status, summary
      }]')"
  ms_app="$({ svc_get "${OBS_NAMESPACE}" "${COROOT_SVC}" "/api/project/${pid}/app/${pid}:genai:Deployment:model-server?from=${from_ms}&to=${to_ms}" 2>/dev/null || echo '{"data":{}}'; } \
    | jq -c '.data | {
        app_map: {id: .app_map.application.id, status: .app_map.application.status, instances: [.app_map.instances[]? | {id, labels}]},
        reports: [.reports[]? | {
          name, status,
          checks: [.checks[]? | {id, title, status, message, threshold, unit}],
          tables: [.widgets[]? | select(.table != null) | {
            header: .table.header,
            rows: [.table.rows[]? | [.cells[]? | {value, status}]]
          }],
          charts: [.widgets[]? | ((.chart_group.charts // (if .chart then [.chart] else [] end))[]) | {
            title,
            series: [.series[]? | {name, data: .data[-6:]}],
            threshold: (if .threshold then {name: .threshold.name, data: .threshold.data[-6:]} else null end)
          }]
        }]
      }')"
  ca_app="$({ svc_get "${OBS_NAMESPACE}" "${COROOT_SVC}" "/api/project/${pid}/app/${pid}:genai:Deployment:chat-api?from=${from_ms}&to=${to_ms}" 2>/dev/null || echo '{"data":{}}'; } \
    | jq -c '.data | {
        app_map: {id: .app_map.application.id, status: .app_map.application.status, instances: [.app_map.instances[]? | {id, labels}]},
        reports: [.reports[]? | {
          name, status,
          checks: [.checks[]? | {id, title, status, message, threshold, unit}],
          tables: [.widgets[]? | select(.table != null) | {
            header: .table.header,
            rows: [.table.rows[]? | [.cells[]? | {value, status}]]
          }],
          charts: [.widgets[]? | ((.chart_group.charts // (if .chart then [.chart] else [] end))[]) | {
            title,
            series: [.series[]? | {name, data: .data[-6:]}],
            threshold: (if .threshold then {name: .threshold.name, data: .threshold.data[-6:]} else null end)
          }]
        }]
      }')"
  t1="$(date +%s%3N)"
  query_ms=$(( t1 - t0 ))

  jq -n \
    --arg cid "${case_id}" \
    --arg fault "${fault}" \
    --arg variant "${variant}" \
    --arg expected "${expected}" \
    --arg target "${target}" \
    --arg from_utc "${from_utc}" \
    --arg to_utc "${to_utc}" \
    --argjson from_ms "${from_ms}" \
    --argjson to_ms "${to_ms}" \
    --argjson query_ms "${query_ms}" \
    --arg coroot_image "${COROOT_IMAGE}" \
    --arg agent_image "${COROOT_NODE_AGENT_IMAGE}" \
    --arg pid "${pid}" \
    --argjson ov_apps "${ov_apps}" \
    --argjson ov_deps "${ov_deps}" \
    --argjson ms_app "${ms_app}" \
    --argjson ca_app "${ca_app}" \
    --argjson kp "${kp_json}" \
    '{
      case_id: $cid,
      fault: $fault,
      variant: $variant,
      expected_cause: $expected,
      target_workload: $target,
      window: {
        from_utc: $from_utc,
        to_utc: $to_utc,
        from_ms: $from_ms,
        to_ms: $to_ms
      },
      coroot: {
        image: $coroot_image,
        node_agent_image: $agent_image,
        project_id: $pid,
        query_ms: $query_ms,
        overview_applications: $ov_apps,
        overview_deployments: $ov_deps,
        model_server: $ms_app,
        chat_api: $ca_app
      },
      kubepilot: $kp
    }' > "${OUT_DIR}/${case_id}.json"
  log "recorded ${OUT_DIR#"${ROOT_DIR}/"}/${case_id}.json (Coroot query ${query_ms}ms)"
}

run_case() {
  local case_id="$1" fault="$2" variant="$3" expected="$4" target="$5" wait_s="$6"
  if [[ "${FORCE:-0}" != 1 && -s "${OUT_DIR}/${case_id}.json" ]]; then
    log "skip ${case_id}: ${OUT_DIR#"${ROOT_DIR}/"}/${case_id}.json already exists"
    return 0
  fi

  local kind="fault"
  [[ "${fault}" == "nofault" ]] && kind="nofault"

  # Enable lab fault hooks only for app-delay.
  local hooks_enabled=0
  if [[ "${fault}" == "app-delay" && "$(chat_api_hooks)" == 0 ]]; then
    log "enabling chat-api lab fault hooks for ${case_id}"
    "${LAB_DIR}/scripts/lab-hooks.sh" on
    hooks_enabled=1
    sleep 10
    rescan_node_agent
    sleep 15
  fi

  local tmp_dir
  tmp_dir="$(mktemp -d "/tmp/kp-p3-${case_id}.XXXXXX")"
  init_run "${tmp_dir}" "p3--${case_id}" manual "${kind}" "${fault}" "${variant}"

  local from_ms inject_start reset_start to_ms kp_json
  from_ms=$(( $(date +%s%3N) - 30000 ))
  run_set "${tmp_dir}" '.timeline.baseline_start = $t' --arg t "$(date -u -d '-120 seconds' +%Y-%m-%dT%H:%M:%S.000Z)"
  mark "${tmp_dir}" inject_start
  inject_start="$(run_get "${tmp_dir}" '.timeline.inject_start')"

  log "injecting ${case_id} (${fault}/${variant})"
  if ! "${FAULTS_DIR}/${fault}.sh" inject "${tmp_dir}" "${variant}"; then
    reapply_declared || true
    "${FAULTS_DIR}/${fault}.sh" reset "${tmp_dir}" || true
    [[ "${hooks_enabled}" == 1 ]] && "${LAB_DIR}/scripts/lab-hooks.sh" off
    rm -rf "${tmp_dir}"
    die "failed to inject ${case_id}"
  fi
  mark "${tmp_dir}" inject_done

  # Wait briefly for any newly created container to start, then rescan /proc in node-agent.
  sleep 4
  rescan_node_agent

  log "waiting ${wait_s}s for active telemetry on ${case_id}"
  sleep "${wait_s}"

  mark "${tmp_dir}" reset_start
  reset_start="$(run_get "${tmp_dir}" '.timeline.reset_start')"
  to_ms="$(date -u -d "${reset_start}" +%s%3N)"

  kp_json="$(kubepilot_for_case "${case_id}")"

  query_coroot_window "${case_id}" "${fault}" "${variant}" "${expected}" "${target}" \
    "${inject_start}" "${reset_start}" "${from_ms}" "${to_ms}" "${kp_json}"

  log "resetting ${case_id}"
  "${FAULTS_DIR}/${fault}.sh" reset "${tmp_dir}" || true
  reapply_declared
  kc -n "${GENAI_NAMESPACE}" delete pvc model-scratch --ignore-not-found --wait=true >/dev/null 2>&1 || true
  if [[ "${hooks_enabled}" == 1 ]]; then
    "${LAB_DIR}/scripts/lab-hooks.sh" off
  fi
  kc -n "${GENAI_NAMESPACE}" rollout status deploy/model-server deploy/chat-api --timeout=90s >/dev/null 2>&1 || true
  wait_chat_ok 180 || log "WARNING: chat-api did not return 200 within 180s after ${case_id}"
  rescan_node_agent
  sleep 15
  rm -rf "${tmp_dir}"
}

matching_run_dir() {
  local cid="$1"
  if [[ -d "${ROOT_DIR}/eval/runs/dev/dev--${cid}--r1" ]]; then
    echo "${ROOT_DIR}/eval/runs/dev/dev--${cid}--r1"
  elif [[ -d "${ROOT_DIR}/eval/runs/test/test--${cid}--r1" ]]; then
    echo "${ROOT_DIR}/eval/runs/test/test--${cid}--r1"
  else
    die "no matching dev/test run directory for ${cid}"
  fi
}

kubepilot_for_case() {
  local rdir
  rdir="$(matching_run_dir "$1")"
  go run "${ROOT_DIR}/cmd/why-slow" --run-dir "${rdir}" --format json 2>/dev/null || echo 'null'
}

refresh_kubepilot() {
  local f cid kp_json
  for f in "${OUT_DIR}"/*.json; do
    [[ -e "${f}" ]] || continue
    [[ "$(basename "${f}")" == "summary.json" ]] && continue
    cid="$(basename "${f}" .json)"
    kp_json="$(kubepilot_for_case "${cid}")"
    jq --argjson kp "${kp_json}" '.kubepilot = $kp' "${f}" > "${f}.tmp" && mv "${f}.tmp" "${f}"
  done
}

requery_case() {
  local f="${OUT_DIR}/$1.json"
  [[ -s "${f}" ]] || die "missing ${f}"
  local cid fault variant expected target from_utc to_utc from_ms to_ms kp_json
  cid="$(jq -r '.case_id' "${f}")"
  fault="$(jq -r '.fault' "${f}")"
  variant="$(jq -r '.variant' "${f}")"
  expected="$(jq -r '.expected_cause' "${f}")"
  target="$(jq -r '.target_workload' "${f}")"
  from_utc="$(jq -r '.window.from_utc' "${f}")"
  to_utc="$(jq -r '.window.to_utc' "${f}")"
  from_ms="$(jq -r '.window.from_ms' "${f}")"
  to_ms="$(jq -r '.window.to_ms' "${f}")"
  kp_json="$(jq -c '.kubepilot' "${f}")"
  query_coroot_window "${cid}" "${fault}" "${variant}" "${expected}" "${target}" \
    "${from_utc}" "${to_utc}" "${from_ms}" "${to_ms}" "${kp_json}"
}

requery_all() {
  local f cid
  for f in "${OUT_DIR}"/*.json; do
    [[ -e "${f}" ]] || continue
    [[ "$(basename "${f}")" == "summary.json" ]] && continue
    cid="$(basename "${f}" .json)"
    requery_case "${cid}"
  done
}

build_summary() {
  python3 - "${OUT_DIR}" <<'PY'
import glob, json, os, sys

out_dir = sys.argv[1]
files = sorted(glob.glob(os.path.join(out_dir, "*.json")))
files = [f for f in files if os.path.basename(f) != "summary.json"]

cases = []
counts = {"pass": 0, "partial": 0, "miss": 0, "false_positive": 0, "healthy_ok": 0}

for path in files:
    with open(path) as f:
        rec = json.load(f)
    cid = rec["case_id"]
    expected = rec["expected_cause"]
    coroot = rec["coroot"]
    ms = coroot["model_server"]
    ca = coroot["chat_api"]

    def non_ok_checks(app):
        res = []
        for r in app.get("reports") or []:
            for c in r.get("checks") or []:
                if c.get("status") not in ("ok", "unknown"):
                    res.append({
                        "report": r["name"],
                        "id": c["id"],
                        "title": c["title"],
                        "status": c["status"],
                        "message": c.get("message", "")
                    })
        return res

    def instance_rows(app):
        rows = []
        for r in app.get("reports") or []:
            if r.get("name") == "Instances":
                for t in r.get("tables") or []:
                    for row in t.get("rows") or []:
                        if len(row) >= 5:
                            rows.append({
                                "instance": row[0].get("value", ""),
                                "status": row[1].get("value", ""),
                                "restarts": row[2].get("value", ""),
                                "ip": row[3].get("value", ""),
                                "node": row[4].get("value", "")
                            })
        return rows

    ms_checks = non_ok_checks(ms)
    ca_checks = non_ok_checks(ca)
    ms_inst = instance_rows(ms)
    ca_inst = instance_rows(ca)

    # Determine Coroot grade according to eval/baselines/README.md rubric
    grade = "miss"
    rationale = ""
    all_checks = [("model-server", c) for c in ms_checks] + [("chat-api", c) for c in ca_checks]
    check_ids = {c["id"] for _, c in all_checks}

    if expected == "none":
        if not all_checks:
            grade = "healthy_ok"
            rationale = "All checks on genai/model-server and genai/chat-api remained ok while noise/decoy-oom failed."
            counts["healthy_ok"] += 1
        else:
            grade = "false_positive"
            rationale = f"Non-ok checks fired on genai during nofault window: {all_checks}"
            counts["false_positive"] += 1
    elif expected == "cpu_throttling":
        if "CPUContainer" in check_ids:
            grade = "pass"
            rationale = "CPUContainer check fired ('high CPU utilization') and CPU report recorded limit=0.3 cores and elevated Throttled time."
            counts["pass"] += 1
        elif all_checks:
            grade = "partial"
            rationale = f"Flagged model-server ({[c['id'] for _, c in all_checks]}) without CPUContainer check."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = "No check fired on model-server."
            counts["miss"] += 1
    elif expected == "oom_killed":
        target_checks = ca_checks if "chat-api" in rec["target_workload"] else ms_checks
        target_inst = ca_inst if "chat-api" in rec["target_workload"] else ms_inst
        target_ids = {c["id"] for c in target_checks}
        inst_statuses = " ".join(i.get("status", "") for i in target_inst)
        if "MemoryOOM" in target_ids or "OOMKilled" in inst_statuses:
            grade = "pass"
            rationale = f"Identified OOM / container termination on {rec['target_workload']} (checks={[c['id'] for c in target_checks]}, instances={inst_statuses})."
            counts["pass"] += 1
        elif target_checks:
            grade = "partial"
            rationale = f"Flagged {rec['target_workload']} checks {[ (c['id'], c['message']) for c in target_checks ]} without naming OOMKilled in check message."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = f"No check fired on {rec['target_workload']}."
            counts["miss"] += 1
    elif expected == "unschedulable":
        inst_statuses = " ".join(i.get("status", "") for i in ms_inst)
        if "InstanceAvailability" in check_ids and ("unschedulable" in inst_statuses.lower() or "pending" in inst_statuses.lower()):
            grade = "pass"
            rationale = f"InstanceAvailability fired and Instances table reported '{inst_statuses}'."
            counts["pass"] += 1
        elif "InstanceAvailability" in check_ids:
            grade = "partial"
            rationale = f"InstanceAvailability fired ('no instances available') on model-server; Instances table status='{inst_statuses}' (does not distinguish taint vs unbound PVC)."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = "No check fired on model-server."
            counts["miss"] += 1
    elif expected == "readiness_failing":
        inst_statuses = " ".join(i.get("status", "") for i in ms_inst)
        if "InstanceAvailability" in check_ids and "not ready" in inst_statuses.lower():
            grade = "partial"
            rationale = f"InstanceAvailability fired and Instances table showed '{inst_statuses}', but does not distinguish failing HTTP probe vs initialDelaySeconds=600."
            counts["partial"] += 1
        elif "InstanceAvailability" in check_ids:
            grade = "partial"
            rationale = f"InstanceAvailability fired ('no instances available'); Instances table status='{inst_statuses}'."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = "No check fired on model-server."
            counts["miss"] += 1
    elif expected == "cold_start":
        inst_statuses = " ".join(i.get("status", "") for i in ms_inst)
        if "InstanceAvailability" in check_ids:
            grade = "partial"
            rationale = f"InstanceAvailability fired during startup outage (status='{inst_statuses}'), without attributing unavailability to initContainer model download."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = "All checks ok once pod became Ready (or startup window did not attribute cause to initContainer cold start)."
            counts["miss"] += 1
    elif expected == "config_change":
        if all_checks:
            grade = "partial"
            rationale = f"Checks fired ({[c['id'] for _, c in all_checks]}), but Deployments check requires >= 30m lifetime to compare rollout performance."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = "All checks ok; Coroot Deployments inspection requires >= 30m deployment lifetime ('Not enough data due to the lifetime < 30 minutes') and does not inspect llama.cpp token/s or --threads args."
            counts["miss"] += 1
    elif expected == "host_cpu_contention":
        if "CPUNode" in check_ids:
            grade = "pass"
            rationale = "CPUNode check fired ('high CPU utilization of node') alongside CPU delay and CPU consumers chart showing cpu-neighbor."
            counts["pass"] += 1
        elif all_checks:
            grade = "partial"
            rationale = f"Checks fired ({[c['id'] for _, c in all_checks]}) without CPUNode."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = "No check fired on model-server (CPU delay chart recorded stall time, but CPUNode threshold >80% did not trip or no check alerted)."
            counts["miss"] += 1
    elif expected == "unknown_no_infrastructure_cause":
        if not all_checks:
            grade = "miss"
            rationale = "All infrastructure checks (CPU, Memory, Instances, Net, Logs) reported ok, but Coroot did not flag the 2000ms application-internal delay between the HTTP server span and GenAI client span."
            counts["miss"] += 1
        else:
            grade = "partial"
            rationale = f"Checks fired ({[c['id'] for _, c in all_checks]}) without identifying the pre-inference span gap."
            counts["partial"] += 1

    kp = rec.get("kubepilot") or {}
    kp_top1 = (kp.get("topCauses") or [{}])[0] if isinstance(kp, dict) else {}

    cases.append({
        "case_id": cid,
        "fault": rec["fault"],
        "variant": rec["variant"],
        "expected_cause": expected,
        "target_workload": rec["target_workload"],
        "window": rec["window"],
        "coroot": {
            "query_ms": coroot["query_ms"],
            "model_server_status": ms.get("app_map", {}).get("status", "unknown"),
            "chat_api_status": ca.get("app_map", {}).get("status", "unknown"),
            "model_server_non_ok_checks": ms_checks,
            "chat_api_non_ok_checks": ca_checks,
            "model_server_instances": ms_inst,
            "chat_api_instances": ca_inst,
            "deployments": coroot.get("overview_deployments") or [],
            "grade": grade,
            "rationale": rationale
        },
        "kubepilot": {
            "status": kp.get("status") if isinstance(kp, dict) else None,
            "top1_cause": kp_top1.get("cause"),
            "top1_confidence": kp_top1.get("confidence"),
            "top1_component": kp_top1.get("component"),
            "top1_summary": kp_top1.get("summary")
        }
    })

summary = {
    "tool": "Coroot Community Edition",
    "coroot_image": cases[0]["coroot"] if not cases else files and json.load(open(files[0]))["coroot"]["image"],
    "node_agent_image": files and json.load(open(files[0]))["coroot"]["node_agent_image"],
    "total_cases": len(cases),
    "fault_cases": len([c for c in cases if c["expected_cause"] != "none"]),
    "nofault_cases": len([c for c in cases if c["expected_cause"] == "none"]),
    "counts": counts,
    "cases": cases
}

with open(os.path.join(out_dir, "summary.json"), "w") as f:
    json.dump(summary, f, indent=2)
    f.write("\n")
print(f"Wrote {os.path.join(out_dir, 'summary.json')} ({len(cases)} cases): {counts}")
PY
}

eval_all() {
  mkdir -p "${OUT_DIR}"
  up
  rescan_node_agent

  # Case 1: cpu-throttle / limit-300m
  run_case "cpu-throttle--limit-300m" "cpu-throttle" "limit-300m" "cpu_throttling" "deployment/model-server" 45
  # Case 2: oomkill / noload-400mi
  run_case "oomkill--noload-400mi" "oomkill" "noload-400mi" "oom_killed" "deployment/model-server" 35
  # Case 3: config-regression / threads-1 (runs with clean pre-injection baseline)
  run_case "config-regression--threads-1" "config-regression" "threads-1" "config_change" "deployment/model-server" 60
  # Case 4: nofault / decoy-same-node-oom
  run_case "nofault--decoy-same-node-oom" "nofault" "decoy-same-node-oom" "none" "none" 35
  # Case 5: unschedulable / node-taint
  run_case "unschedulable--node-taint" "unschedulable" "node-taint" "unschedulable" "deployment/model-server" 35
  # Case 6: unschedulable / pvc-unbound
  run_case "unschedulable--pvc-unbound" "unschedulable" "pvc-unbound" "unschedulable" "deployment/model-server" 35
  # Case 7: readiness-fail / probe-path-404
  run_case "readiness-fail--probe-path-404" "readiness-fail" "probe-path-404" "readiness_failing" "deployment/model-server" 35
  # Case 8: readiness-fail / initial-delay-600
  run_case "readiness-fail--initial-delay-600" "readiness-fail" "initial-delay-600" "readiness_failing" "deployment/model-server" 35
  # Case 9: host-contention / neighbor-pod-same-node
  run_case "host-contention--neighbor-pod-same-node" "host-contention" "neighbor-pod-same-node" "host_cpu_contention" "deployment/model-server" 75
  # Case 10: app-delay / delay-2000ms
  run_case "app-delay--delay-2000ms" "app-delay" "delay-2000ms" "unknown_no_infrastructure_cause" "deployment/chat-api" 40
  # Case 11: oomkill / app-oom-40mi
  run_case "oomkill--app-oom-40mi" "oomkill" "app-oom-40mi" "oom_killed" "deployment/chat-api" 35
  # Case 12: cold-start / model-download-10mbs
  run_case "cold-start--model-download-10mbs" "cold-start" "model-download-10mbs" "cold_start" "deployment/model-server" 15

  log "waiting 16s + Coroot cache update before final window queries"
  sleep 16
  wait_coroot_cache_update
  requery_all
  build_summary
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  eval) eval_all ;;
  requery) requery_all ;;
  refresh-kp) refresh_kubepilot && build_summary ;;
  summary) build_summary ;;
  *) die "usage: $0 up | eval | down | requery | refresh-kp | summary" ;;
esac
