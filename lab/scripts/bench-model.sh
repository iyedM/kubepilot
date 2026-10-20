#!/usr/bin/env bash
# Latency benchmark of the model server, from a pod inside the cluster.
#
# One warm-up request (reported separately) then BENCH_REQUESTS sequential
# requests with a fixed prompt, temperature 0, MAX_TOKENS output tokens and
# prompt caching disabled, so every request does the same work. Records the
# raw responses, the host CPU load during the run, and the CFS throttling
# counters of the model-server container before and after.
#
# Output: lab/measurements/<name>.jsonl (raw) and <name>-summary.json.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/checklib.sh"

BENCH_REQUESTS="${BENCH_REQUESTS:-50}"
MAX_TOKENS="${MAX_TOKENS:-64}"
BENCH_NAME="${BENCH_NAME:-2a-model-latency}"
OUT_DIR="${LAB_DIR}/measurements"
POD="bench-model"
PROMPT="Explain in two sentences what a Kubernetes readiness probe does."

cluster_exists || die "cluster ${CLUSTER_NAME} not found, run 'make lab-up' first"
mkdir -p "${OUT_DIR}"

model_pod="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')"
[[ -n "${model_pod}" ]] || die "no running model-server pod"

# prom_value EXPR UNIX_TIME -> first sample value of an instant query at TIME.
prom_value() {
  local q
  q="$(jq -rn --arg q "$1" '$q | @uri')"
  svc_get "${OBS_NAMESPACE}" prometheus-server:80 "/api/v1/query?query=${q}&time=$2" 2>/dev/null \
    | jq -r '.data.result[0].value[1] // "null"'
}
sel="namespace=\"${GENAI_NAMESPACE}\", container=\"model-server\", pod=\"${model_pod}\""
throttled_expr="container_cpu_cfs_throttled_periods_total{${sel}}"
periods_expr="container_cpu_cfs_periods_total{${sel}}"

# Aggregate host CPU counters from /proc/stat: prints "busy total" jiffies.
cpu_jiffies() {
  awk '/^cpu / {busy=$2+$3+$4+$7+$8+$9; total=busy+$5+$6; print busy, total}' /proc/stat
}

request="$(jq -cn --arg p "${PROMPT}" --argjson n "${MAX_TOKENS}" \
  '{model:"qwen2.5-0.5b-instruct", messages:[{role:"user",content:$p}],
    max_tokens:$n, temperature:0, cache_prompt:false}')"
url="http://model-server.${GENAI_NAMESPACE}.svc.cluster.local:8080/v1/chat/completions"

# The pod prints one JSON line per request: index, HTTP code, curl timing and
# the raw response body (or null when the request failed). Single quotes are
# intended: the variables are expanded by sh inside the pod, not here.
# shellcheck disable=SC2016
script='
i=0
while [ "$i" -le "$N" ]; do
  code=$(curl -s -o /tmp/body -w "%{http_code} %{time_total}" \
    -H "content-type: application/json" -d "$REQ" "$URL") || code="000 0"
  set -- $code
  if [ "$1" = "200" ]; then body=$(cat /tmp/body); else body=null; fi
  printf "{\"i\":%d,\"http\":%s,\"time_total\":%s,\"resp\":%s}\n" "$i" "$1" "$2" "$body"
  i=$((i + 1))
done
'

kc -n "${GENAI_NAMESPACE}" delete pod "${POD}" --ignore-not-found --now >/dev/null

started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
t_start="$(date +%s)"
read -r busy0 total0 < <(cpu_jiffies)
load0="$(cut -d' ' -f1-3 /proc/loadavg)"
throttled0="$(prom_value "${throttled_expr}" "${t_start}")"
periods0="$(prom_value "${periods_expr}" "${t_start}")"

log "running ${BENCH_REQUESTS} requests (+1 warm-up) against ${model_pod}, max_tokens=${MAX_TOKENS}"
kc -n "${GENAI_NAMESPACE}" run "${POD}" --image="${CURL_IMAGE}" --restart=Never \
  --env="N=${BENCH_REQUESTS}" --env="URL=${url}" --env="REQ=${request}" \
  --command -- sh -c "${script}" >/dev/null
kc -n "${GENAI_NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${POD}" --timeout=30m >/dev/null

