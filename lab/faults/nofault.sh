#!/usr/bin/env bash
# nofault windows (quiet or with a decoy pod in namespace noise).
#   inject RUN_DIR VARIANT   start the decoy if the variant has one
#   reset  RUN_DIR           delete any decoy pod in namespace noise
#   check  RUN_DIR           verify GenAI stayed healthy and the decoy fired
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

NOISE_NS="noise"

# wait_decoy POD REASON TIMEOUT_S: wait until the decoy container terminated
# with REASON and restarted at least once.
wait_decoy() {
  local pod="$1" want="$2" deadline=$(( $(date +%s) + $3 ))
  while (( $(date +%s) < deadline )); do
    if kc -n "${NOISE_NS}" get pod "${pod}" -o json 2>/dev/null | jq -e --arg r "${want}" '
        .status.containerStatuses[0] as $c
        | ($c.restartCount // 0) >= 1
          and (($c.lastState.terminated.reason // $c.state.terminated.reason // "") == $r)' >/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 1
}

inject() {
  local dir="$1" variant="$2" ms_node other_node node pod mech want_reason cmd what img
  if [[ "${variant}" == quiet ]]; then
    run_set "${dir}" '.decoys = []'
    return 0
  fi

  ms_node="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')"
  [[ -n "${ms_node}" ]] || die "no running model-server pod"
  other_node="$(kc get nodes -o json | jq -r --arg m "${ms_node}" '
    [.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null and .metadata.name != $m)][0].metadata.name // empty')"
  [[ -n "${other_node}" ]] || die "no worker node other than ${ms_node}"

  case "${variant}" in
    decoy-same-node)
      node="${ms_node}"; pod="decoy-crash"; mech="crash-loop"; want_reason="Error"
      cmd='["sh", "-c", "exit 1"]'
      ;;
    decoy-other-node-crash)
      node="${other_node}"; pod="decoy-crash"; mech="crash-loop"; want_reason="Error"
      cmd='["sh", "-c", "exit 1"]'
      ;;
    decoy-other-node-oom)
      node="${other_node}"; pod="decoy-oom"; mech="oomkill"; want_reason="OOMKilled"
      cmd='["python", "-c", "x = bytearray(80 * 1024 * 1024)"]'
      ;;
    decoy-same-node-oom)
      node="${ms_node}"; pod="decoy-oom"; mech="oomkill"; want_reason="OOMKilled"
      cmd='["python", "-c", "x = bytearray(80 * 1024 * 1024)"]'
      ;;
    *) die "unknown nofault variant ${variant}" ;;
  esac

  img="$(kc -n "${GENAI_NAMESPACE}" get deploy chat-api -o jsonpath='{.spec.template.spec.containers[0].image}')"
  [[ -n "${img}" ]] || die "cannot read chat-api image"
  kc create namespace "${NOISE_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null
  kc -n "${NOISE_NS}" delete pod -l app.kubernetes.io/part-of=kubepilot-decoy --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true

  kc -n "${NOISE_NS}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: ${NOISE_NS}
  labels:
    app.kubernetes.io/name: ${pod}
    app.kubernetes.io/part-of: kubepilot-decoy
spec:
  nodeName: ${node}
  restartPolicy: Always
  terminationGracePeriodSeconds: 0
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: decoy
      image: ${img}
      command: ${cmd}
      resources:
        requests: {cpu: "10m", memory: "32Mi"}
        limits: {cpu: "100m", memory: "32Mi"}
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
EOF

  what="$(jq -r --arg v "${variant}" 'first(.nofault.variants[] | select(.id == $v) | .decoys[0].what) // ""' "${CATALOG}")"
  run_set "${dir}" '.decoys = [{variant: $v, what: $w, namespace: $ns, pod: $p, node: $n, model_server_node: $mn, mechanism: $m}]' \
    --arg v "${variant}" --arg w "${what}" --arg ns "${NOISE_NS}" --arg p "${pod}" \
    --arg n "${node}" --arg mn "${ms_node}" --arg m "${mech}"

  if ! wait_decoy "${pod}" "${want_reason}" 90; then
    note "${dir}" "decoy ${NOISE_NS}/${pod} did not reach ${want_reason} with restartCount>=1 within 90s"
  fi
}

reset() {
  kc -n "${NOISE_NS}" delete pod -l app.kubernetes.io/part-of=kubepilot-decoy --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
}

