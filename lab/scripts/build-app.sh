#!/usr/bin/env bash
# Build the chat-api image and load it into the kind cluster.
#
# The tag is derived from the content of the build inputs, so a changed
# source always produces a new tag (no stale image reused by the nodes) and
# an unchanged source is not rebuilt. Prints the image reference on stdout.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

APP_DIR="${LAB_DIR}/apps/chat-api"

content_tag() {
  (
    cd "${APP_DIR}"
    find Dockerfile requirements.txt chat_api -type f -name '*' ! -name '*.pyc' ! -path '*/__pycache__/*' \
      | LC_ALL=C sort | xargs sha256sum
  ) | sha256sum | cut -c1-12
}

image="kubepilot/chat-api:$(content_tag)"

if docker image inspect "${image}" >/dev/null 2>&1; then
  log "image ${image} already built" >&2
else
  log "building ${image}" >&2
  docker build --quiet -t "${image}" "${APP_DIR}" >&2
fi

if cluster_exists; then
  log "loading ${image} into kind cluster ${CLUSTER_NAME}" >&2
  "${KIND}" load docker-image "${image}" --name "${CLUSTER_NAME}" >&2
fi

echo "${image}"
