#!/usr/bin/env bash
# End-to-end check of the model server (P1 increment 2a). Runs every check,
# prints PASS/FAIL for each, exits non-zero if any failed.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/checklib.sh"

# A new scrape target needs service discovery plus one scrape interval (1m).
POLL_SECONDS=150
SVC="model-server:8080"

cluster_exists || die "cluster ${CLUSTER_NAME} not found, run 'make lab-up' first"

# --- 1. A model-server pod is running and Ready ------------------------------
if out="$(kc -n "${GENAI_NAMESPACE}" wait --for=condition=Ready pod \
  -l app.kubernetes.io/name=model-server --timeout=120s 2>&1)"; then
  pass "model-server pod is Ready"
else
  fail "model-server pod not Ready: ${out}"
fi
# The pod that is actually running now, and its IP. Prometheus keeps
# returning the last samples of a vanished target for up to 5 minutes, so
# every series below is pinned to this pod to reject stale data.
running_pod="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
running_ip="$(kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)"

# --- 2. /health answers ok ---------------------------------------------------
health="$(svc_get "${GENAI_NAMESPACE}" "${SVC}" /health 2>&1 || true)"
if [[ "$(jq -r '.status // empty' <<<"${health}" 2>/dev/null)" == "ok" ]]; then
  pass "/health returned {\"status\":\"ok\"}"
else
  fail "/health did not return ok: ${health}"
fi

# --- 3. A chat completion returns content and token usage -------------------
request='{"model":"qwen2.5-0.5b-instruct","messages":[{"role":"user","content":"Reply with one word: ready"}],"max_tokens":16,"temperature":0}'
resp="$(svc_post_json "${GENAI_NAMESPACE}" "${SVC}" /v1/chat/completions "${request}" 2>&1 || true)"
content="$(jq -r '.choices[0].message.content // empty' <<<"${resp}" 2>/dev/null || true)"
tokens="$(jq -r '.usage.completion_tokens // 0' <<<"${resp}" 2>/dev/null || echo 0)"
if [[ -n "${content}" && "${tokens}" -gt 0 ]]; then
  pass "chat completion returned ${tokens} tokens: $(head -c 60 <<<"${content}" | tr '\n' ' ')"
else
  fail "chat completion failed: $(head -c 300 <<<"${resp}")"
fi

# --- 4. Prometheus scrapes the llama.cpp metrics of the running pod ----------
# This scrape job has no pod label; the target is identified by its
# instance (pod IP:port).
if [[ -z "${running_ip}" ]]; then
  fail "scrape target: no running model-server pod"
  fail "llama.cpp token counter: no running model-server pod"
else
  instance="${running_ip}:8080"
  check_series "model-server scrape target up (instance ${instance})" \
    "up{namespace=\"${GENAI_NAMESPACE}\", service=\"model-server\", instance=\"${instance}\"} == 1"
  check_series "llama.cpp token counter (instance ${instance})" \
    "llamacpp:tokens_predicted_total{namespace=\"${GENAI_NAMESPACE}\", instance=\"${instance}\"}"
fi

# --- 5. cAdvisor throttling series for the running model-server container ---
# Proves the series exists for the pod running now (not a stale one from a
# previous pod). It does not prove that throttling happens.
if [[ -z "${running_pod}" ]]; then
  fail "throttling series: no running model-server pod"
else
  check_series "CPU throttling series for running pod ${running_pod}" \
    "container_cpu_cfs_throttled_periods_total{namespace=\"${GENAI_NAMESPACE}\", container=\"model-server\", pod=\"${running_pod}\"}"
fi

# --- Info: the llama.cpp build actually running -----------------------------
if [[ -n "${running_pod}" ]]; then
  info "llama-server: $(kc -n "${GENAI_NAMESPACE}" exec "${running_pod}" -- /app/llama-server --version 2>&1 | head -1)"
fi

check_summary "check-model"
