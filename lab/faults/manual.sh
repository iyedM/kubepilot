#!/usr/bin/env bash
# Manual fault injection (split "manual", never scored).
#   manual.sh inject FAULT VARIANT   inject now; baseline = the 3 previous minutes
#   manual.sh reset                  undo the active fault, export, close the run
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

inject() {
  local fault="$1" variant="$2" run_id dir
  [[ -x "${FAULTS_DIR}/${fault}.sh" ]] || die "unknown fault ${fault}"
  [[ -s "${ACTIVE_RUN}" ]] && die "a fault is already active ($(cat "${ACTIVE_RUN}")): run 'make fault-reset' first"
  run_id="manual--${fault}--${variant}--$(date -u +%Y%m%dT%H%M%SZ)"
  dir="${RUNS_DIR}/manual/${run_id}"
  init_run "${dir}" "${run_id}" manual fault "${fault}" "${variant}"
  echo "${dir}" > "${ACTIVE_RUN}"
  # No waiting phase: the 3 minutes before the injection serve as baseline.
  run_set "${dir}" '.timeline.baseline_start = $t' --arg t "$(date -u -d '-180 seconds' +%Y-%m-%dT%H:%M:%S.000Z)"
  mark "${dir}" inject_start
  if ! "${FAULTS_DIR}/${fault}.sh" inject "${dir}" "${variant}"; then
    "${FAULTS_DIR}/${fault}.sh" reset "${dir}" || true
    reapply_declared || true
    note "${dir}" "injection failed"
    set_status "${dir}" invalid
    : > "${ACTIVE_RUN}"
    die "injection of ${fault}/${variant} failed; the lab was reset"
  fi
  mark "${dir}" inject_done
  run_set "${dir}" '.expected_causes = [{cause: .fault.cause,
    component: {namespace: "genai", kind: "Pod", workload: ("deployment/" + (.fault.applied.app // "model-server"))},
    active_from: .timeline.inject_start, active_to: null}]'
  log "injected ${fault}/${variant}; record: ${dir#"${ROOT_DIR}/"}; undo with 'make fault-reset'"
}

reset() {
  local dir fault
  [[ -s "${ACTIVE_RUN}" ]] || { log "no active fault"; return 0; }
  dir="$(cat "${ACTIVE_RUN}")"
  fault="$(run_get "${dir}" '.fault.name')"
  mark "${dir}" reset_start
  "${FAULTS_DIR}/${fault}.sh" reset "${dir}"
  reapply_declared
  mark "${dir}" reset_done
  run_set "${dir}" '(.expected_causes[]?.active_to) = .timeline.reset_done'
  if wait_chat_ok 300; then mark "${dir}" lab_healthy_again; else note "${dir}" "chat-api not healthy 300s after reset"; fi
  mark "${dir}" end
  run_set "${dir}" '.target.pods_after = $p | .host.loadavg_end = $l' \
    --argjson p "$(pods_json model-server)" --argjson l "$(loadavg_json)"
  export_run "${dir}"
  set_status "${dir}" complete
  : > "${ACTIVE_RUN}"
  log "reset done; record: ${dir#"${ROOT_DIR}/"}"
}

case "${1:-}" in
  inject) inject "${2:-}" "${3:-}" ;;
  reset) reset ;;
  *) die "usage: $0 inject FAULT VARIANT | reset" ;;
esac
