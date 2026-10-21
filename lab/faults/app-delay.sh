#!/usr/bin/env bash
# app-delay fault: chat-api is slow before the model call (lab hook), visible
# as a gap between the HTTP span and the GenAI span in the trace, with no
# Kubernetes event, no resource metric and no configuration change explaining
# it. Expected answer: unknown_no_infrastructure_cause.
#   inject RUN_DIR VARIANT   set the delay through the lab hook
#   reset  RUN_DIR           set the delay back to 0
#   check  RUN_DIR           exit 0 only if the trace gap is really there
# Requires LAB_FAULT_HOOKS=1 on chat-api (make lab-hooks-on, between runs).
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

MIN_GAP_FRACTION="0.8"         # median gap >= 80 % of the injected delay
MAX_THROTTLED_FRACTION="0.05"

# set_delay MS [EVERY_N]: PUT /lab/delay on the ready chat-api pod; prints the answer.
set_delay() {
  local ms="$1" every_n="${2:-1}" pod
  pod="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=chat-api -o json \
    | jq -r '[.items[] | select(.metadata.deletionTimestamp == null and any(.status.conditions[]?; .type == "Ready" and .status == "True"))][0].metadata.name // empty')"
  [[ -n "${pod}" ]] || die "no ready chat-api pod"
  kc -n "${GENAI_NAMESPACE}" exec "${pod}" -c chat-api -- python -c "
import json, urllib.request
req = urllib.request.Request('http://127.0.0.1:8000/lab/delay',
                             data=json.dumps({'ms': ${ms}, 'every_n': ${every_n}}).encode(),
                             headers={'content-type': 'application/json'}, method='PUT')
print(urllib.request.urlopen(req, timeout=10).read().decode())"
}

inject() {
  local dir="$1" variant="$2" ms every_n
  [[ "$(chat_api_hooks)" == 1 ]] || die "LAB_FAULT_HOOKS is off on chat-api: run 'make lab-hooks-on' between runs"
  case "${variant}" in
    delay-*)
      ms="$(run_get "${dir}" '.fault.params.delay_ms')"
      set_delay "${ms}" 1 >/dev/null
      run_set "${dir}" '.fault.applied = {variant: $v, delay_ms: ($m | tonumber), every_n: 1}' --arg v "${variant}" --arg m "${ms}"
      ;;
    intermittent-delay)
      ms="$(run_get "${dir}" '.fault.params.delay_ms')"
      every_n="$(run_get "${dir}" '.fault.params.every_n')"
      set_delay "${ms}" "${every_n}" >/dev/null
      run_set "${dir}" '.fault.applied = {variant: $v, delay_ms: ($m | tonumber), every_n: ($e | tonumber)}' \
        --arg v "${variant}" --arg m "${ms}" --arg e "${every_n}"
      ;;
    *) die "unknown or not yet supported app-delay variant ${variant}" ;;
  esac
}

reset() {
  set_delay 0 1 >/dev/null || true
}