check() {
  local dir="$1" start done_t stats ok errors n_decoys rc=0
  start="$(run_get "${dir}" '.timeline.inject_start')"
  done_t="$(run_get "${dir}" '.timeline.inject_done')"
  stats="$(loadgen_window "${done_t}" "$(now_ms)")"
  ok="$(jq '[.[] | .ok] | add // 0' <<<"${stats}")"
  errors="$(jq '[.[] | .errors] | add // 0' <<<"${stats}")"
  if [[ "${ok}" -ge 1 && "${errors}" -eq 0 ]]; then
    echo "PASS  load generator: ${ok} successful requests, 0 failed in the window"
  else
    echo "FAIL  load generator: ${ok} successful, ${errors} failed in the window (want >= 1 ok and 0 failed)"
    rc=1
  fi

  n_decoys="$(run_get "${dir}" '.decoys | length')"
  if [[ "${n_decoys}" -eq 0 ]]; then
    local noise_pods
    noise_pods="$(kc -n "${NOISE_NS}" get pods -o json 2>/dev/null | jq '.items | length' || echo 0)"
    if [[ "${noise_pods:-0}" -eq 0 ]]; then
      echo "PASS  quiet window: 0 pods in namespace ${NOISE_NS}"
    else
      echo "FAIL  quiet window: ${noise_pods} pod(s) present in namespace ${NOISE_NS}"
      rc=1
    fi
    return "${rc}"
  fi

  local variant pod want_node ms_node mech actual_node reason restarts want_reason ksm backoff
  variant="$(run_get "${dir}" '.decoys[0].variant')"
  pod="$(run_get "${dir}" '.decoys[0].pod')"
  want_node="$(run_get "${dir}" '.decoys[0].node')"
  ms_node="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')"
  mech="$(run_get "${dir}" '.decoys[0].mechanism')"
  actual_node="$(kc -n "${NOISE_NS}" get pod "${pod}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"

  if [[ "${variant}" == *same-node* && "${actual_node}" == "${ms_node}" && "${actual_node}" == "${want_node}" ]]; then
    echo "PASS  decoy ${NOISE_NS}/${pod} on model-server node ${actual_node}"
  elif [[ "${variant}" == *other-node* && -n "${actual_node}" && "${actual_node}" != "${ms_node}" && "${actual_node}" == "${want_node}" ]]; then
    echo "PASS  decoy ${NOISE_NS}/${pod} on node ${actual_node} (model-server on ${ms_node})"
  else
    echo "FAIL  decoy ${NOISE_NS}/${pod} node='${actual_node}', want='${want_node}', model-server='${ms_node}'"
    rc=1
  fi

  want_reason="Error"
  [[ "${mech}" == oomkill ]] && want_reason="OOMKilled"
  reason="$(kc -n "${NOISE_NS}" get pod "${pod}" -o json 2>/dev/null \
    | jq -r '.status.containerStatuses[0] | (.lastState.terminated.reason // .state.terminated.reason // "none")')"
  restarts="$(kc -n "${NOISE_NS}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)"
  if [[ "${reason}" == "${want_reason}" && "${restarts:-0}" -ge 1 ]]; then
    echo "PASS  decoy ${NOISE_NS}/${pod}: last termination ${reason}, ${restarts} restarts"
  else
    echo "FAIL  decoy ${NOISE_NS}/${pod}: last termination '${reason}' (want ${want_reason}), ${restarts:-0} restarts"
    rc=1
  fi

  if [[ "${mech}" == oomkill ]]; then
    ksm=""
    for _ in $(seq 1 18); do
      ksm="$(prom_scalar "kube_pod_container_status_last_terminated_reason{namespace=\"${NOISE_NS}\",pod=\"${pod}\",reason=\"OOMKilled\"}")"
      [[ "${ksm}" == 1 ]] && break
      sleep 5
    done
    if [[ "${ksm}" == 1 ]]; then
      echo "PASS  kube-state-metrics: last_terminated_reason{namespace=\"${NOISE_NS}\",pod=\"${pod}\",reason=\"OOMKilled\"} == 1"
    else
      echo "FAIL  kube-state-metrics shows no OOMKilled termination for ${NOISE_NS}/${pod}"
      rc=1
    fi
  fi

  backoff="$(events_since "${NOISE_NS}" "${pod}" "${start}" | jq '[.[] | select(.reason == "BackOff")] | length')"
  if [[ "${backoff}" -ge 1 ]]; then
    echo "PASS  BackOff event present for ${NOISE_NS}/${pod}"
  else
    echo "FAIL  no BackOff event for ${NOISE_NS}/${pod} since ${start}"
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
