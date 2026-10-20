#!/usr/bin/env bash
# Helpers shared by the lab-check scripts. Source it after lib.sh.
#
# Services are reached through the API server's service proxy, so no
# port-forward and no extra client image are needed.

# Variables defined here are used by the scripts that source this file.
# shellcheck disable=SC2034

POLL_SECONDS="${POLL_SECONDS:-90}"

passed=0
failed=0
pass() { printf 'PASS  %s\n' "$*"; passed=$((passed + 1)); }
fail() { printf 'FAIL  %s\n' "$*"; failed=$((failed + 1)); }
info() { printf 'INFO  %s\n' "$*"; }

# svc_get NAMESPACE SERVICE:PORT PATH -> response body (GET).
svc_get() {
  kc get --raw "/api/v1/namespaces/$1/services/$2/proxy$3"
}

# svc_post_json NAMESPACE SERVICE:PORT PATH JSON -> response body (POST).
svc_post_json() {
  local body
  body="$(mktemp)"
  printf '%s' "$4" > "${body}"
  kc create --raw "/api/v1/namespaces/$1/services/$2/proxy$3" -f "${body}"
  local rc=$?
  rm -f "${body}"
  return "${rc}"
}

# prom_count EXPR -> number of series returned by an instant query.
prom_count() {
  local q
  q="$(jq -rn --arg q "$1" '$q | @uri')"
  svc_get "${OBS_NAMESPACE}" prometheus-server:80 "/api/v1/query?query=${q}" 2>/dev/null \
    | jq -r '.data.result | length' 2>/dev/null || echo 0
}

# check_series DESCRIPTION EXPR : PASS if at least one series, polling.
check_series() {
  local desc="$1" expr="$2" n=0
  for _ in $(seq 1 $((POLL_SECONDS / 5))); do
    n="$(prom_count "${expr}")"
    [[ "${n}" -gt 0 ]] && break
    sleep 5
  done
  if [[ "${n}" -gt 0 ]]; then
    pass "${desc}: ${n} series for ${expr}"
  else
    fail "${desc}: no series for ${expr} after ${POLL_SECONDS}s"
  fi
}

# check_summary NAME : print the totals, return non-zero if anything failed.
check_summary() {
  echo
  echo "$1: ${passed} passed, ${failed} failed"
  [[ "${failed}" -eq 0 ]]
}
