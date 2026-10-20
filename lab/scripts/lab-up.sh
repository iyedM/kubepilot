#!/usr/bin/env bash
# Create the kind cluster (if missing), install the observability stack and
# the model server. Idempotent: re-running upgrades everything in place.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

EVENT_TTL="${EVENT_TTL:-1h}"
export EVENT_TTL MODELS_DIR

"${LAB_DIR}/scripts/tools.sh"
"${LAB_DIR}/scripts/verify-pins.sh"
# The model directory must exist before kind mounts it.
"${LAB_DIR}/scripts/models.sh"

if cluster_exists; then
  log "cluster ${CLUSTER_NAME} already exists (EVENT_TTL only applies at creation)"
  # A cluster created before increment 2a has no /models mount; it cannot be
  # added to a running kind node, so refuse instead of failing later.
  for node in $("${KIND}" get nodes --name "${CLUSTER_NAME}"); do
    if ! docker inspect "${node}" --format '{{range .Mounts}}{{.Destination}} {{end}}' | grep -qw /models; then
      die "node ${node} has no /models mount: run 'make lab-down && make lab-up' to recreate the cluster"
    fi
  done
else
  rendered="$(mktemp --suffix .yaml)"
  trap 'rm -f "${rendered}"' EXIT
  # Substitute only ${EVENT_TTL} and ${MODELS_DIR}, nothing else in the file.
  # shellcheck disable=SC2016
  envsubst '${EVENT_TTL} ${MODELS_DIR}' < "${LAB_DIR}/kind/cluster.yaml" > "${rendered}"
  log "creating cluster ${CLUSTER_NAME} (event-ttl=${EVENT_TTL}, models=${MODELS_DIR})"
  "${KIND}" create cluster --config "${rendered}" --image "${KIND_NODE_IMAGE}" \
    --kubeconfig "${KUBECONFIG}" --wait 120s
fi

kc create namespace "${OBS_NAMESPACE}" --dry-run=client -o yaml | kc apply -f -

log "installing tempo ${TEMPO_CHART_VERSION}"
hm upgrade --install tempo tempo --repo "${TEMPO_CHART_REPO}" --version "${TEMPO_CHART_VERSION}" \
  -n "${OBS_NAMESPACE}" -f "${LAB_DIR}/observability/tempo-values.yaml" --wait --timeout 5m

log "installing prometheus ${PROMETHEUS_CHART_VERSION}"
hm upgrade --install prometheus prometheus --repo "${PROMETHEUS_CHART_REPO}" --version "${PROMETHEUS_CHART_VERSION}" \
  -n "${OBS_NAMESPACE}" -f "${LAB_DIR}/observability/prometheus-values.yaml" --wait --timeout 5m

log "installing opentelemetry-collector ${OTELCOL_CHART_VERSION} (image ${OTELCOL_IMAGE_REPO}:${OTELCOL_IMAGE_TAG})"
hm upgrade --install otel-collector opentelemetry-collector --repo "${OTELCOL_CHART_REPO}" --version "${OTELCOL_CHART_VERSION}" \
  -n "${OBS_NAMESPACE}" -f "${LAB_DIR}/observability/otel-collector-values.yaml" \
  --set image.repository="${OTELCOL_IMAGE_REPO}" --set image.tag="${OTELCOL_IMAGE_TAG}" \
  --wait --timeout 5m

log "deploying model server (${LLAMACPP_IMAGE##*@}, model ${MODEL_FILE})"
export LLAMACPP_IMAGE MODEL_FILE
# shellcheck disable=SC2016
envsubst '${LLAMACPP_IMAGE} ${MODEL_FILE}' < "${LAB_DIR}/genai/model-server.yaml" | kc apply -f -
kc -n "${GENAI_NAMESPACE}" rollout status deploy/model-server --timeout 5m

CHAT_API_IMAGE="$("${LAB_DIR}/scripts/build-app.sh")"
export CHAT_API_IMAGE
log "deploying chat-api and loadgen (${CHAT_API_IMAGE})"
for manifest in chat-api loadgen; do
  # shellcheck disable=SC2016
  envsubst '${CHAT_API_IMAGE}' < "${LAB_DIR}/genai/${manifest}.yaml" | kc apply -f -
done
kc -n "${GENAI_NAMESPACE}" rollout status deploy/chat-api --timeout 3m
kc -n "${GENAI_NAMESPACE}" rollout status deploy/loadgen --timeout 3m

kc -n "${OBS_NAMESPACE}" get pods -o wide
kc -n "${GENAI_NAMESPACE}" get pods -o wide
log "lab is up. Use it with: export KUBECONFIG=${KUBECONFIG}"
