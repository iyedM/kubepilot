#!/usr/bin/env bash
# lab-hooks.sh on|off: switch the lab-only fault hooks of chat-api
# (LAB_FAULT_HOOKS, used by the app-delay fault).
#
# This REDEPLOYS chat-api (rolling update of its Deployment). It is refused
# while a fault run is active or holds the fault lock: hooks are switched
# between runs only, so a switch never shows up inside a recorded window.
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/../faults/lib.sh"

case "${1:-}" in
  on) value=1 ;;
  off) value=0 ;;
  *) die "usage: $0 on|off" ;;
esac

mkdir -p "${TOOLS_DIR}"
exec 9>"${TOOLS_DIR}/fault.lock"
flock -n 9 || die "a fault run holds ${TOOLS_DIR}/fault.lock: switch hooks between runs only"
[[ -s "${ACTIVE_RUN}" ]] && die "a fault is active ($(cat "${ACTIVE_RUN}")): switch hooks between runs only"

if [[ "$(chat_api_hooks)" == "${value}" ]]; then
  log "LAB_FAULT_HOOKS is already ${value}"
  exit 0
fi
CHAT_API_IMAGE="$(kc -n "${GENAI_NAMESPACE}" get deploy chat-api -o jsonpath='{.spec.template.spec.containers[0].image}')"
LAB_FAULT_HOOKS="${value}"
export CHAT_API_IMAGE LAB_FAULT_HOOKS
log "setting LAB_FAULT_HOOKS=${value}: this redeploys chat-api"
# shellcheck disable=SC2016
envsubst '${CHAT_API_IMAGE} ${LAB_FAULT_HOOKS}' < "${LAB_DIR}/genai/chat-api.yaml" | kc apply -f -
kc -n "${GENAI_NAMESPACE}" rollout status deploy/chat-api --timeout=3m