check() {
  local dir="$1" start done_t ms every_n gaps n median delayed fast pod now dur sel throttled periods fraction events rc=0
  start="$(run_get "${dir}" '.timeline.inject_start')"
  done_t="$(run_get "${dir}" '.timeline.inject_done')"
  ms="$(run_get "${dir}" '.fault.applied.delay_ms // empty')"
  every_n="$(run_get "${dir}" '.fault.applied.every_n // 1')"
  [[ -n "${ms}" ]] || { echo "FAIL  no app-delay injection recorded in this run"; return 1; }
  # Gap between the HTTP server span start and the GenAI span start, in the
  # traces of the load generator requests made during the fault window.
  gaps="$(kc -n "${GENAI_NAMESPACE}" logs deploy/loadgen --since-time="$(date -u -d "${done_t}" +%Y-%m-%dT%H:%M:%SZ)" 2>/dev/null \
    | jq -r 'select(.status == 200 and .trace_id != null) | .trace_id' | tail -12 | while read -r tid; do
        svc_get "${OBS_NAMESPACE}" tempo:3200 "/api/v2/traces/${tid}" 2>/dev/null | jq -r '
          [.trace.resourceSpans[]?.scopeSpans[]?.spans[]?] as $s
          | ($s | map(select(.kind == "SPAN_KIND_SERVER")) | first) as $srv
          | ($s | map(select(any(.attributes[]?; .key == "gen_ai.operation.name"))) | first) as $g
          | if $srv and $g then ((($g.startTimeUnixNano | tonumber) - ($srv.startTimeUnixNano | tonumber)) / 1e6 | floor) else empty end' 2>/dev/null || true
      done)"
  n="$(grep -c . <<<"${gaps}" || true)"
  median="$(sort -n <<<"${gaps}" | awk '{a[NR]=$1} END {if (NR) print a[int((NR + 1) / 2)]}')"
  if [[ "${every_n}" -gt 1 ]]; then
    delayed="$(awk -v d="${ms}" -v f="${MIN_GAP_FRACTION}" '$1 >= f * d {c++} END {print c + 0}' <<<"${gaps}")"
    fast="$(awk -v d="${ms}" '$1 < 0.2 * d {c++} END {print c + 0}' <<<"${gaps}")"
    if [[ "${n}" -ge 6 && "${delayed}" -ge 2 && "${fast}" -ge 2 ]]; then
      echo "PASS  intermittent trace gap HTTP span start -> GenAI span start: ${delayed}/${n} delayed (>= ${MIN_GAP_FRACTION} x ${ms} ms), ${fast}/${n} fast (median ${median} ms, every_n=${every_n})"
    else
      echo "FAIL  intermittent trace gap: ${delayed}/${n} delayed, ${fast}/${n} fast over ${n} traces (want >= 2 delayed and >= 2 fast on >= 6 traces)"
      rc=1
    fi
  else
    if [[ "${n}" -ge 3 ]] && awk -v m="${median}" -v d="${ms}" -v f="${MIN_GAP_FRACTION}" 'BEGIN {exit !(m >= f * d)}'; then
      echo "PASS  trace gap HTTP span start -> GenAI span start: median ${median} ms over ${n} traces (delay ${ms} ms)"
    else
      echo "FAIL  trace gap: median '${median}' ms over ${n} traces (want >= ${MIN_GAP_FRACTION} x ${ms} ms on >= 3 traces)"
      rc=1
    fi
  fi
  pod="$(running_pod model-server)"
  now="$(date +%s)"
  dur=$(( now - $(to_epoch "${done_t}") ))
  sel="namespace=\"genai\",container=\"model-server\",pod=\"${pod}\""
  throttled="$(prom_scalar "increase(container_cpu_cfs_throttled_periods_total{${sel}}[${dur}s])" "${now}")"
  periods="$(prom_scalar "increase(container_cpu_cfs_periods_total{${sel}}[${dur}s])" "${now}")"
  fraction="$(awk -v t="${throttled:-0}" -v p="${periods:-0}" 'BEGIN {printf "%.3f", (p > 0 ? t / p : 0)}')"
  if [[ -n "${periods}" ]] && awk -v f="${fraction}" -v m="${MAX_THROTTLED_FRACTION}" 'BEGIN {exit !(f < m)}'; then
    echo "PASS  no CFS throttling of the model server: fraction ${fraction}"
  else
    echo "FAIL  CFS throttled fraction ${fraction} (periods '${periods}')"
    rc=1
  fi
  events="$(kc -n "${GENAI_NAMESPACE}" get events -o json | jq --arg t "${start:0:19}" \
    '[.items[] | select(((.lastTimestamp // .eventTime // .firstTimestamp) // "") >= $t)] | length')"
  if [[ "${events}" -eq 0 ]]; then
    echo "PASS  no Kubernetes event in genai since the injection"
  else
    echo "FAIL  ${events} Kubernetes event(s) in genai since the injection"
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
