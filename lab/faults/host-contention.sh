#!/usr/bin/env bash
# host-contention fault: other work competes for CPU with the model server
# (CPU pressure rises, no CFS throttling, inference slows down).
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           stop the competing load
#   check  RUN_DIR           exit 0 only if the contention signal is really there
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

NOISE_NS="noise"
NEIGHBOR="cpu-neighbor"
SAFETY_S=480                   # busy loops stop by themselves after this
MIN_PSI_RATIO="5"              # measured x58 (0.18 s -> 10.3 s per 3 min)
MIN_PSI_SECONDS="3"
MAX_THROTTLED_FRACTION="0.05"
MIN_LATENCY_RATIO="1.3"        # measured x1.85-2.2; quiet spread <= x1.05

inject() {
  local dir="$1" variant="$2" loops node marker
  loops="$(run_get "${dir}" '.fault.params.busy_loops')"
  case "${variant}" in
    host-busy-loops-*)
      # Busy loops on the host, outside the cluster. Each command line carries
      # a marker unique to the run, so reset can find and stop every loop
      # (killing a process group is not enough: timeout(1) puts its command
      # in a group of its own). Each loop also stops by itself after SAFETY_S
      # in case the run dies.
      marker="kubepilot-burn-$(basename "${dir}")"
      for _ in $(seq 1 "${loops}"); do
        timeout "${SAFETY_S}" sh -c ": ${marker}; while :; do :; done" </dev/null >/dev/null 2>&1 &
      done
      sleep 1
      run_set "${dir}" '.fault.applied = {variant: $v, marker: $m, busy_loops: ($l | tonumber), running: ($n | tonumber)}' \
        --arg v "${variant}" --arg m "${marker}" --arg l "${loops}" --arg n "$(pgrep -fc -- "^sh -c : ${marker}" || true)"
      ;;
    neighbor-pod-same-node)
      node="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}')"
      [[ -n "${node}" ]] || die "no running model-server pod to place the neighbor next to"
      kc create namespace "${NOISE_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null
      kc -n "${NOISE_NS}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${NEIGHBOR}
  namespace: ${NOISE_NS}
  labels: {app.kubernetes.io/name: ${NEIGHBOR}}
spec:
  nodeName: ${node}
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: burn
      image: $(kc -n "${GENAI_NAMESPACE}" get deploy chat-api -o jsonpath='{.spec.template.spec.containers[0].image}')
      command: ["sh", "-c", "for i in \$(seq 1 ${loops}); do timeout ${SAFETY_S} sh -c 'while :; do :; done' & done; wait"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
EOF
      kc -n "${NOISE_NS}" wait --for=condition=Ready "pod/${NEIGHBOR}" --timeout=60s >/dev/null
      run_set "${dir}" '.fault.applied = {variant: $v, neighbor: ($ns + "/" + $p), node: $n, busy_loops: ($l | tonumber)}' \
        --arg v "${variant}" --arg ns "${NOISE_NS}" --arg p "${NEIGHBOR}" --arg n "${node}" --arg l "${loops}"
      ;;
    *) die "unknown host-contention variant ${variant}" ;;
  esac
}

reset() {
  local dir="$1" marker left
  marker="$(run_get "${dir}" '.fault.applied.marker // empty')"
  if [[ -n "${marker}" ]]; then
    pkill -TERM -f -- "${marker}" || true
    for _ in $(seq 1 10); do
      pgrep -f -- "${marker}" >/dev/null || break
      sleep 1
    done
    if pgrep -f -- "${marker}" >/dev/null; then
      pkill -KILL -f -- "${marker}" || true
      sleep 1
    fi
    left="$(pgrep -fc -- "${marker}" || true)"
    [[ "${left:-0}" -eq 0 ]] || die "host-contention reset: ${left} busy loop(s) still running (${marker})"
  fi
  if [[ "$(run_get "${dir}" '.fault.variant')" == neighbor-pod-same-node ]]; then
    kc -n "${NOISE_NS}" delete pod "${NEIGHBOR}" --ignore-not-found --wait=false >/dev/null
  fi
}