t_end="$(date +%s)"
read -r busy1 total1 < <(cpu_jiffies)
load1="$(cut -d' ' -f1-3 /proc/loadavg)"

raw="${OUT_DIR}/${BENCH_NAME}.jsonl"
kc -n "${GENAI_NAMESPACE}" logs "${POD}" > "${raw}"
kc -n "${GENAI_NAMESPACE}" delete pod "${POD}" --now >/dev/null

# cAdvisor is scraped every minute: wait for one scrape after the run.
log "waiting 75s for a Prometheus scrape after the run"
sleep 75
t_after="$(date +%s)"
throttled1="$(prom_value "${throttled_expr}" "${t_after}")"
periods1="$(prom_value "${periods_expr}" "${t_after}")"

version="$(kc -n "${GENAI_NAMESPACE}" exec "${model_pod}" -- /app/llama-server --version 2>&1 | head -1)"
host_busy_pct="$(awk -v b0="${busy0}" -v t0="${total0}" -v b1="${busy1}" -v t1="${total1}" \
  'BEGIN {printf "%.1f", 100 * (b1 - b0) / (t1 - t0)}')"

summary="${OUT_DIR}/${BENCH_NAME}-summary.json"
jq -s \
  --arg started "${started}" --arg duration_s "$((t_end - t_start))" \
  --arg image "${LLAMACPP_IMAGE}" --arg version "${version}" \
  --arg model "${MODEL_FILE}" --arg model_sha256 "${MODEL_SHA256}" \
  --arg pod "${model_pod}" --arg prompt "${PROMPT}" \
  --argjson max_tokens "${MAX_TOKENS}" --argjson n "${BENCH_REQUESTS}" \
  --arg nproc "$(nproc)" --arg host_busy_pct "${host_busy_pct}" \
  --arg load0 "${load0}" --arg load1 "${load1}" \
  --arg throttled0 "${throttled0}" --arg throttled1 "${throttled1}" \
  --arg periods0 "${periods0}" --arg periods1 "${periods1}" '
  def pct(p): sort | .[((length * p) | ceil) - 1];
  (map(select(.i == 0)) | first) as $warm
  | map(select(.i > 0)) as $runs
  | ($runs | map(select(.http == 200))) as $ok
  | ($ok | map(.time_total)) as $lat
  | {
      started_utc: $started, duration_s: ($duration_s | tonumber),
      conditions: {
        image: $image, llama_server_version: $version,
        model: $model, model_sha256: $model_sha256, model_server_pod: $pod,
        cpu_limit: "2", threads: 2, parallel: 1, context: 2048,
        prompt: $prompt, max_tokens: $max_tokens, temperature: 0, cache_prompt: false,
        client: "curl pod in the same cluster, sequential requests"
      },
      host: {
        nproc: ($nproc | tonumber), cpu_busy_pct_during_run: ($host_busy_pct | tonumber),
        loadavg_before: $load0, loadavg_after: $load1
      },
      requests: $n, ok: ($ok | length), failed: ($n - ($ok | length)),
      warmup_s: $warm.time_total,
      latency_s: {
        p50: ($lat | pct(0.50)), p95_indicative: ($lat | pct(0.95)),
        max: ($lat | max), min: ($lat | min)
      },
      completion_tokens_mean: ($ok | map(.resp.usage.completion_tokens) | add / length),
      prompt_tokens: ($ok[0].resp.usage.prompt_tokens),
      generation_tokens_per_s_mean: ($ok | map(.resp.timings.predicted_per_second) | add / length),
      prompt_ms_mean: ($ok | map(.resp.timings.prompt_ms) | add / length),
      cfs_throttling: {
        throttled_periods_delta: (if $throttled0 == "null" or $throttled1 == "null" then null
                                  else ($throttled1 | tonumber) - ($throttled0 | tonumber) end),
        periods_delta: (if $periods0 == "null" or $periods1 == "null" then null
                        else ($periods1 | tonumber) - ($periods0 | tonumber) end),
        note: "counters read from Prometheus (1m scrape) before the run and 75s after it"
      }
    }' "${raw}" > "${summary}"

cat "${summary}"
log "raw: ${raw}"
log "summary: ${summary}"
