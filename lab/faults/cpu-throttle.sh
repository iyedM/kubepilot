#!/usr/bin/env bash
# cpu-throttle fault on the model server.
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           undo it (the caller then re-applies the manifests)
#   check  RUN_DIR           exit 0 only if the throttling signal is really there
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

# Thresholds of the check (documented in docs/lab.md).
MIN_THROTTLED_FRACTION="0.20"
MIN_LATENCY_RATIO="2.0"

resize_cpu() { # resize_cpu POD CPU
  kc -n "${GENAI_NAMESPACE}" patch pod "$1" --subresource resize --type strategic \
    -p "{\"spec\":{\"containers\":[{\"name\":\"model-server\",\"resources\":{\"requests\":{\"cpu\":\"$2\"},\"limits\":{\"cpu\":\"$2\"}}}]}}" >/dev/null
  local deadline=$(( $(date +%s) + 60 ))
  until [[ "$(kc -n "${GENAI_NAMESPACE}" get pod "$1" -o jsonpath='{.status.containerStatuses[0].resources.limits.cpu}')" == "$2" ]]; do
    (( $(date +%s) < deadline )) || die "resize of $1 to $2 not reported in pod status after 60s"
    sleep 2
  done
}

inject() {
  local dir="$1" variant="$2" cpu pod
  cpu="$(run_get "${dir}" '.fault.params.cpu')"
  case "${variant}" in
    limit-*)
      pod="$(running_pod model-server)"
      [[ -n "${pod}" ]] || die "no running model-server pod"
      resize_cpu "${pod}" "${cpu}"
      run_set "${dir}" '.fault.applied = {mode: "in-place", pod: $p, cpu: $c}' --arg p "${pod}" --arg c "${cpu}"
      ;;
    rollout-limit-*)
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type strategic \
        -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"model-server\",\"resources\":{\"requests\":{\"cpu\":\"${cpu}\"},\"limits\":{\"cpu\":\"${cpu}\"}}}]}}}}" >/dev/null
      kc -n "${GENAI_NAMESPACE}" rollout status deploy/model-server --timeout=3m >/dev/null
      run_set "${dir}" '.fault.applied = {mode: "rollout", pod: $p, cpu: $c}' --arg p "$(running_pod model-server)" --arg c "${cpu}"
      ;;
    *) die "unknown cpu-throttle variant ${variant}" ;;
  esac
}

reset() {
  local dir="$1" pod mode
  mode="$(run_get "${dir}" '.fault.applied.mode // empty')"
  pod="$(run_get "${dir}" '.fault.applied.pod // empty')"
  # In-place resize does not touch the Deployment: re-applying the manifest
  # would not undo it, so resize the same pod back.
  if [[ "${mode}" == in-place && -n "${pod}" ]] && kc -n "${GENAI_NAMESPACE}" get pod "${pod}" >/dev/null 2>&1; then
    resize_cpu "${pod}" 2
  fi
}

