#!/usr/bin/env bash
# Shared helpers for fault injection: ground truth (run.json) and evidence
# export. Source after lab/scripts/lib.sh and lab/scripts/checklib.sh.
#
# run.json is updated with jq through a temporary file and an atomic mv, so
# an interrupted script never leaves a half-written record.

# Variables defined here are used by the scripts that source this file.
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2034,SC2016

RUNS_DIR="${ROOT_DIR}/eval/runs"
CATALOG="${ROOT_DIR}/eval/faults/catalog.v1.json"
ACTIVE_RUN="${TOOLS_DIR}/active-run"
FAULTS_DIR="${LAB_DIR}/faults"
PROM_SVC="prometheus-server:80"

now_ms() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
to_epoch() { date -u -d "$1" +%s; }

# run_set RUN_DIR JQ_FILTER [jq args...]: update run.json atomically.
run_set() {
  local dir="$1" filter="$2"
  shift 2
  local tmp
  tmp="$(mktemp "${dir}/.run.json.XXXXXX")"
  jq "$@" "${filter}" "${dir}/run.json" > "${tmp}" && mv "${tmp}" "${dir}/run.json"
}
run_get() { jq -r "$2" "$1/run.json"; }

# --- host load ---------------------------------------------------------------
cpu_jiffies() { awk '/^cpu / {busy=$2+$3+$4+$7+$8+$9; total=busy+$5+$6; print busy, total}' /proc/stat; }
phase_begin() { cpu_jiffies > "$1/.cpu-$2"; }
# phase_end RUN_DIR PHASE: store host CPU busy % of the phase in run.json.
phase_end() {
  local b0 t0 b1 t1 pct
  read -r b0 t0 < "$1/.cpu-$2"
  read -r b1 t1 < <(cpu_jiffies)
  pct="$(awk -v a="${b0}" -v b="${t0}" -v c="${b1}" -v d="${t1}" 'BEGIN {printf "%.1f", 100*(c-a)/(d-b)}')"
  run_set "$1" '.host.cpu_busy_pct[$p] = ($v | tonumber)' --arg p "$2" --arg v "${pct}"
}
loadavg_json() { awk '{printf "[%s,%s,%s]", $1, $2, $3}' /proc/loadavg; }
mem_available_mib() { awk '/MemAvailable/ {printf "%d", $2/1024}' /proc/meminfo; }

