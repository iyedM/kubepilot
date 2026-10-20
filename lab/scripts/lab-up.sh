#!/usr/bin/env bash
# Create the kind cluster (if missing) and install the observability stack.
# Idempotent: re-running upgrades the Helm releases in place.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

EVENT_TTL="${EVENT_TTL:-1h}"
export EVENT_TTL

"${LAB_DIR}/scripts/tools.sh"
"${LAB_DIR}/scripts/verify-pins.sh"

if cluster_exists; then
  log "cluster ${CLUSTER_NAME} already exists (EVENT_TTL only applies at creation)"
else
  rendered="$(mktemp --suffix .yaml)"
  trap 'rm -f "${rendered}"' EXIT
  # Substitute only ${EVENT_TTL}, nothing else in the file.
  # shellcheck disable=SC2016
  envsubst '${EVENT_TTL}' < "${LAB_DIR}/kind/cluster.yaml" > "${rendered}"
  log "creating cluster ${CLUSTER_NAME} (event-ttl=${EVENT_TTL})"
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

kc -n "${OBS_NAMESPACE}" get pods -o wide
log "lab is up. Use it with: export KUBECONFIG=${KUBECONFIG}"
