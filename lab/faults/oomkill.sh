#!/usr/bin/env bash
# oomkill fault.
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           nothing specific: the caller re-applies the manifests
#   check  RUN_DIR           exit 0 only if an OOM kill really happened
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

# target_app RUN_DIR -> app label of the target (model-server by default).
target_app() {
  local t
  t="$(run_get "$1" '.fault.params.target // "deployment/model-server"')"
  echo "${t#deployment/}"
}

# wait_oom APP INJECT_START TIMEOUT_S: until a pod of APP created after the
# injection reports an OOMKilled termination.
wait_oom() {
  local deadline=$(( $(date +%s) + $3 ))
  while (( $(date +%s) < deadline )); do
    if kc -n "${GENAI_NAMESPACE}" get pods -l "app.kubernetes.io/name=$1" -o json | jq -e --arg t "${2:0:19}" '
        [.items[] | select(.metadata.creationTimestamp >= $t) | .status.containerStatuses[]?
         | select(.lastState.terminated.reason == "OOMKilled" or .state.terminated.reason == "OOMKilled")] | length > 0' >/dev/null; then
      return 0
    fi
    sleep 3
  done
  return 1
}

inject() {
  local dir="$1" variant="$2" mem app start
  mem="$(run_get "${dir}" '.fault.params.memory_limit')"
  start="$(run_get "${dir}" '.timeline.inject_start')"
  case "${variant}" in
    noload-*)
      # Model loaded in anonymous memory (--load-mode none): the limit applies
      # to it deterministically, unlike mmap (docs/lab.md, increment 3a).
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type json -p "[
        {\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--load-mode\"},
        {\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"none\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/requests/memory\",\"value\":\"${mem}\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/limits/memory\",\"value\":\"${mem}\"}]" >/dev/null
      ;;
    kv-cache-ctx32k)
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type json -p "[
        {\"op\":\"test\",\"path\":\"/spec/template/spec/containers/0/args/7\",\"value\":\"-c\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/8\",\"value\":\"$(run_get "${dir}" '.fault.params.context')\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/requests/memory\",\"value\":\"${mem}\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/limits/memory\",\"value\":\"${mem}\"}]" >/dev/null
      ;;
    app-oom-*)
      kc -n "${GENAI_NAMESPACE}" patch deploy chat-api --type json -p "[
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/requests/memory\",\"value\":\"${mem}\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/resources/limits/memory\",\"value\":\"${mem}\"}]" >/dev/null
      ;;
    *) die "unknown oomkill variant ${variant}" ;;
  esac
  app="$(target_app "${dir}")"
  run_set "${dir}" '.fault.applied = {app: $a, memory_limit: $m}' --arg a "${app}" --arg m "${mem}"
  if ! wait_oom "${app}" "${start}" 180; then
    note "${dir}" "no OOMKilled termination observed within 180s of the injection"
  fi
}

reset() { :; }

check() {
  local dir="$1" app start pod reason restarts ksm backoff errors rc=0
  app="$(target_app "${dir}")"
  start="$(run_get "${dir}" '.timeline.inject_start')"
  # Newest pod of the target created after the injection (it crash-loops, so
  # it is not necessarily Ready).
  pod="$(kc -n "${GENAI_NAMESPACE}" get pods -l "app.kubernetes.io/name=${app}" -o json \
    | jq -r --arg t "${start:0:19}" '[.items[] | select(.metadata.creationTimestamp >= $t)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty')"
  if [[ -z "${pod}" ]]; then
    echo "FAIL  no ${app} pod created after the injection (${start})"
    return 1
  fi
  reason="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json | jq -r '.status.containerStatuses[0] | (.lastState.terminated.reason // .state.terminated.reason // "none")')"
  restarts="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  if [[ "${reason}" == OOMKilled && "${restarts}" -ge 1 ]]; then
    echo "PASS  pod ${pod}: last termination OOMKilled, ${restarts} restarts"
  else
    echo "FAIL  pod ${pod}: last termination '${reason}', ${restarts} restarts"
    rc=1
  fi
  ksm=""
  for _ in $(seq 1 18); do
    ksm="$(prom_scalar "kube_pod_container_status_last_terminated_reason{namespace=\"genai\",pod=\"${pod}\",reason=\"OOMKilled\"}")"
    [[ "${ksm}" == 1 ]] && break
    sleep 5
  done
  if [[ "${ksm}" == 1 ]]; then
    echo "PASS  kube-state-metrics: last_terminated_reason{reason=\"OOMKilled\",pod=\"${pod}\"} == 1"
  else
    echo "FAIL  kube-state-metrics shows no OOMKilled termination for ${pod}"
    rc=1
  fi
  backoff="$(events_since "${GENAI_NAMESPACE}" "${pod}" "${start}" | jq '[.[] | select(.reason == "BackOff")] | length')"
  if [[ "${backoff}" -ge 1 ]]; then
    echo "PASS  BackOff event present for ${pod}"
  else
    echo "FAIL  no BackOff event for ${pod} since ${start}"
    rc=1
  fi
  errors="$(loadgen_window "$(run_get "${dir}" '.timeline.inject_done')" "$(now_ms)" | jq '[.[] | .errors] | add // 0')"
  if [[ "${errors}" -ge 1 ]]; then
    echo "PASS  load generator saw ${errors} failed requests in the fault window"
  else
    echo "FAIL  load generator saw no failed request in the fault window"
    rc=1
  fi
  return "${rc}"
}

case "${1:-}" in
  inject) inject "$2" "$3" ;;
  reset) reset "$2" ;;
  check) check "$2" ;;
  *) die "usage: $0 inject RUN_DIR VARIANT | reset RUN_DIR | check RUN_DIR" ;;
esac