check() {
  local dir="$1" base_start start done_t pod now dur base_dur sel psi_f psi_b rate_f rate_b throttled periods fraction base slow ratio rc=0
  base_start="$(run_get "${dir}" '.timeline.baseline_start')"
  start="$(run_get "${dir}" '.timeline.inject_start')"
  done_t="$(run_get "${dir}" '.timeline.inject_done')"
  pod="$(running_pod model-server)"
  [[ -n "${pod}" ]] || { echo "FAIL  no running model-server pod"; return 1; }
  now="$(date +%s)"
  dur=$(( now - $(to_epoch "${done_t}") ))
  base_dur=$(( $(to_epoch "${start}") - $(to_epoch "${base_start}") ))
  sel="namespace=\"genai\",container=\"model-server\",pod=\"${pod}\""
  psi_f="$(prom_scalar "increase(container_pressure_cpu_waiting_seconds_total{${sel}}[${dur}s])" "${now}")"
  psi_b="$(prom_scalar "increase(container_pressure_cpu_waiting_seconds_total{${sel}}[${base_dur}s])" "$(to_epoch "${start}")")"
  rate_f="$(awk -v x="${psi_f:-0}" -v d="${dur}" 'BEGIN {printf "%.4f", x / d}')"
  rate_b="$(awk -v x="${psi_b:-0}" -v d="${base_dur}" 'BEGIN {printf "%.4f", (d > 0 ? x / d : 0)}')"
  if awk -v f="${rate_f}" -v b="${rate_b}" -v x="${psi_f:-0}" -v r="${MIN_PSI_RATIO}" -v m="${MIN_PSI_SECONDS}" \
      'BEGIN {exit !(x >= m && f >= r * (b > 0 ? b : 0.0001))}'; then
    echo "PASS  CPU pressure (PSI waiting) of ${pod}: ${psi_f} s over ${dur} s vs ${psi_b:-0} s over ${base_dur} s of baseline"
  else
    echo "FAIL  CPU pressure (PSI waiting) of ${pod}: ${psi_f:-none} s over ${dur} s vs ${psi_b:-none} s over ${base_dur} s of baseline"
    rc=1
  fi
  throttled="$(prom_scalar "increase(container_cpu_cfs_throttled_periods_total{${sel}}[${dur}s])" "${now}")"
  periods="$(prom_scalar "increase(container_cpu_cfs_periods_total{${sel}}[${dur}s])" "${now}")"
  fraction="$(awk -v t="${throttled:-0}" -v p="${periods:-0}" 'BEGIN {printf "%.3f", (p > 0 ? t / p : 0)}')"
  if [[ -n "${periods}" ]] && awk -v f="${fraction}" -v m="${MAX_THROTTLED_FRACTION}" 'BEGIN {exit !(f < m)}'; then
    echo "PASS  no CFS throttling: fraction ${fraction} < ${MAX_THROTTLED_FRACTION}"
  else
    echo "FAIL  CFS throttled fraction ${fraction} (periods '${periods}'): this would be cpu-throttle"
    rc=1
  fi
  base="$(loadgen_window "${base_start}" "${start}" | jq -r '."short-16".p50_ms // empty')"
  slow="$(loadgen_window "${done_t}" "$(now_ms)" | jq -r '."short-16".p50_ms // empty')"
  if [[ -n "${base}" && -n "${slow}" ]]; then
    ratio="$(awk -v a="${slow}" -v b="${base}" 'BEGIN {printf "%.2f", a / b}')"
    if awk -v r="${ratio}" -v m="${MIN_LATENCY_RATIO}" 'BEGIN {exit !(r >= m)}'; then
      echo "PASS  short-16 p50 ${slow} ms vs baseline ${base} ms (x${ratio} >= x${MIN_LATENCY_RATIO})"
    else
      echo "FAIL  short-16 p50 ${slow} ms vs baseline ${base} ms (x${ratio} < x${MIN_LATENCY_RATIO})"
      rc=1
    fi
  else
    echo "FAIL  not enough successful short-16 requests (baseline '${base}', fault '${slow}')"
    rc=1
  fi
  local marker
  marker="$(run_get "${dir}" '.fault.applied.marker // empty')"
  if [[ -n "${marker}" ]]; then
    local running
    running="$(pgrep -fc -- "^sh -c : ${marker}" || true)"
    if [[ "${running:-0}" -eq "$(run_get "${dir}" '.fault.applied.busy_loops')" ]]; then
      echo "PASS  ${running} host busy loops running (${marker})"
    else
      echo "FAIL  ${running:-0} host busy loops running, expected $(run_get "${dir}" '.fault.applied.busy_loops')"
      rc=1
    fi
  fi
  if [[ "$(run_get "${dir}" '.fault.variant // empty')" == neighbor-pod-same-node ]]; then
    local node nnode usage
    node="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.spec.nodeName}')"
    nnode="$(kc -n "${NOISE_NS}" get pod "${NEIGHBOR}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
    usage="$(prom_scalar "rate(container_cpu_usage_seconds_total{namespace=\"${NOISE_NS}\",pod=\"${NEIGHBOR}\",container=\"burn\"}[2m])" "${now}")"
    if [[ "${nnode}" == "${node}" ]] && awk -v u="${usage:-0}" 'BEGIN {exit !(u >= 1)}'; then
      echo "PASS  neighbor ${NOISE_NS}/${NEIGHBOR} on the model-server node ${node}, using ${usage} CPU"
    else
      echo "FAIL  neighbor on node '${nnode}' (model-server on '${node}'), CPU usage '${usage}'"
      rc=1
    fi
  fi
  return "${rc}"
}

case "${1:-}" in
  inject) inject "$2" "$3" ;;
  reset) reset "$2" ;;
  check) check "$2" ;;
  *) die "usage: $0 inject RUN_DIR VARIANT | reset RUN_DIR | check RUN_DIR" ;;
esac