check() {
  local dir="$1" pod inject_done now dur throttled periods fraction base_start inject_start base fault ratio rc=0
  inject_done="$(run_get "${dir}" '.timeline.inject_done')"
  inject_start="$(run_get "${dir}" '.timeline.inject_start')"
  base_start="$(run_get "${dir}" '.timeline.baseline_start')"
  # The pod running now, after the injection (a rollout variant replaces it).
  pod="$(running_pod model-server)"
  if [[ -z "${pod}" ]]; then
    echo "FAIL  no running model-server pod"
    return 1
  fi
  now="$(date +%s)"
  dur=$(( now - $(to_epoch "${inject_done}") ))
  if (( dur < 60 )); then
    echo "FAIL  fault window too short (${dur}s) to measure throttling"
    return 1
  fi
  local sel="namespace=\"genai\",container=\"model-server\",pod=\"${pod}\""
  throttled="$(prom_scalar "increase(container_cpu_cfs_throttled_periods_total{${sel}}[${dur}s])" "${now}")"
  periods="$(prom_scalar "increase(container_cpu_cfs_periods_total{${sel}}[${dur}s])" "${now}")"
  fraction="$(awk -v t="${throttled:-0}" -v p="${periods:-0}" 'BEGIN {printf "%.3f", (p > 0 ? t / p : 0)}')"
  if awk -v f="${fraction}" -v m="${MIN_THROTTLED_FRACTION}" 'BEGIN {exit !(f >= m)}'; then
    echo "PASS  throttled fraction ${fraction} >= ${MIN_THROTTLED_FRACTION} for ${pod} over the fault window (${dur}s, ${throttled:-0}/${periods:-0} periods)"
  else
    echo "FAIL  throttled fraction ${fraction} < ${MIN_THROTTLED_FRACTION} for ${pod} over the fault window (${dur}s, ${throttled:-0}/${periods:-0} periods)"
    rc=1
  fi

  # Latency: p50 of short-16 in the fault window vs the baseline of this run.
  base="$(loadgen_window "${base_start}" "${inject_start}" | jq -r '."short-16".p50_ms // empty')"
  fault="$(loadgen_window "${inject_done}" "$(now_ms)" | jq -r '."short-16".p50_ms // empty')"
  if [[ -n "${base}" && -n "${fault}" ]]; then
    ratio="$(awk -v a="${fault}" -v b="${base}" 'BEGIN {printf "%.2f", a / b}')"
    if awk -v r="${ratio}" -v m="${MIN_LATENCY_RATIO}" 'BEGIN {exit !(r >= m)}'; then
      echo "PASS  short-16 p50 ${fault} ms vs baseline ${base} ms (x${ratio} >= x${MIN_LATENCY_RATIO})"
    else
      echo "FAIL  short-16 p50 ${fault} ms vs baseline ${base} ms (x${ratio} < x${MIN_LATENCY_RATIO})"
      rc=1
    fi
  else
    echo "FAIL  not enough successful short-16 requests (baseline p50='${base}', fault p50='${fault}')"
    rc=1
  fi

  # Mechanism evidence, depending on how the limit was applied.
  case "$(run_get "${dir}" '.fault.applied.mode // empty')" in
    in-place)
      local before_cid now_cid resized
      before_cid="$(run_get "${dir}" '.target.pods_before[0].container_id')"
      now_cid="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[0].containerID}')"
      resized="$(events_since "${GENAI_NAMESPACE}" "${pod}" "${inject_start}" | jq '[.[] | select(.reason == "ResizeCompleted")] | length')"
      if [[ "${now_cid}" == "${before_cid}" && "${resized}" -ge 1 ]]; then
        echo "PASS  in-place: same container (${now_cid:13:12}), ResizeCompleted event present"
      else
        echo "FAIL  in-place: container ${now_cid:13:12} vs before ${before_cid:13:12}, ResizeCompleted events: ${resized}"
        rc=1
      fi
      ;;
    rollout)
      local created limit
      created="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.metadata.creationTimestamp}')"
      limit="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o jsonpath='{.spec.containers[0].resources.limits.cpu}')"
      if [[ "${created}" > "${inject_start:0:19}" && "${limit}" == "$(run_get "${dir}" '.fault.params.cpu')" ]]; then
        echo "PASS  rollout: pod ${pod} created after injection with CPU limit ${limit}"
      else
        echo "FAIL  rollout: pod ${pod} created ${created}, CPU limit ${limit}"
        rc=1
      fi
      ;;
    *)
      echo "FAIL  no cpu-throttle injection recorded in this run"
      rc=1
      ;;
  esac
  return "${rc}"
}

case "${1:-}" in
  inject) inject "$2" "$3" ;;
  reset) reset "$2" ;;
  check) check "$2" ;;
  *) die "usage: $0 inject RUN_DIR VARIANT | reset RUN_DIR | check RUN_DIR" ;;
esac
