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

# Render each chart with the lab values; any rendering error is fatal. Each
# chart is rendered on its own so that one failure cannot be masked by the
# exit status of the next command.
# render RELEASE CHART REPO VERSION ARGS...
render() {
  local release="$1" chart="$2" repo="$3" version="$4" out
  shift 4
  log "render ${chart} ${version} with lab values"
  if ! out="$(helm template "${release}" "${chart}" --repo "${repo}" --version "${version}" \
    -n "${OBS_NAMESPACE}" "$@" 2>&1)"; then
    fail "helm template ${chart} ${version}" "${out}"
  fi
  rendered+="${out}"$'\n'
}
rendered=""
render tempo tempo "${TEMPO_CHART_REPO}" "${TEMPO_CHART_VERSION}" \
  -f "${LAB_DIR}/observability/tempo-values.yaml"
render prometheus prometheus "${PROMETHEUS_CHART_REPO}" "${PROMETHEUS_CHART_VERSION}" \
  -f "${LAB_DIR}/observability/prometheus-values.yaml"
render otel-collector opentelemetry-collector "${OTELCOL_CHART_REPO}" "${OTELCOL_CHART_VERSION}" \
  -f "${LAB_DIR}/observability/otel-collector-values.yaml" \
  --set image.repository="${OTELCOL_IMAGE_REPO}" --set image.tag="${OTELCOL_IMAGE_TAG}"

mapfile -t images < <(
  {
    grep -E '^\s*image:\s' <<<"${rendered}" | sed -E 's/^\s*image:\s*//; s/"//g'
    echo "${TELEMETRYGEN_IMAGE}"
    echo "${LLAMACPP_IMAGE}"
    echo "${CURL_IMAGE}"
  } | sort -u
)

for img in "${images[@]}"; do
  log "image ${img}"
  if ! out="$(docker manifest inspect "${img}" 2>&1 >/dev/null)"; then
    fail "image ${img}" "${out}"
  fi
done

# The model URL is pinned to a repository commit; HEAD it (following the
# redirect to the storage backend). The sha256 is checked by models.sh.
log "model ${MODEL_URL}"
if ! out="$(curl -fsSIL --retry 3 --retry-all-errors --connect-timeout 20 -o /dev/null "${MODEL_URL}" 2>&1)"; then
  fail "model ${MODEL_URL}" "${out}"
fi

log "all pins resolve (${#images[@]} images, 3 charts, 1 node image, 1 model)"
