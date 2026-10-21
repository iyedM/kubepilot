#!/usr/bin/env bash
# config-regression fault on the model server: a rollout changes the
# configuration (fewer inference threads) and serving gets slower, without
# any resource-pressure signal.
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           nothing specific: the Deployment reset covers it
#   check  RUN_DIR           exit 0 only if the regression is really there
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

MAX_THROTTLED_FRACTION="0.05"  # no CFS throttling: this is not cpu-throttle
MIN_LATENCY_RATIO="1.3"        # measured x1.66 for short-16; quiet spread <= x1.05

newest_pod() {
  kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server -o json \
    | jq -r --arg t "${1:0:19}" '[.items[] | select(.metadata.creationTimestamp >= $t)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty'
}

ready_at() {
  kc -n "${GENAI_NAMESPACE}" get pod "$1" -o json \
    | jq -r '[.status.conditions[]? | select(.type == "Ready" and .status == "True")] | first | .lastTransitionTime // empty'
}

inject() {
  local dir="$1" variant="$2" threads
  case "${variant}" in
    threads-*)
      threads="$(run_get "${dir}" '.fault.params.threads')"
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type json -p "[
        {\"op\":\"test\",\"path\":\"/spec/template/spec/containers/0/args/9\",\"value\":\"--threads\"},
        {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/10\",\"value\":\"${threads}\"}]" >/dev/null
      kc -n "${GENAI_NAMESPACE}" rollout status deploy/model-server --timeout=3m >/dev/null
      run_set "${dir}" '.fault.applied = {variant: $v, threads: $t, pod: $p}' \
        --arg v "${variant}" --arg t "${threads}" --arg p "$(running_pod model-server)"
      ;;
    *) die "unknown config-regression variant ${variant}" ;;
  esac
}

reset() { :; }

check() {
  local dir="$1" start base_start pod ready threads rollout now dur sel throttled periods fraction base slow ratio rc=0
  start="$(run_get "${dir}" '.timeline.inject_start')"
  base_start="$(run_get "${dir}" '.timeline.baseline_start')"
  pod="$(newest_pod "${start}")"
  if [[ -z "${pod}" ]]; then
    echo "FAIL  no model-server pod created after the injection (${start})"
    return 1
  fi
  ready="$(ready_at "${pod}")"
  threads="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json | jq -r '.spec.containers[0].args as $a | ($a | index("--threads")) as $i | if $i then $a[$i + 1] else empty end')"
  if [[ -n "${ready}" && "${threads}" == "$(run_get "${dir}" '.fault.params.threads')" ]]; then
    echo "PASS  pod ${pod} Ready at ${ready} with --threads ${threads} in its spec"
  else
    echo "FAIL  pod ${pod}: Ready='${ready}', --threads '${threads}'"
    rc=1
  fi
  rollout="$(events_since "${GENAI_NAMESPACE}" model-server "${start}" | jq '[.[] | select(.reason == "ScalingReplicaSet")] | length')"
  if [[ "${rollout}" -ge 1 ]]; then
    echo "PASS  rollout event (ScalingReplicaSet on deployment/model-server) since the injection"
  else
    echo "FAIL  no rollout event on deployment/model-server since ${start}"
    rc=1
  fi
  if [[ -n "${ready}" ]]; then
    now="$(date +%s)"
    dur=$(( now - $(to_epoch "${ready}") ))
    sel="namespace=\"genai\",container=\"model-server\",pod=\"${pod}\""
    throttled="$(prom_scalar "increase(container_cpu_cfs_throttled_periods_total{${sel}}[${dur}s])" "${now}")"
    periods="$(prom_scalar "increase(container_cpu_cfs_periods_total{${sel}}[${dur}s])" "${now}")"
    fraction="$(awk -v t="${throttled:-0}" -v p="${periods:-0}" 'BEGIN {printf "%.3f", (p > 0 ? t / p : 0)}')"
    if [[ -n "${periods}" ]] && awk -v f="${fraction}" -v m="${MAX_THROTTLED_FRACTION}" 'BEGIN {exit !(f < m)}'; then
      echo "PASS  no CFS throttling: fraction ${fraction} < ${MAX_THROTTLED_FRACTION} since Ready (${throttled:-0}/${periods} periods)"
    else
      echo "FAIL  CFS throttled fraction ${fraction} (periods '${periods}'): not a configuration-only regression"
      rc=1
    fi
    base="$(loadgen_window "${base_start}" "${start}" | jq -r '."short-16".p50_ms // empty')"
    slow="$(loadgen_window "${ready}" "$(now_ms)" | jq -r '."short-16".p50_ms // empty')"
    if [[ -n "${base}" && -n "${slow}" ]]; then
      ratio="$(awk -v a="${slow}" -v b="${base}" 'BEGIN {printf "%.2f", a / b}')"
      if awk -v r="${ratio}" -v m="${MIN_LATENCY_RATIO}" 'BEGIN {exit !(r >= m)}'; then
        echo "PASS  short-16 p50 ${slow} ms after Ready vs baseline ${base} ms (x${ratio} >= x${MIN_LATENCY_RATIO})"
      else
        echo "FAIL  short-16 p50 ${slow} ms after Ready vs baseline ${base} ms (x${ratio} < x${MIN_LATENCY_RATIO})"
        rc=1
      fi
    else
      echo "FAIL  not enough successful short-16 requests (baseline '${base}', after Ready '${slow}')"
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
