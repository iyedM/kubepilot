#!/usr/bin/env bash
# Compatibility matrix for the OpenAI instrumentation packages (P1, step 0
# of increment 2b). Each case installs one instrumentation package and one
# openai SDK version in a throwaway python container and runs probe.py
# against the real llama-server (same image digest and model as the lab),
# started locally in Docker. Results go to eval/fixtures/genai-instrumentation/.
#
# Also checks that the longest planned request (~600 prompt tokens +
# 256 generated tokens) fits in the server context (-c 2048).
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"

PYTHON_IMAGE="python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1"
NET="kp-compat"
LLAMA="kp-compat-llama"
OUT_ROOT="${ROOT_DIR}/eval/fixtures/genai-instrumentation"
OTEL_SDK="opentelemetry-sdk==1.45.1"

"${LAB_DIR}/scripts/models.sh"

cleanup() {
  docker rm -f "${LLAMA}" >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
docker network create "${NET}" >/dev/null

# Same flags as lab/genai/model-server.yaml.
docker run -d --name "${LLAMA}" --network "${NET}" --user 65532:65532 --read-only \
  --cap-drop ALL --security-opt no-new-privileges --cpus 2 \
  -v "${MODELS_DIR}:/models:ro" -p 127.0.0.1:18080:8080 \
  "${LLAMACPP_IMAGE}" -m "/models/${MODEL_FILE}" --host 0.0.0.0 --port 8080 \
  --metrics -c 2048 --threads 2 --parallel 1 --alias qwen2.5-0.5b-instruct >/dev/null
for _ in $(seq 1 60); do
  [[ "$(curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:18080/health)" == "200" ]] && break
  sleep 1
done
log "llama-server up: $(docker exec "${LLAMA}" /app/llama-server --version 2>&1 | head -1)"

# --- context check: ~600 prompt tokens + 256 generated tokens -----------------
n_ctx="$(curl -s 127.0.0.1:18080/props | jq '.default_generation_settings.n_ctx')"
sentence="A Kubernetes readiness probe tells the kubelet when a container can receive traffic from a Service. "
long_prompt=""
for _ in $(seq 1 30); do long_prompt+="${sentence}"; done
prompt_raw_tokens="$(jq -n --arg c "${long_prompt}" '{content:$c}' \
  | curl -s 127.0.0.1:18080/tokenize -H 'content-type: application/json' -d @- | jq '.tokens | length')"
ctx_resp="$(jq -n --arg c "${long_prompt}Summarize the text above." \
  '{model:"qwen2.5-0.5b-instruct", messages:[{role:"user",content:$c}],
    max_tokens:256, temperature:0, cache_prompt:false, ignore_eos:true}' \
  | curl -s 127.0.0.1:18080/v1/chat/completions -H 'content-type: application/json' -d @-)"
log "context check: n_ctx=${n_ctx}, raw prompt tokens=${prompt_raw_tokens}, $(jq -c '{usage: .usage, finish_reason: .choices[0].finish_reason, error: .error}' <<<"${ctx_resp}")"

# --- matrix ------------------------------------------------------------------
# case | packages | semconv opt-in | content capture
cases=(
  "A1|opentelemetry-instrumentation-openai-v2==2.4b0 openai==3.35.1||"
  "A2|opentelemetry-instrumentation-openai-v2==2.4b0 openai==3.35.1|gen_ai_latest_experimental|"
  "B|opentelemetry-instrumentation-openai-v2==2.4b0 openai==2.54.0||"
  "C|opentelemetry-instrumentation-genai-openai==1.2b0 openai==3.35.1||"
  # Positive control for the privacy detector: content capture switched on.
  "C-capture|opentelemetry-instrumentation-genai-openai==1.2b0 openai==3.35.1||span_and_event"
)

for entry in "${cases[@]}"; do
  IFS='|' read -r name pkgs semconv capture <<<"${entry}"
  out="${OUT_ROOT}/${name}"
  rm -rf "${out}" && mkdir -p "${out}"
  log "case ${name}: ${pkgs} semconv='${semconv:-default}' capture='${capture:-default}'"
  # Only pass the OTEL_* settings that are set: an empty variable is not
  # guaranteed to mean "unset" to every package.
  env_args=(-e CASE="${name}" -e BASE_URL="http://${LLAMA}:8080/v1" -e MODEL=qwen2.5-0.5b-instruct -e OUT_DIR=/out)
  [[ -n "${semconv}" ]] && env_args+=(-e OTEL_SEMCONV_STABILITY_OPT_IN="${semconv}")
  [[ -n "${capture}" ]] && env_args+=(-e OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT="${capture}")
  # Run as the invoking user so the fixtures written into the repo are not
  # owned by root; packages go to a user site under /tmp.
  # shellcheck disable=SC2086  # pkgs is a space-separated list on purpose
  if ! docker run --rm --network "${NET}" --user "$(id -u):$(id -g)" \
    -e HOME=/tmp -e PYTHONUSERBASE=/tmp/pyuser "${env_args[@]}" \
    -v "${LAB_DIR}/compat:/compat:ro" -v "${out}:/out" \
    "${PYTHON_IMAGE}" sh -c "pip install --user --quiet --no-cache-dir --disable-pip-version-check ${OTEL_SDK} ${pkgs} \
      && pip freeze --user > /out/pip-freeze.txt && python /compat/probe.py" > "${out}/run.log" 2>&1; then
    log "case ${name}: probe FAILED, see ${out}/run.log"
    tail -20 "${out}/run.log"
  else
    tail -1 "${out}/run.log"
  fi
done
log "fixtures in ${OUT_ROOT}"
