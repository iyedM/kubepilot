#!/usr/bin/env bash
# check-fault.sh FAULT [RUN_DIR]: is the signal of FAULT really present?
#
# RUN_DIR defaults to the active run (.tools/active-run). Without an active
# run, a synthetic context is used (baseline = 10 to 5 minutes ago, "fault"
# = the last 5 minutes, no injection recorded): this is how the negative test
# "check without any fault" is run, and it must fail.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

fault="${1:-}"
[[ -x "${FAULTS_DIR}/${fault}.sh" ]] || die "usage: $0 FAULT [RUN_DIR] (unknown fault '${fault}')"
dir="${2:-}"
if [[ -z "${dir}" && -s "${ACTIVE_RUN}" ]]; then
  dir="$(cat "${ACTIVE_RUN}")"
fi
if [[ -z "${dir}" ]]; then
  dir="$(mktemp -d "${TOOLS_DIR}/check-context.XXXXXX")"
  jq -n --arg b "$(date -u -d '-600 seconds' +%Y-%m-%dT%H:%M:%S.000Z)" \
    --arg i "$(date -u -d '-300 seconds' +%Y-%m-%dT%H:%M:%S.000Z)" \
    --argjson before "$(pods_json model-server)" \
    '{fault: null, timeline: {baseline_start: $b, inject_start: $i, inject_done: $i},
      target: {pods_before: $before}}' > "${dir}/run.json"
  echo "INFO  no active run: synthetic context (baseline -10..-5 min, window -5 min..now, no injection)"
else
  echo "INFO  run context: ${dir#"${ROOT_DIR}/"} (fault: $(run_get "${dir}" '.fault.name // "none"'))"
fi
"${FAULTS_DIR}/${fault}.sh" check "${dir}"
