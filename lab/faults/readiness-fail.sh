#!/usr/bin/env bash
# readiness-fail fault on the model server.
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           nothing specific: the Deployment reset covers it
#   check  RUN_DIR           exit 0 only if the pod really runs without being Ready
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

patch_readiness() { # patch_readiness JSON_FRAGMENT
  kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type strategic \
    -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"model-server\",\"readinessProbe\":$1}]}}}}" >/dev/null
}

# newest_pod START -> newest model-server pod created after START.
newest_pod() {
  kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server -o json \
    | jq -r --arg t "${1:0:19}" '[.items[] | select(.metadata.creationTimestamp >= $t)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty'
}

inject() {
  local dir="$1" variant="$2" start deadline pod
  start="$(run_get "${dir}" '.timeline.inject_start')"
  case "${variant}" in
    probe-path-404) patch_readiness '{"httpGet":{"path":"/does-not-exist","port":"http"}}' ;;
    probe-wrong-port) patch_readiness '{"httpGet":{"path":"/health","port":9999}}' ;;
    initial-delay-600) patch_readiness '{"initialDelaySeconds":600}' ;;
    server-wrong-port)
      # The probe is right, the server listens elsewhere. Remove startupProbe
      # and livenessProbe so the container stays Running (0 restarts) and
      # readinessProbe runs against 8080; 8081 only appears in containers[0].args.
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type json -p '[
        {"op":"test","path":"/spec/template/spec/containers/0/args/4","value":"--port"},
        {"op":"replace","path":"/spec/template/spec/containers/0/args/5","value":"8081"},
        {"op":"remove","path":"/spec/template/spec/containers/0/startupProbe"},
        {"op":"remove","path":"/spec/template/spec/containers/0/livenessProbe"}]' >/dev/null
      ;;
    *) die "unknown readiness-fail variant ${variant}" ;;
  esac
  run_set "${dir}" '.fault.applied = {variant: $v}' --arg v "${variant}"
  # Wait for the replacement pod to be running (it never becomes Ready).
  deadline=$(( $(date +%s) + 120 ))
  while (( $(date +%s) < deadline )); do
    pod="$(newest_pod "${start}")"
    [[ -n "${pod}" && "$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.phase}')" == Running ]] && return 0
    sleep 2
  done
  note "${dir}" "no running model-server pod created after the injection within 120s"
}

reset() { :; }

check() {
  local dir="$1" start variant pod running ready ready_eps unhealthy delay ksm errors rc=0
  start="$(run_get "${dir}" '.timeline.inject_start')"
  variant="$(run_get "${dir}" '.fault.variant // empty')"
  pod="$(newest_pod "${start}")"
  if [[ -z "${pod}" ]]; then
    echo "FAIL  no model-server pod created after the injection (${start})"
    return 1
  fi
  running="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json | jq -r '.status.containerStatuses[0].state | has("running")')"
  ready="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json | jq -r '[.status.conditions[]? | select(.type == "Ready")] | first | .status')"
  if [[ "${running}" == true && "${ready}" == False ]]; then
    echo "PASS  pod ${pod}: container running, Ready=False"
  else
    echo "FAIL  pod ${pod}: container running=${running}, Ready=${ready}"
    rc=1
  fi
  ready_eps="$(kc -n "${GENAI_NAMESPACE}" get endpointslices -l kubernetes.io/service-name=model-server -o json \
    | jq '[.items[].endpoints[]? | select(.conditions.ready == true)] | length')"
  if [[ "${ready_eps}" -eq 0 ]]; then
    echo "PASS  Service model-server has no ready endpoint"
  else
    echo "FAIL  Service model-server has ${ready_eps} ready endpoint(s)"
    rc=1
  fi
  if [[ "${variant}" == initial-delay-600 ]]; then
    # The probe has not run yet: no Unhealthy event is expected; the evidence
    # is the pod spec.
    delay="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.spec.containers[0].readinessProbe.initialDelaySeconds}')"
    if [[ "${delay:-0}" -ge 600 ]]; then
      echo "PASS  readiness probe initialDelaySeconds=${delay} in the pod spec"
    else
      echo "FAIL  readiness probe initialDelaySeconds=${delay:-unset}"
      rc=1
    fi
  else
    unhealthy="$(events_since "${GENAI_NAMESPACE}" "${pod}" "${start}" \
      | jq -r '[.[] | select(.reason == "Unhealthy" and (.message | startswith("Readiness probe failed")))] | last | .message // empty')"
    if [[ -n "${unhealthy}" ]]; then
      echo "PASS  Unhealthy event: ${unhealthy}"
    else
      echo "FAIL  no 'Readiness probe failed' event for ${pod} since ${start}"
      rc=1
    fi
  fi
  ksm=""
  for _ in $(seq 1 18); do
    ksm="$(prom_scalar "kube_pod_status_ready{namespace=\"genai\",pod=\"${pod}\",condition=\"true\"}")"
    [[ "${ksm}" == 0 ]] && break
    sleep 5
  done
  if [[ "${ksm}" == 0 ]]; then
    echo "PASS  kube-state-metrics: kube_pod_status_ready{condition=\"true\",pod=\"${pod}\"} == 0"
  else
    echo "FAIL  kube-state-metrics ready value for ${pod}: '${ksm}'"
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
