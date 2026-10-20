#!/usr/bin/env bash
# Regenerate requirements.txt and requirements-dev.txt with hashes, inside the
# same pinned Python image the app runs on, so the lock matches the target
# platform (linux/amd64, CPython 3.12).
set -euo pipefail
cd "$(dirname "$0")"
PYTHON_IMAGE="python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1"
PIP_TOOLS="pip-tools==7.6.2"

docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e PYTHONUSERBASE=/tmp/pyuser \
  -v "${PWD}:/work" -w /work "${PYTHON_IMAGE}" sh -c "
    pip install --user --quiet --no-cache-dir --disable-pip-version-check ${PIP_TOOLS} &&
    /tmp/pyuser/bin/pip-compile --quiet --generate-hashes --allow-unsafe --strip-extras \
      --no-emit-index-url --output-file requirements.txt requirements.in &&
    /tmp/pyuser/bin/pip-compile --quiet --generate-hashes --allow-unsafe --strip-extras \
      --no-emit-index-url --output-file requirements-dev.txt requirements-dev.in"
