#!/usr/bin/env bash
# End-to-end check of the instrumented GenAI app (P1 increment 2b). Runs
# every check, prints PASS/FAIL for each, exits non-zero if any failed.
#
# Requests are sent from inside the chat-api pod to the chat-api Service
# (kubectl exec + python urllib), i.e. through the same path as a real
# in-cluster client. Every Prometheus check is pinned to the pod running now.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/checklib.sh"

POLL_SECONDS=90
RULES="${LAB_DIR}/genai/content-keys.json"
CANARY="KPCANARY7F3A" # same marker as chat_api/prompts.py
CHAT_URL="http://chat-api.${GENAI_NAMESPACE}.svc.cluster.local:8000/chat"

cluster_exists || die "cluster ${CLUSTER_NAME} not found, run 'make lab-up' first"

running() { # running LABEL JSONPATH
  kc -n "${GENAI_NAMESPACE}" get pods -l "app.kubernetes.io/name=$1" \
    --field-selector=status.phase=Running -o jsonpath="{.items[0].$2}" 2>/dev/null || true
}

# chat PROMPT_ID -> {"status": N, "body": {...}} (status 0 on transport error)
chat() {
  kc -n "${GENAI_NAMESPACE}" exec "${chat_pod}" -c chat-api -- python -c '
import json, sys, urllib.request, urllib.error
req = urllib.request.Request(sys.argv[1], data=json.dumps({"prompt_id": sys.argv[2]}).encode(),
                             headers={"content-type": "application/json"}, method="POST")
try:
    r = urllib.request.urlopen(req, timeout=120)
    print(json.dumps({"status": r.status, "body": json.load(r)}))
except urllib.error.HTTPError as e:
    print(json.dumps({"status": e.code, "body": json.load(e)}))
except Exception as e:
    print(json.dumps({"status": 0, "body": {"error": type(e).__name__}}))
' "${CHAT_URL}" "$1" 2>/dev/null || echo '{"status":0,"body":{"error":"exec failed"}}'
}

# fetch_trace TRACE_ID -> Tempo v2 JSON once the GenAI span is there.
fetch_trace() {
  local t=""
  for _ in $(seq 1 $((POLL_SECONDS / 5))); do
    t="$(svc_get "${OBS_NAMESPACE}" tempo:3200 "/api/v2/traces/$1" 2>/dev/null || true)"
    if jq -e '[.trace.resourceSpans[]?.scopeSpans[]?.spans[]?
               | select(any(.attributes[]?; .key == "gen_ai.operation.name"))] | length > 0' \
      <<<"${t}" >/dev/null 2>&1; then
      echo "${t}"
      return 0
    fi
    sleep 5
  done
  echo "${t}"
  return 1
}

# --- 1. chat-api and loadgen are running and Ready ----------------------------
for app in chat-api loadgen; do
  if out="$(kc -n "${GENAI_NAMESPACE}" wait --for=condition=Ready pod \
    -l "app.kubernetes.io/name=${app}" --timeout=120s 2>&1)"; then
    pass "${app} pod is Ready"
  else
    fail "${app} pod not Ready: ${out}"
  fi
done
chat_pod="$(running chat-api metadata.name)"
chat_uid="$(running chat-api metadata.uid)"
[[ -n "${chat_pod}" ]] || die "no running chat-api pod: cannot run the remaining checks"

# --- 2. POST /chat answers 200 with tokens and a trace id ---------------------
resp="$(chat check)"
trace_id="$(jq -r '.body.trace_id // empty' <<<"${resp}")"
if [[ "$(jq -r .status <<<"${resp}")" == "200" && "$(jq -r '.body.completion_tokens // 0' <<<"${resp}")" -gt 0 ]]; then
  pass "POST /chat returned 200, $(jq -r .body.completion_tokens <<<"${resp}") tokens, trace ${trace_id}"
else
  fail "POST /chat failed: ${resp}"
fi

# --- 3. That exact trace is in Tempo: HTTP span -> GenAI span with tokens ------
if [[ -z "${trace_id}" ]]; then
  fail "trace: no trace id returned by /chat"
  trace=""
elif ! trace="$(fetch_trace "${trace_id}")"; then
  fail "trace ${trace_id}: no GenAI span in Tempo after ${POLL_SECONDS}s"
