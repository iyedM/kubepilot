#!/usr/bin/env bash
# unschedulable fault on the model server.
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           undo what the Deployment reset does not cover
#   check  RUN_DIR           exit 0 only if the scheduling failure is really there
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

TAINT_KEY="kubepilot.dev/maintenance"
PVC_NAME="model-scratch"

# Expected FailedScheduling message fragment per variant.
expected_message() {
  case "$1" in
    nodeselector-gpu) echo "didn't match Pod's node affinity/selector" ;;
    cpu-request-64) echo "Insufficient cpu" ;;
    node-taint) echo "untolerated taint" ;;
    pvc-unbound) echo "unbound immediate PersistentVolumeClaims" ;;
  esac
}

workers() { kc get nodes -o json | jq -r '.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null) | .metadata.name'; }

# wait_pending INJECT_START TIMEOUT_S: until a model-server pod created after
# the injection is Pending with PodScheduled=False.
wait_pending() {
  local deadline=$(( $(date +%s) + $2 ))
  while (( $(date +%s) < deadline )); do
    if kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server -o json | jq -e --arg t "${1:0:19}" '
        [.items[] | select(.metadata.creationTimestamp >= $t and .status.phase == "Pending")
         | select(any(.status.conditions[]?; .type == "PodScheduled" and .status == "False"))] | length > 0' >/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 1
}

inject() {
  local dir="$1" variant="$2" start
  start="$(run_get "${dir}" '.timeline.inject_start')"
  case "${variant}" in
    nodeselector-gpu)
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type strategic \
        -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubepilot.dev/gpu":"true"}}}}}' >/dev/null
      ;;
    cpu-request-64)
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type strategic \
        -p '{"spec":{"template":{"spec":{"containers":[{"name":"model-server","resources":{"requests":{"cpu":"64"},"limits":{"cpu":"64"}}}]}}}}' >/dev/null
      ;;
    node-taint)
      # Node-side cause: the pod spec is unchanged; the running pod is deleted
      # so that its replacement has to be scheduled on tainted nodes.
      for n in $(workers); do kc taint nodes "${n}" "${TAINT_KEY}=true:NoSchedule" --overwrite >/dev/null; done
      kc -n "${GENAI_NAMESPACE}" delete pod -l app.kubernetes.io/name=model-server --wait=false >/dev/null
      ;;
    pvc-unbound)
      kc -n "${GENAI_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_NAME}
  namespace: ${GENAI_NAMESPACE}
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: $(run_get "${dir}" '.fault.params.storageClassName')
  resources:
    requests:
      storage: 1Gi
EOF
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type strategic -p "{\"spec\":{\"template\":{\"spec\":{
        \"volumes\":[{\"name\":\"scratch\",\"persistentVolumeClaim\":{\"claimName\":\"${PVC_NAME}\"}}],
        \"containers\":[{\"name\":\"model-server\",\"volumeMounts\":[{\"name\":\"scratch\",\"mountPath\":\"/scratch\"}]}]}}}}" >/dev/null
      ;;
    *) die "unknown unschedulable variant ${variant}" ;;
  esac
  run_set "${dir}" '.fault.applied = {variant: $v}' --arg v "${variant}"
  if ! wait_pending "${start}" 120; then
    note "${dir}" "no Pending model-server pod created after the injection within 120s"
  fi
}

reset() {
  local dir="$1"
  # The Deployment itself is put back by reapply_declared (exact replace).
  case "$(run_get "${dir}" '.fault.variant')" in
    node-taint)
      for n in $(workers); do kc taint nodes "${n}" "${TAINT_KEY}-" >/dev/null 2>&1 || true; done
      ;;
    pvc-unbound)
      kc -n "${GENAI_NAMESPACE}" delete pvc "${PVC_NAME}" --ignore-not-found --wait=false >/dev/null
      ;;
  esac
}

check() {
  local dir="$1" start variant pod phase sched msg want ksm errors rc=0
  start="$(run_get "${dir}" '.timeline.inject_start')"
  variant="$(run_get "${dir}" '.fault.variant // empty')"
  pod="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server -o json \
    | jq -r --arg t "${start:0:19}" '[.items[] | select(.metadata.creationTimestamp >= $t)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty')"
  if [[ -z "${pod}" ]]; then
    echo "FAIL  no model-server pod created after the injection (${start})"
    return 1
  fi
  phase="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.phase}')"
  sched="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json | jq -r '[.status.conditions[]? | select(.type == "PodScheduled")] | first | "\(.status)/\(.reason // "")"')"
  if [[ "${phase}" == Pending && "${sched}" == False/Unschedulable ]]; then
    echo "PASS  pod ${pod}: Pending, PodScheduled=${sched}"
  else
    echo "FAIL  pod ${pod}: phase ${phase}, PodScheduled=${sched}"
    rc=1
  fi
  msg="$(events_since "${GENAI_NAMESPACE}" "${pod}" "${start}" | jq -r '[.[] | select(.reason == "FailedScheduling") | .message] | last // empty')"
  want="$(expected_message "${variant}")"
  if [[ -n "${msg}" && ( -z "${want}" || "${msg}" == *"${want}"* ) ]]; then
    echo "PASS  FailedScheduling event: ${msg}"
  else
    echo "FAIL  FailedScheduling event missing or unexpected (want '${want:-any}'): '${msg}'"
    rc=1
  fi
  ksm=""
  for _ in $(seq 1 18); do
    ksm="$(prom_scalar "kube_pod_status_unschedulable{namespace=\"genai\",pod=\"${pod}\"}")"
    [[ "${ksm}" == 1 ]] && break
    sleep 5
  done
  if [[ "${ksm}" == 1 ]]; then
    echo "PASS  kube-state-metrics: kube_pod_status_unschedulable{pod=\"${pod}\"} == 1"
  else
    echo "FAIL  kube-state-metrics does not report ${pod} as unschedulable"
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
