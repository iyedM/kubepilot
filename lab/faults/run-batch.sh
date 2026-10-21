#!/usr/bin/env bash
# run-batch.sh SPLIT: run or resume every catalog entry of a split (dev or test).
#
#   1. Holds a single systemd-inhibit lock for the whole batch when INHIBIT_SLEEP=1.
#   2. Runs all non-hook fault variants, then no-fault windows, then app-delay
#      variants (toggling LAB_FAULT_HOOKS once around app-delay only if needed).
#   3. Skips any run whose run.json already has status=="complete".
#   4. Prints a final summary table and exits non-zero if any run is not complete.
#
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

[[ $# -eq 1 ]] || die "usage: $0 SPLIT (dev or test)"
split="$1"
[[ "${split}" =~ ^(dev|test)$ ]] || die "SPLIT must be dev or test"

if [[ "${split}" == test ]]; then
  # eval/README.md: no test run before a committed pre-registration.
  git -C "${ROOT_DIR}" ls-files --error-unmatch 'eval/preregistration/*.json' >/dev/null 2>&1 \
    || die "no committed pre-registration in eval/preregistration/: test runs are not allowed yet"
  [[ -z "$(git -C "${ROOT_DIR}" status --porcelain --untracked-files=no)" ]] \
    || die "uncommitted changes in tracked files: test runs require a clean tree"
fi

if [[ "${INHIBIT_SLEEP:-0}" == 1 && "${_KUBEPILOT_BATCH_INHIBITED:-0}" != 1 ]]; then
  exec systemd-inhibit --what=sleep:idle:handle-lid-switch --who=kubepilot \
    --why="KubePilot ${split} fault batch" --mode=block \
    env _KUBEPILOT_BATCH_INHIBITED=1 "$0" "$@"
fi

fault_reps="$(jq -r --arg s "${split}" '.repetitions[$s]' "${CATALOG}")"
nofault_reps="$(jq -r --arg s "${split}" '.repetitions["nofault_" + $s]' "${CATALOG}")"

# Build the ordered schedule of "fault variant rep" entries:
#   1) faults other than app-delay
#   2) no-fault windows
#   3) app-delay faults (grouped at the end so hooks are toggled at most once)
schedule=()
while IFS=$'\t' read -r fault variant; do
  [[ -n "${fault}" ]] || continue
  for rep in $(seq 1 "${fault_reps}"); do
    schedule+=("${fault}"$'\t'"${variant}"$'\t'"${rep}")
  done
done < <(jq -r --arg s "${split}" '
  .faults[] | select(.name != "app-delay") as $f
  | $f.variants[] | select(.split == $s)
  | "\($f.name)\t\(.id)"' "${CATALOG}")

while IFS= read -r variant; do
  [[ -n "${variant}" ]] || continue
  for rep in $(seq 1 "${nofault_reps}"); do
    schedule+=("nofault"$'\t'"${variant}"$'\t'"${rep}")
  done
done < <(jq -r --arg s "${split}" '.nofault.variants[] | select(.split == $s) | .id' "${CATALOG}")

app_delay_entries=()
while IFS=$'\t' read -r fault variant; do
  [[ -n "${fault}" ]] || continue
  for rep in $(seq 1 "${fault_reps}"); do
    entry="${fault}"$'\t'"${variant}"$'\t'"${rep}"
    schedule+=("${entry}")
    app_delay_entries+=("${entry}")
  done
done < <(jq -r --arg s "${split}" '
  .faults[] | select(.name == "app-delay") as $f
  | $f.variants[] | select(.split == $s)
  | "\($f.name)\t\(.id)"' "${CATALOG}")

hooks_switched_on=false
cleanup_hooks() {
  if [[ "${hooks_switched_on}" == true ]]; then
    log "disabling chat-api lab fault hooks after app-delay section"
    "${LAB_DIR}/scripts/lab-hooks.sh" off || true
    hooks_switched_on=false
  fi
}
trap cleanup_hooks EXIT

is_complete() {
  local dir="${RUNS_DIR}/${split}/$1"
  [[ -f "${dir}/run.json" && "$(run_get "${dir}" .status)" == complete ]]
}

# Does any app-delay run still need to be executed?
need_app_delay_hooks=false
for entry in "${app_delay_entries[@]}"; do
  IFS=$'\t' read -r fault variant rep <<<"${entry}"
  run_id="${split}--${fault}--${variant}--r${rep}"
  if ! is_complete "${run_id}"; then
    need_app_delay_hooks=true
    break
  fi
done

n_total="${#schedule[@]}"
n_complete=0
n_ran=0
n_skipped=0
n_invalid=0
n_interrupted=0
declare -A run_action=()

log "starting ${split} batch (${n_total} runs)"

in_app_delay=false
for entry in "${schedule[@]}"; do
  IFS=$'\t' read -r fault variant rep <<<"${entry}"
  run_id="${split}--${fault}--${variant}--r${rep}"

  if [[ "${fault}" == app-delay && "${in_app_delay}" == false ]]; then
    in_app_delay=true
    if [[ "${need_app_delay_hooks}" == true && "$(chat_api_hooks)" == 0 ]]; then
      log "enabling chat-api lab fault hooks for app-delay section"
      "${LAB_DIR}/scripts/lab-hooks.sh" on
      hooks_switched_on=true
    fi
  fi

  if is_complete "${run_id}"; then
    log "skip ${run_id}: already complete"
    run_action["${run_id}"]="skipped"
    n_skipped=$(( n_skipped + 1 ))
    n_complete=$(( n_complete + 1 ))
    continue
  fi

  rc=0
  "${FAULTS_DIR}/run.sh" "${split}" "${fault}" "${variant}" "${rep}" || rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    run_action["${run_id}"]="ran"
    n_ran=$(( n_ran + 1 ))
    n_complete=$(( n_complete + 1 ))
  elif [[ "${rc}" -eq 3 ]]; then
    run_action["${run_id}"]="invalid"
    n_invalid=$(( n_invalid + 1 ))
  else
    run_action["${run_id}"]="interrupted"
    n_interrupted=$(( n_interrupted + 1 ))
    log "batch stopped after interrupted run ${run_id} (exit ${rc})"
    break
  fi
done

cleanup_hooks

echo
printf '%-46s %-11s %-10s %-6s %-13s %s\n' "RUN_ID" "ACTION" "STATUS" "CHECK" "PHASES(s)" "FAULT_CPU%"
printf '%-46s %-11s %-10s %-6s %-13s %s\n' "----------------------------------------------" "-----------" "----------" "------" "-------------" "----------"
for entry in "${schedule[@]}"; do
  IFS=$'\t' read -r fault variant rep <<<"${entry}"
  run_id="${split}--${fault}--${variant}--r${rep}"
  dir="${RUNS_DIR}/${split}/${run_id}"
  action="${run_action[${run_id}]:-pending}"
  if [[ -f "${dir}/run.json" ]]; then
    status="$(run_get "${dir}" '.status // "-"')"
    check_res="$(run_get "${dir}" '.check.result // "-"')"
    phases="$(run_get "${dir}" 'if .phase_seconds then "\(.phase_seconds.baseline.real // "-")/\(.phase_seconds.fault.real // "-")/\(.phase_seconds.recovery.real // "-")" else "-" end')"
    cpu="$(run_get "${dir}" 'if .host.cpu_busy_pct.fault != null then "\(.host.cpu_busy_pct.fault)%" else "-" end')"
  else
    status="-"
    check_res="-"
    phases="-"
    cpu="-"
  fi
  printf '%-46s %-11s %-10s %-6s %-13s %s\n' "${run_id}" "${action}" "${status}" "${check_res}" "${phases}" "${cpu}"
done
echo
log "batch ${split} summary: ${n_complete}/${n_total} complete (${n_ran} ran, ${n_skipped} skipped), ${n_invalid} invalid, ${n_interrupted} interrupted"

[[ "${n_complete}" -eq "${n_total}" && "${n_invalid}" -eq 0 && "${n_interrupted}" -eq 0 ]]
