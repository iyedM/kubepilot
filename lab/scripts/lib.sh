#!/usr/bin/env bash
# Shared settings for the lab scripts. Source it, do not execute it.
#
# The lab never touches ~/.kube/config: kind writes its kubeconfig to
# .tools/kubeconfig and every kubectl/helm call uses it explicitly.

# Variables defined here are used by the scripts that source this file.
# shellcheck disable=SC2034

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAB_DIR="${ROOT_DIR}/lab"
TOOLS_DIR="${ROOT_DIR}/.tools"
BIN_DIR="${TOOLS_DIR}/bin"

# shellcheck source=lab/versions.env
source "${LAB_DIR}/versions.env"

CLUSTER_NAME="kubepilot-lab"
OBS_NAMESPACE="observability"
export KUBECONFIG="${TOOLS_DIR}/kubeconfig"

KIND="${BIN_DIR}/kind"
KUBECTL="${BIN_DIR}/kubectl"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

kc() { "${KUBECTL}" --context "kind-${CLUSTER_NAME}" "$@"; }
hm() { helm --kube-context "kind-${CLUSTER_NAME}" "$@"; }

cluster_exists() {
  "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"
}
