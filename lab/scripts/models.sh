#!/usr/bin/env bash
# Download the pinned GGUF model into .tools/models and verify its sha256.
# The directory is mounted read-only into every kind node as /models.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"

mkdir -p "${MODELS_DIR}"
dest="${MODELS_DIR}/${MODEL_FILE}"

if [[ -f "${dest}" ]] && echo "${MODEL_SHA256}  ${dest}" | sha256sum --check --status; then
  log "model ${MODEL_FILE} already present, sha256 verified"
  exit 0
fi

log "downloading ${MODEL_FILE} from ${MODEL_URL}"
# --continue-at - resumes a partial download after a network cut.
if ! curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 \
  --continue-at - -o "${dest}.part" "${MODEL_URL}"; then
  die "model download failed: ${MODEL_URL}"
fi
if ! echo "${MODEL_SHA256}  ${dest}.part" | sha256sum --check --status; then
  got="$(sha256sum "${dest}.part" | cut -d' ' -f1)"
  rm -f "${dest}.part"
  die "${MODEL_FILE}: sha256 mismatch (expected ${MODEL_SHA256}, got ${got})"
fi
mv "${dest}.part" "${dest}"
log "model ${MODEL_FILE} downloaded, sha256 verified ($(du -h "${dest}" | cut -f1))"