# --- cluster state -----------------------------------------------------------
# pods_json APP: pods of an app label as [{name, uid, node, phase,
# container_id, restarts, created}] (all phases, newest first).
pods_json() {
  kc -n "${GENAI_NAMESPACE}" get pods -l "app.kubernetes.io/name=$1" -o json \
    | jq -c '[.items | sort_by(.metadata.creationTimestamp) | reverse | .[]
              | {name: .metadata.name, uid: .metadata.uid, node: .spec.nodeName, phase: .status.phase,
                 container_id: (.status.containerStatuses[0].containerID // null),
                 restarts: (.status.containerStatuses[0].restartCount // 0),
                 created: .metadata.creationTimestamp}]'
}
running_pod() {
  kc -n "${GENAI_NAMESPACE}" get pods -l "app.kubernetes.io/name=$1" \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}
# image_json APP -> {image, image_id}: the reference declared in the pod spec
# and the ID reported by the runtime (for images loaded with `kind load`,
# containerd reports an "import-<date>" name, so the spec reference matters).
image_json() {
  kc -n "${GENAI_NAMESPACE}" get pods -l "app.kubernetes.io/name=$1" -o json 2>/dev/null \
    | jq -c '.items[0] // {} | {image: (.spec.containers[0].image // null),
                               image_id: (.status.containerStatuses[0].imageID // null)}'
}

versions_json() {
  local dirty=false llama=""
  [[ -n "$(git -C "${ROOT_DIR}" status --porcelain --untracked-files=no)" ]] && dirty=true
  local pod
  pod="$(running_pod model-server)"
  [[ -n "${pod}" ]] && llama="$(kc -n "${GENAI_NAMESPACE}" exec "${pod}" -- /app/llama-server --version 2>&1 | head -1)"
  jq -n --arg commit "$(git -C "${ROOT_DIR}" rev-parse HEAD)" --argjson dirty "${dirty}" \
    --arg venv "$(sha256sum "${LAB_DIR}/versions.env" | cut -d' ' -f1)" \
    --arg k8s "$(kc version -o json 2>/dev/null | jq -r .serverVersion.gitVersion)" \
    --argjson ms "$(image_json model-server)" --argjson ca "$(image_json chat-api)" --argjson lg "$(image_json loadgen)" \
    --arg llama "${llama}" \
    '{git_commit: $commit, git_dirty: $dirty, versions_env_sha256: $venv, kubernetes: $k8s,
      images: {"model-server": $ms, "chat-api": $ca, "loadgen": $lg}, llama_server_version: $llama}'
}

# --- catalog -----------------------------------------------------------------
# catalog_variant FAULT VARIANT -> {"fault": {...}, "variant": {...}} or fails.
catalog_variant() {
  jq -ce --arg f "$1" --arg v "$2" '
    (.faults[] | select(.name == $f)) as $fault
    | ($fault.variants[] | select(.id == $v)) as $variant
    | {fault: ($fault | del(.variants)), variant: $variant}' "${CATALOG}"
}
catalog_nofault() {
  jq -ce --arg v "$1" --arg s "$2" '.nofault.variants[] | select(.id == $v and .split == $s)' "${CATALOG}"
}

# --- run record --------------------------------------------------------------
# init_run DIR RUN_ID SPLIT KIND FAULT VARIANT: write the initial run.json.
init_run() {
  local dir="$1" run_id="$2" split="$3" kind="$4" fault="$5" variant="$6" entry="null" fault_json="null"
  mkdir -p "${dir}"
  if [[ "${kind}" == fault ]]; then
    entry="$(catalog_variant "${fault}" "${variant}")" || die "unknown catalog entry ${fault}/${variant}"
    fault_json="$(jq -c '{name: .fault.name, variant: .variant.id, cause: .fault.cause,
                          mechanism: .variant.mechanism, params: (.variant.params // {})}' <<<"${entry}")"
  else
    catalog_nofault "${variant}" "${split}" >/dev/null || [[ "${split}" == manual || "${split}" == dryrun ]] \
      || die "unknown no-fault variant ${variant} for split ${split}"
  fi
  jq -n --arg id "${run_id}" --arg split "${split}" --arg kind "${kind}" \
    --arg cat "eval/faults/catalog.v1.json" --arg sha "$(sha256sum "${CATALOG}" | cut -d' ' -f1)" \
    --arg entry_id "${fault}/${variant}" --argjson fault "${fault_json}" \
    --argjson versions "$(versions_json)" --argjson before "$(pods_json model-server)" \
    --argjson load "$(loadavg_json)" --argjson mem "$(mem_available_mib)" \
    --argjson lg "$(kc -n "${GENAI_NAMESPACE}" get deploy loadgen -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)" \
    '{schema_version: 1, run_id: $id, split: $split, kind: $kind, status: "running",
      catalog: {file: $cat, sha256: $sha, entry_id: $entry_id},
      fault: $fault, decoys: [], expected_causes: [],
      timeline: {}, target: {namespace: "genai", workload: "deployment/model-server", pods_before: $before},
      versions: $versions,
      host: {nproc: ('"$(nproc)"'), loadavg_start: $load, mem_available_mib_start: $mem, cpu_busy_pct: {}},
      loadgen: {replicas: $lg, interval_s: 5},
      exports: {}, check: null, notes: []}' > "${dir}/run.json"
}

mark() { run_set "$1" '.timeline[$k] = $t' --arg k "$2" --arg t "$(now_ms)"; }

# shelve_run DIR: move an invalid or interrupted run out of its split
# directory into eval/runs/invalid/ (never committed), so that split
# directories only hold runs that can be scored or resumed.
shelve_run() {
  local dest
  dest="${RUNS_DIR}/invalid/$(basename "$1")--$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "${RUNS_DIR}/invalid"
  mv "$1" "${dest}"
  log "run record moved to ${dest#"${ROOT_DIR}/"}"
}
note() { run_set "$1" '.notes += [$n]' --arg n "$2"; }
set_status() { run_set "$1" '.status = $s' --arg s "$2"; }

# --- loadgen statistics ------------------------------------------------------
# loadgen_window START END -> per prompt {n, ok, errors, p50_ms} for lines
# whose timestamp is in [START, END).
loadgen_window() {
  kc -n "${GENAI_NAMESPACE}" logs deploy/loadgen --since-time="$(date -u -d "$1 - 60 seconds" +%Y-%m-%dT%H:%M:%SZ)" 2>/dev/null \
    | jq -s -c --arg s "$1" --arg e "$2" '
        def p50: sort | if length == 0 then null else .[(length / 2 | floor)] end;
        map(select(.ts >= ($s | .[0:19]) and .ts < ($e | .[0:19])))
        | group_by(.prompt_id)
        | map({key: .[0].prompt_id,
               value: {n: length, ok: (map(select(.status == 200)) | length),
                       errors: (map(select(.status != 200)) | length),
                       p50_ms: (map(select(.status == 200) | .client_latency_ms) | p50)}})
        | from_entries'
}

# prom_scalar QUERY [UNIX_TIME] -> first value or empty.
prom_scalar() {
  local q t="${2:-}"
  q="$(jq -rn --arg q "$1" '$q | @uri')"
  svc_get "${OBS_NAMESPACE}" "${PROM_SVC}" "/api/v1/query?query=${q}${t:+&time=${t}}" 2>/dev/null \
    | jq -r '.data.result[0].value[1] // empty'
}

# events_since NAMESPACE OBJECT_NAME ISO_TIME -> JSON array of events.
events_since() {
  kc -n "$1" get events --field-selector "involvedObject.name=$2" -o json 2>/dev/null \
    | jq -c --arg t "$3" '[.items[] | select(((.lastTimestamp // .eventTime // .firstTimestamp) // "") >= ($t | .[0:19]))
                            | {time: (.lastTimestamp // .eventTime), type, reason, message, count}]'
}

# --- export ------------------------------------------------------------------
# export_run DIR: events, pods, loadgen lines, traces and metrics of the run
# window, so the run can be re-scored offline after retention.
export_run() {
  local dir="$1" start end from to
  start="$(run_get "${dir}" '.timeline.baseline_start')"
  end="$(run_get "${dir}" '.timeline.end // empty')"
  [[ -n "${end}" ]] || end="$(now_ms)"
  from="$(date -u -d "${start} - 5 minutes" +%Y-%m-%dT%H:%M:%SZ)"
  to="$(date -u -d "${end} + 5 minutes" +%Y-%m-%dT%H:%M:%SZ)"

  kc get events -A -o json | jq --arg from "${from}" --arg to "${to}" '
    {window: {from: $from, to: $to},
     items: [.items[] | select(.involvedObject.namespace as $n | ["genai", "observability", "kube-system", "noise"] | index($n))
             | select(((.lastTimestamp // .eventTime // .firstTimestamp) // "") as $t | $t >= $from and $t <= $to)]}' \
    > "${dir}/events.json"

  kc get pods -n "${GENAI_NAMESPACE}" -o json > "${dir}/pods.json"

  kc -n "${GENAI_NAMESPACE}" logs deploy/loadgen --since-time="$(date -u -d "${start}" +%Y-%m-%dT%H:%M:%SZ)" 2>/dev/null \
    | jq -c --arg e "${end:0:19}" 'select(.ts <= $e)' > "${dir}/loadgen.jsonl" || true

  : > "${dir}/traces.jsonl"
  jq -r 'select(.trace_id != null) | .trace_id' "${dir}/loadgen.jsonl" | sort -u | while read -r tid; do
    svc_get "${OBS_NAMESPACE}" tempo:3200 "/api/v2/traces/${tid}" 2>/dev/null \
      | jq -c --arg id "${tid}" '{trace_id: $id, trace: .}' >> "${dir}/traces.jsonl" || true
  done
  gzip -nf "${dir}/traces.jsonl"

  local s e queries
  s="$(to_epoch "${from}")"
  e="$(to_epoch "${to}")"
  queries='{
    "cpu_throttled_periods": "container_cpu_cfs_throttled_periods_total{namespace=\"genai\"}",
    "cpu_periods": "container_cpu_cfs_periods_total{namespace=\"genai\"}",
    "cpu_usage_seconds": "container_cpu_usage_seconds_total{namespace=\"genai\",container!=\"\"}",
    "cpu_pressure_waiting_seconds": "container_pressure_cpu_waiting_seconds_total{namespace=\"genai\",container!=\"\"}",
    "memory_working_set_bytes": "container_memory_working_set_bytes{namespace=\"genai\",container!=\"\"}",
    "restarts": "kube_pod_container_status_restarts_total{namespace=\"genai\"}",
    "last_terminated_reason": "kube_pod_container_status_last_terminated_reason{namespace=\"genai\"}",
    "pod_ready": "kube_pod_status_ready{namespace=\"genai\",condition=\"true\"}",
    "pod_phase": "kube_pod_status_phase{namespace=\"genai\"} == 1",
    "genai_duration_count": "gen_ai_client_operation_duration_seconds_count",
    "genai_duration_sum": "gen_ai_client_operation_duration_seconds_sum",
    "llama_predicted_tokens_per_second": "llamacpp:predicted_tokens_seconds",
    "llama_requests_deferred": "llamacpp:requests_deferred"
  }'
  jq -n '{}' > "${dir}/metrics.json"
  while IFS=$'\t' read -r name query; do
    local qe body
    qe="$(jq -rn --arg q "${query}" '$q | @uri')"
    body="$(svc_get "${OBS_NAMESPACE}" "${PROM_SVC}" "/api/v1/query_range?query=${qe}&start=${s}&end=${e}&step=15" 2>/dev/null || echo '{}')"
    local tmp
    tmp="$(mktemp "${dir}/.metrics.XXXXXX")"
    jq --arg n "${name}" --arg q "${query}" --argjson b "${body}" '.[$n] = {query: $q, result: ($b.data.result // [])}' \
      "${dir}/metrics.json" > "${tmp}" && mv "${tmp}" "${dir}/metrics.json"
  done < <(jq -r 'to_entries[] | "\(.key)\t\(.value)"' <<<"${queries}")

  # Everything but run.json and the small text files is stored gzipped
  # (eval/README.md, "What is committed"); -n keeps the files reproducible.
  gzip -nf "${dir}/events.json" "${dir}/pods.json" "${dir}/loadgen.jsonl" "${dir}/metrics.json"
  run_set "${dir}" '.exports = {events: "events.json.gz", pods: "pods.json.gz", loadgen: "loadgen.jsonl.gz",
                                traces: "traces.jsonl.gz", metrics: "metrics.json.gz", check: "check.txt"}'
}

# chat_api_hooks -> "1" if the lab fault hooks are on in the chat-api
# Deployment, "0" otherwise.
chat_api_hooks() {
  local v
  v="$(kc -n "${GENAI_NAMESPACE}" get deploy chat-api -o json 2>/dev/null \
    | jq -r '.spec.template.spec.containers[0].env[]? | select(.name == "LAB_FAULT_HOOKS") | .value')"
  [[ "${v}" == 1 ]] && echo 1 || echo 0
}

# active_fault -> name of the fault of the active run, or empty.
active_fault() {
  [[ -s "${ACTIVE_RUN}" ]] || return 0
  jq -r '.fault.name // empty' "$(cat "${ACTIVE_RUN}")/run.json" 2>/dev/null || true
}

# genai_changes_since SECONDS -> JSON array of changes in the genai namespace
# during the last SECONDS: Kubernetes events and pods created. Used by the
# quiet-period precondition of run.sh.
genai_changes_since() {
  local since
  since="$(date -u -d "-$1 seconds" +%Y-%m-%dT%H:%M:%SZ)"
  {
    kc -n "${GENAI_NAMESPACE}" get events -o json \
      | jq -c --arg s "${since}" '[.items[] | ((.lastTimestamp // .eventTime // .firstTimestamp) // "" | .[0:19] + "Z") as $t
          | select($t >= $s) | {time: $t, what: "event \(.reason) on \(.involvedObject.kind)/\(.involvedObject.name)"}]'
    kc -n "${GENAI_NAMESPACE}" get pods -o json \
      | jq -c --arg s "${since}" '[.items[] | select(.metadata.creationTimestamp >= $s)
          | {time: .metadata.creationTimestamp, what: "pod \(.metadata.name) created"}]'
  } | jq -sc 'add | sort_by(.time)'
}

# --- reset to the declared state ----------------------------------------------
# reapply_declared: model-server, chat-api and loadgen manifests as lab-up
# renders them (without rebuilding or reloading images), then wait.
reapply_declared() {
  export LLAMACPP_IMAGE MODEL_FILE
  # Faults never change the chat-api image, so the image currently in the
  # Deployment is the declared one; reading it avoids a rebuild/reload.
  CHAT_API_IMAGE="$(kc -n "${GENAI_NAMESPACE}" get deploy chat-api -o jsonpath='{.spec.template.spec.containers[0].image}')"
  [[ -n "${CHAT_API_IMAGE}" ]] || die "cannot read the chat-api image from its Deployment"
  # Keep the current hook state: it is only changed between runs, by
  # lab/scripts/lab-hooks.sh, never by a fault reset.
  LAB_FAULT_HOOKS="$(chat_api_hooks)"
  export CHAT_API_IMAGE LAB_FAULT_HOOKS
  local rendered
  # shellcheck disable=SC2016
  rendered="$(envsubst '${LLAMACPP_IMAGE} ${MODEL_FILE}' < "${LAB_DIR}/genai/model-server.yaml"
              echo "---"
              for m in chat-api loadgen; do
                envsubst '${CHAT_API_IMAGE} ${LAB_FAULT_HOOKS}' < "${LAB_DIR}/genai/${m}.yaml"
                echo "---"
              done)"
  local objects
  # The objects come from `create --dry-run=client` (the manifest alone):
  # `apply --dry-run=client` returns the live object merged with the
  # manifest, so the drift would be written back.
  objects="$(kc create --dry-run=client -o json -f - <<<"${rendered}" | jq -c '(.items // [.])[]')"
  # Non-Deployment objects (namespace, services) are applied.
  jq -c 'select(.kind != "Deployment")' <<<"${objects}" | while read -r obj; do
    kc apply -f - >/dev/null <<<"${obj}"
  done
  # Deployments are replaced, never applied: `kubectl apply` is a three-way
  # merge, and a field added by `kubectl patch` that was never in the applied
  # manifest (a nodeSelector, an initContainer) survives it. Replacing puts
  # back exactly the declared spec. A replace can hit a Conflict while the
  # controller updates the object, so it is retried. --save-config keeps the
  # annotation that later `kubectl apply` calls rely on.
  jq -c 'select(.kind == "Deployment")' <<<"${objects}" | while read -r obj; do
    local attempt out
    for attempt in 1 2 3 4 5; do
      if out="$(kc replace --save-config -f - 2>&1 <<<"${obj}")"; then
        break
      fi
      if [[ "${out}" != *Conflict* || "${attempt}" == 5 ]]; then
        echo "${out}" >&2
        return 1
      fi
      sleep 2
    done
  done
  kc -n "${GENAI_NAMESPACE}" rollout status deploy/model-server --timeout=5m >/dev/null
  kc -n "${GENAI_NAMESPACE}" rollout status deploy/chat-api --timeout=3m >/dev/null
  kc -n "${GENAI_NAMESPACE}" rollout status deploy/loadgen --timeout=3m >/dev/null
}

# wait_chat_ok TIMEOUT_S: until one /chat call through the Service returns 200.
wait_chat_ok() {
  local deadline=$(( $(date +%s) + $1 )) pod code
  while (( $(date +%s) < deadline )); do
    pod="$(running_pod chat-api)"
    if [[ -n "${pod}" ]]; then
      code="$(kc -n "${GENAI_NAMESPACE}" exec "${pod}" -c chat-api -- python -c '
import json, urllib.request
req = urllib.request.Request("http://chat-api.genai.svc.cluster.local:8000/chat", data=b"{\"prompt_id\": \"check\"}",
                             headers={"content-type": "application/json"}, method="POST")
try:
    print(urllib.request.urlopen(req, timeout=60).status)
except Exception as e:
    print(getattr(e, "code", 0))' 2>/dev/null || echo 0)"
      [[ "${code}" == 200 ]] && return 0
    fi
    sleep 5
  done
  return 1
}
