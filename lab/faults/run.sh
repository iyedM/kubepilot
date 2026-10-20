#!/usr/bin/env bash
# run.sh SPLIT FAULT|nofault VARIANT REP: one complete, recorded run.
#
#   precheck -> baseline (BASELINE_S) -> inject -> fault (FAULT_S, check at
#   the end) -> reset -> healthy again -> recovery (RECOVERY_S) -> export
#
# The run id is deterministic, so a run already marked "complete" is skipped
# (resume after an interruption). Exit codes: 0 complete or skipped,
# 3 invalid (precondition or signal check failed), 4 interrupted.
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

# Quiet-period precondition: nothing changed in genai for QUIET_S before the
# baseline; wait up to QUIET_MAX_WAIT_S for it (the previous run's reset
# creates a pod only RECOVERY_S before), then the run is invalid.
QUIET_S="${QUIET_S:-300}"
QUIET_MAX_WAIT_S="${QUIET_MAX_WAIT_S:-420}"
BASELINE_S="${BASELINE_S:-180}"
FAULT_S="${FAULT_S:-300}"
RECOVERY_S="${RECOVERY_S:-180}"

[[ $# -eq 4 ]] || die "usage: $0 SPLIT FAULT|nofault VARIANT REP"
split="$1" fault="$2" variant="$3" rep="$4"
[[ "${split}" =~ ^(dev|test|dryrun)$ ]] || die "split must be dev, test or dryrun"
[[ "${rep}" =~ ^[0-9]+$ ]] || die "REP must be a number"
kind=fault
[[ "${fault}" == nofault ]] && kind=nofault
[[ "${kind}" == nofault || -x "${FAULTS_DIR}/${fault}.sh" ]] || die "unknown fault ${fault}"

if [[ "${split}" == test ]]; then
  # eval/README.md: no test run before a committed pre-registration.
  git -C "${ROOT_DIR}" ls-files --error-unmatch 'eval/preregistration/*.json' >/dev/null 2>&1 \
    || die "no committed pre-registration in eval/preregistration/: test runs are not allowed yet"
  # The run must be attributable to an exact commit.
  [[ -z "$(git -C "${ROOT_DIR}" status --porcelain --untracked-files=no)" ]] \
    || die "uncommitted changes in tracked files: test runs require a clean tree"
fi

run_id="${split}--${fault}--${variant}--r${rep}"
dir="${RUNS_DIR}/${split}/${run_id}"
if [[ -f "${dir}/run.json" && "$(run_get "${dir}" .status)" == complete ]]; then
  log "skip ${run_id}: already complete"
  exit 0
fi

mkdir -p "${TOOLS_DIR}"
exec 9>"${TOOLS_DIR}/fault.lock"
flock -n 9 || die "another fault run holds ${TOOLS_DIR}/fault.lock"
[[ -s "${ACTIVE_RUN}" ]] && die "a fault is active ($(cat "${ACTIVE_RUN}")): run 'make fault-reset' first"

if [[ -d "${dir}" ]]; then
  prev="${dir}.prev-$(date -u +%Y%m%dT%H%M%SZ)"
  mv "${dir}" "${prev}"
  log "previous incomplete attempt kept as ${prev#"${ROOT_DIR}/"}"
fi

log "run ${run_id}"
init_run "${dir}" "${run_id}" "${split}" "${kind}" "${fault}" "${variant}"
mark "${dir}" run_start
echo "${dir}" > "${ACTIVE_RUN}"

injected=false
on_interrupt() {
  log "interrupted: resetting the lab"
  set_status "${dir}" interrupted
  if [[ "${injected}" == true ]]; then
    "${FAULTS_DIR}/${fault}.sh" reset "${dir}" || true
    reapply_declared || true
  fi
  : > "${ACTIVE_RUN}"
  shelve_run "${dir}"
  exit 4
}
trap on_interrupt INT TERM

finish_invalid() {
  note "${dir}" "$1"
  set_status "${dir}" invalid
  : > "${ACTIVE_RUN}"
  log "run ${run_id} INVALID: $1"
  shelve_run "${dir}"
  exit 3
}

# --- precondition: healthy lab with traffic -------------------------------------
replicas="$(kc -n "${GENAI_NAMESPACE}" get deploy loadgen -o jsonpath='{.spec.replicas}')"
if [[ "${replicas:-0}" -lt 1 ]]; then
  finish_invalid "precondition: loadgen has ${replicas:-0} replicas (traffic is required)"
fi
deadline=$(( $(date +%s) + QUIET_MAX_WAIT_S ))
while :; do
  changes="$(genai_changes_since "${QUIET_S}")"
  [[ "$(jq length <<<"${changes}")" -eq 0 ]] && break
  if (( $(date +%s) >= deadline )); then
    finish_invalid "precondition: genai not quiet for ${QUIET_S}s after waiting ${QUIET_MAX_WAIT_S}s; last change: $(jq -r 'last | "\(.time) \(.what)"' <<<"${changes}") ($(jq length <<<"${changes}") changes in the last ${QUIET_S}s)"
  fi
  log "waiting for a quiet genai namespace (last change: $(jq -r 'last | "\(.time) \(.what)"' <<<"${changes}"))"
  sleep 15
done
run_set "${dir}" '.timeline.quiet_since = $t' --arg t "$(date -u -d "-${QUIET_S} seconds" +%Y-%m-%dT%H:%M:%SZ)"
if ! "${LAB_DIR}/scripts/check-genai.sh" > "${dir}/precheck.txt" 2>&1; then
  finish_invalid "precondition: check-genai failed (see precheck.txt)"
fi

# --- baseline ----------------------------------------------------------------------
mark "${dir}" baseline_start
phase_begin "${dir}" baseline
log "baseline ${BASELINE_S}s"
sleep "${BASELINE_S}"
phase_end "${dir}" baseline

# --- fault -------------------------------------------------------------------------
mark "${dir}" inject_start
phase_begin "${dir}" fault
if [[ "${kind}" == fault ]]; then
  injected=true
  log "inject ${fault}/${variant}"
  "${FAULTS_DIR}/${fault}.sh" inject "${dir}" "${variant}"
fi
mark "${dir}" inject_done
# The cause is active from the moment the change is applied (inject_start):
# with Recreate the outage starts right away, while inject_done may come
# later (oomkill waits until the OOM kill is observed).
if [[ "${kind}" == fault ]]; then
  run_set "${dir}" '.expected_causes = [{cause: .fault.cause,
    component: {namespace: "genai", kind: "Pod", workload: ("deployment/" + (.fault.applied.app // "model-server"))},
    active_from: .timeline.inject_start, active_to: null}]'
fi
log "fault window ${FAULT_S}s"
sleep "${FAULT_S}"
if [[ "${kind}" == fault ]]; then
  if "${FAULTS_DIR}/${fault}.sh" check "${dir}" > "${dir}/check.txt" 2>&1; then
    run_set "${dir}" '.check = {script: $s, result: "pass"}' --arg s "lab/faults/check-fault.sh ${fault}"
  else
    run_set "${dir}" '.check = {script: $s, result: "fail"}' --arg s "lab/faults/check-fault.sh ${fault}"
  fi
  cat "${dir}/check.txt"
fi
phase_end "${dir}" fault

# --- reset -------------------------------------------------------------------------
mark "${dir}" reset_start
if [[ "${kind}" == fault ]]; then
  log "reset ${fault}"
  "${FAULTS_DIR}/${fault}.sh" reset "${dir}"
fi
reapply_declared
mark "${dir}" reset_done
# A fault whose effect ends by itself (cold start: the pod becomes Ready)
# records fault.applied.effect_end; otherwise the cause lasts until reset.
run_set "${dir}" '(.expected_causes[]?.active_to) = (.fault.applied.effect_end // .timeline.reset_done)'
injected=false
if wait_chat_ok 300; then
  mark "${dir}" lab_healthy_again
else
  note "${dir}" "chat-api did not answer 200 within 300s after the reset"
fi

# --- recovery ----------------------------------------------------------------------
phase_begin "${dir}" recovery
log "recovery ${RECOVERY_S}s"
sleep "${RECOVERY_S}"
phase_end "${dir}" recovery
mark "${dir}" end

run_set "${dir}" '.target.pods_after = $p | .host.loadavg_end = $l' \
  --argjson p "$(pods_json model-server)" --argjson l "$(loadavg_json)"
log "export"
export_run "${dir}"
mark "${dir}" export_done

if [[ "${kind}" == fault && "$(run_get "${dir}" '.check.result')" != pass ]]; then
  finish_invalid "the per-fault check did not see the signal (see check.txt)"
fi
set_status "${dir}" complete
: > "${ACTIVE_RUN}"
log "run ${run_id} complete: ${dir#"${ROOT_DIR}/"}"
