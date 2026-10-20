#!/usr/bin/env bash
# baseline-stability.sh [WINDOWS] [WINDOW_S]: how much does latency vary in
# quiet windows (load generator only, no fault, no check running)?
#
# Runs WINDOWS consecutive windows of WINDOW_S seconds (default 5 x 300 s, the
# length of a fault window) and reports, per prompt, the p50 of each window
# and its spread. Output: eval/baseline/stability-<UTC>.json
# jq programs are passed in single quotes on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

WINDOWS="${1:-5}"
WINDOW_S="${2:-300}"
out_dir="${ROOT_DIR}/eval/baseline"
mkdir -p "${out_dir}" "${TOOLS_DIR}"
out="${out_dir}/stability-$(date -u +%Y%m%dT%H%M%SZ).json"

exec 9>"${TOOLS_DIR}/fault.lock"
flock -n 9 || die "a fault run holds ${TOOLS_DIR}/fault.lock"
[[ -s "${ACTIVE_RUN}" ]] && die "a fault is active: run 'make fault-reset' first"
[[ "$(kc -n "${GENAI_NAMESPACE}" get deploy loadgen -o jsonpath='{.spec.replicas}')" -ge 1 ]] || die "loadgen is not running"

windows="[]"
for i in $(seq 1 "${WINDOWS}"); do
  start="$(now_ms)"
  read -r b0 t0 < <(cpu_jiffies)
  log "window ${i}/${WINDOWS} (${WINDOW_S}s)"
  sleep "${WINDOW_S}"
  end="$(now_ms)"
  read -r b1 t1 < <(cpu_jiffies)
  busy="$(awk -v a="${b0}" -v b="${t0}" -v c="${b1}" -v d="${t1}" 'BEGIN {printf "%.1f", 100*(c-a)/(d-b)}')"
  stats="$(loadgen_window "${start}" "${end}")"
  windows="$(jq -c --arg s "${start}" --arg e "${end}" --argjson st "${stats}" --argjson busy "${busy}" \
    '. + [{start: $s, end: $e, host_cpu_busy_pct: $busy, prompts: $st}]' <<<"${windows}")"
done

jq -n --argjson w "${windows}" --argjson n "${WINDOWS}" --argjson ws "${WINDOW_S}" \
  --argjson versions "$(versions_json)" '
  def stats: (add / length) as $m
    | {min: min, max: max, mean: ($m * 10 | round / 10),
       max_over_min: ((max / min) * 100 | round / 100),
       cv_pct: ((((map((. - $m) * (. - $m)) | add) / length) | sqrt) / $m * 1000 | round / 10)};
  {windows: $n, window_s: $ws, versions: $versions, per_window: $w,
   p50_spread_ms: ([$w[].prompts | keys[]] | unique
     | map({key: ., value: (. as $p | [$w[].prompts[$p].p50_ms | select(. != null)] | stats)}) | from_entries),
   errors: ([$w[].prompts[].errors] | add)}' > "${out}"

jq '{p50_spread_ms, errors, host_cpu_busy_pct: [.per_window[].host_cpu_busy_pct]}' "${out}"
log "written ${out#"${ROOT_DIR}/"}"
