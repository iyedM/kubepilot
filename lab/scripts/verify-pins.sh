#!/usr/bin/env bash
# Check that every pinned artifact in lab/versions.env resolves: node image
# digest, chart versions, and every container image the charts will pull with
# our values. Stops at the first failure and prints the exact error; nothing
# is substituted.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

fail() {
  printf 'PIN CHECK FAILED: %s\n%s\n' "$1" "$2" >&2
  exit 1
}

log "node image ${KIND_NODE_IMAGE}"
if ! out="$(docker pull --quiet "${KIND_NODE_IMAGE}" 2>&1)"; then
  fail "node image ${KIND_NODE_IMAGE}" "${out}"
fi

# chart NAME REPO VERSION
check_chart() {
  log "chart $1 $3 from $2"
  local out
  if ! out="$(helm show chart --repo "$2" "$1" --version "$3" 2>&1)"; then
    fail "chart $1 $3 ($2)" "${out}"
  fi
}
check_chart tempo "${TEMPO_CHART_REPO}" "${TEMPO_CHART_VERSION}"
check_chart prometheus "${PROMETHEUS_CHART_REPO}" "${PROMETHEUS_CHART_VERSION}"
check_chart opentelemetry-collector "${OTELCOL_CHART_REPO}" "${OTELCOL_CHART_VERSION}"

# Render the charts with the lab values and list the images they reference.
rendered="$(
  {
    helm template tempo tempo --repo "${TEMPO_CHART_REPO}" --version "${TEMPO_CHART_VERSION}" \
      -n "${OBS_NAMESPACE}" -f "${LAB_DIR}/observability/tempo-values.yaml"
    helm template prometheus prometheus --repo "${PROMETHEUS_CHART_REPO}" --version "${PROMETHEUS_CHART_VERSION}" \
      -n "${OBS_NAMESPACE}" -f "${LAB_DIR}/observability/prometheus-values.yaml"
    helm template otel-collector opentelemetry-collector --repo "${OTELCOL_CHART_REPO}" --version "${OTELCOL_CHART_VERSION}" \
      -n "${OBS_NAMESPACE}" -f "${LAB_DIR}/observability/otel-collector-values.yaml" \
      --set image.repository="${OTELCOL_IMAGE_REPO}" --set image.tag="${OTELCOL_IMAGE_TAG}"
  } 2>&1
)" || fail "helm template" "${rendered}"

mapfile -t images < <(
  {
    grep -E '^\s*image:\s' <<<"${rendered}" | sed -E 's/^\s*image:\s*//; s/"//g'
    echo "${TELEMETRYGEN_IMAGE}"
  } | sort -u
)

for img in "${images[@]}"; do
  log "image ${img}"
  if ! out="$(docker manifest inspect "${img}" 2>&1 >/dev/null)"; then
    fail "image ${img}" "${out}"
  fi
done

log "all pins resolve (${#images[@]} images, 3 charts, 1 node image)"