else
  verdict="$(jq -r '
    [.trace.resourceSpans[].scopeSpans[].spans[]] as $spans
    | ($spans | map(select(.kind == "SPAN_KIND_SERVER")) | first) as $server
    | ($spans | map(select(any(.attributes[]?; .key == "gen_ai.operation.name"))) | first) as $chat
    | ([$chat.attributes[] | select(.key == "gen_ai.usage.output_tokens") | .value.intValue | tonumber] | first // 0) as $out
    | if $server == null then "no HTTP server span"
      elif $chat.parentSpanId != $server.spanId then "GenAI span is not a child of the HTTP server span"
      elif $out <= 0 then "GenAI span has no output tokens"
      else "ok \($chat.name) output_tokens=\($out)" end' <<<"${trace}")"
  if [[ "${verdict}" == ok* ]]; then
    pass "trace ${trace_id}: HTTP server span -> ${verdict#ok }"
  else
    fail "trace ${trace_id}: ${verdict}"
  fi
  err="$(jq -r '[.trace.resourceSpans[].scopeSpans[].spans[].attributes[]? | select(.key == "error.type") | .value.stringValue] | first // empty' <<<"${trace}")"
  [[ -n "${err}" ]] && info "trace ${trace_id}: GenAI span error.type=${err}"
fi

# --- 4. The trace resource identifies the running pod --------------------------
if [[ -n "${trace}" ]]; then
  res="$(jq -c '
    [.trace.resourceSpans[] | select(any(.scopeSpans[].spans[]; any(.attributes[]?; .key == "gen_ai.operation.name")))
     | .resource.attributes[] | {(.key): .value.stringValue}] | add' <<<"${trace}")"
  uid="$(jq -r '."k8s.pod.uid" // empty' <<<"${res}")"
  name="$(jq -r '."k8s.pod.name" // empty' <<<"${res}")"
  deploy="$(jq -r '."k8s.deployment.name" // empty' <<<"${res}")"
  if [[ "${uid}" == "${chat_uid}" && "${name}" == "${chat_pod}" ]]; then
    pass "trace resource k8s.pod.uid/name match the running pod (${chat_pod})"
  else
    fail "trace resource k8s.pod.uid='${uid}' name='${name}', running pod uid='${chat_uid}' name='${chat_pod}'"
  fi
  # k8s.deployment.name is not set by the app: the collector's k8sattributes
  # adds it after matching the pod, here through k8s.pod.uid.
  if [[ "${deploy}" == "chat-api" ]]; then
    pass "collector k8sattributes added k8s.deployment.name=chat-api"
  else
    fail "k8s.deployment.name='${deploy}', expected chat-api (k8sattributes association failed)"
  fi
else
  fail "trace resource: no trace to inspect"
fi

# --- 5. Privacy: no prompt/completion content in the trace or collector logs ---
resp="$(chat privacy-canary)"
ptrace_id="$(jq -r '.body.trace_id // empty' <<<"${resp}")"
if [[ -z "${ptrace_id}" ]] || ! ptrace="$(fetch_trace "${ptrace_id}")"; then
  fail "privacy: could not get the canary trace (response: ${resp})"
else
  findings="$(jq -r --slurpfile r "${RULES}" --arg canary "${CANARY}" '
    ($r[0].attribute_keys) as $keys | ($r[0].attribute_key_prefixes) as $prefixes | ($r[0].event_names) as $events
    | def content_key: . as $k | ($k | IN($keys[])) or any($prefixes[]; . as $p | $k | startswith($p));
    [ .trace.resourceSpans[].scopeSpans[].spans[] as $s
      | ( ($s.attributes // [])[] | select(.key | content_key) | "span \"\($s.name)\" attribute \(.key)" ),
        ( ($s.events // [])[] | select(.name | IN($events[])) | "span \"\($s.name)\" event \(.name)" ),
        ( ($s.events // [])[] as $e | ($e.attributes // [])[] | select(.key | content_key)
          | "span \"\($s.name)\" event \"\($e.name)\" attribute \(.key)" ) ]
    + (if (tostring | contains($canary)) then ["marker \($canary) found in the trace"] else [] end)
    | join("; ")' <<<"${ptrace}")"
  if [[ -z "${findings}" ]]; then
    pass "privacy: no content key and no marker in trace ${ptrace_id}"
  else
    fail "privacy: content found in trace ${ptrace_id}: ${findings}"
  fi
fi
if kc -n "${OBS_NAMESPACE}" logs deploy/otel-collector --since=10m 2>/dev/null | grep -q "${CANARY}"; then
  fail "privacy: marker ${CANARY} found in the collector logs"
else
  pass "privacy: marker absent from the collector logs (last 10m)"
fi

# --- 6. GenAI metrics in Prometheus for the running pod ------------------------
# Either name of the duration metric is accepted (see docs/genai-conventions.md).
check_series "GenAI duration histogram for ${chat_pod}" \
  "{__name__=~\"gen_ai_client_(operation|inference)_duration_seconds_count\", k8s_pod_name=\"${chat_pod}\"}"
check_series "GenAI token usage histogram for ${chat_pod}" \
  "gen_ai_client_token_usage_count{k8s_pod_name=\"${chat_pod}\"}"

# --- 7. The load generator produces successful traffic -------------------------
ok_lines="$(kc -n "${GENAI_NAMESPACE}" logs deploy/loadgen --since=60s 2>/dev/null \
  | jq -r 'select(.status == 200) | .prompt_id' 2>/dev/null | wc -l)"
if [[ "${ok_lines}" -ge 3 ]]; then
  pass "loadgen: ${ok_lines} successful requests in the last 60s"
else
  fail "loadgen: only ${ok_lines} successful requests in the last 60s (expected >= 3)"
fi

check_summary "check-genai"
