#!/usr/bin/env bash
# cold-start fault on the model server.
#   inject RUN_DIR VARIANT   apply the variant (see eval/faults/catalog.v1.json)
#   reset  RUN_DIR           nothing specific: the Deployment reset covers it
#   check  RUN_DIR           exit 0 only if a slow start really happened
#
# The effect ends by itself when the new pod becomes Ready: inject records
# that time as fault.applied.effect_end (end of the expected cause).
# jq programs are passed in single quotes to run_set/jq on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/lib.sh"

STORE_URL="http://model-store.modelstore.svc.cluster.local:8000"
MIN_INIT_S=15     # minimum initContainer duration for the download variants
MIN_PULL_S=30     # minimum Pulling -> Pulled gap for image-not-cached

newest_pod() {
  kc -n "${GENAI_NAMESPACE}" get pods -l app.kubernetes.io/name=model-server -o json \
    | jq -r --arg t "${1:0:19}" '[.items[] | select(.metadata.creationTimestamp >= $t)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty'
}

# download_patch CMD: one JSON patch (one rollout) that adds an emptyDir
# cache, an initContainer running CMD to fill it, and loads the model from it.
download_patch() {
  local cmd="$1"
  jq -cn --arg img "${CURL_IMAGE}" --arg cmd "${cmd}" --arg model "/cache/${MODEL_FILE}" '[
    {op: "test", path: "/spec/template/spec/containers/0/args/0", value: "-m"},
    {op: "replace", path: "/spec/template/spec/containers/0/args/1", value: $model},
    {op: "add", path: "/spec/template/spec/volumes/-", value: {name: "model-cache", emptyDir: {}}},
    {op: "add", path: "/spec/template/spec/containers/0/volumeMounts/-", value: {name: "model-cache", mountPath: "/cache"}},
    {op: "add", path: "/spec/template/spec/initContainers", value: [{
      name: "fetch-model", image: $img, command: ["sh", "-c", $cmd],
      volumeMounts: [{name: "model-cache", mountPath: "/cache"}],
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}}]}]'
}

inject() {
  local dir="$1" variant="$2" start cmd pod deadline ready_at
  start="$(run_get "${dir}" '.timeline.inject_start')"
  case "${variant}" in
    model-download-hf)
      # Must come before model-download-*: external download, sha256 checked.
      cmd="curl -fsSL -o /cache/${MODEL_FILE} ${MODEL_URL} && echo '${MODEL_SHA256}  /cache/${MODEL_FILE}' | sha256sum -c -"
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type json -p "$(download_patch "${cmd}")" >/dev/null
      ;;
    model-download-*)
      cmd="curl -fsS --limit-rate $(run_get "${dir}" '.fault.params.rate') -o /cache/${MODEL_FILE} ${STORE_URL}/${MODEL_FILE}"
      kc -n "${GENAI_NAMESPACE}" patch deploy model-server --type json -p "$(download_patch "${cmd}")" >/dev/null
      ;;
    image-not-cached)
      # The image cannot be removed while a container uses it: stop the model
      # server, remove the image from every node (this touches the kind nodes,
      # lab only), then start it again so that the new pod has to pull it.
      local removed=() failed=() node out
      kc -n "${GENAI_NAMESPACE}" scale deploy model-server --replicas=0 >/dev/null
      kc -n "${GENAI_NAMESPACE}" wait --for=delete pod -l app.kubernetes.io/name=model-server --timeout=120s >/dev/null 2>&1 || true
      for node in $("${KIND}" get nodes --name "${CLUSTER_NAME}"); do
        if out="$(docker exec "${node}" crictl rmi "${LLAMACPP_IMAGE}" 2>&1)"; then
          removed+=("${node}")
        else
          failed+=("${node}: ${out//$'\n'/ }")
        fi
      done
      run_set "${dir}" '.fault.applied = {variant: "image-not-cached", rmi_removed_on: $r, rmi_failed_on: $f}' \
        --argjson r "$(printf '%s\n' "${removed[@]}" | jq -R . | jq -sc 'map(select(. != ""))')" \
        --argjson f "$(printf '%s\n' "${failed[@]}" | jq -R . | jq -sc 'map(select(. != ""))')"
      kc -n "${GENAI_NAMESPACE}" scale deploy model-server --replicas=1 >/dev/null
      if (( ${#removed[@]} == 0 )); then
        echo "image-not-cached: crictl rmi removed the image on no node (${failed[*]})" >&2
        return 1
      fi
      ;;
    *) die "unknown cold-start variant ${variant}" ;;
  esac
  run_set "${dir}" '.fault.applied.variant = $v' --arg v "${variant}"
  # Wait until the new pod is Ready: that is when the cold start ends.
  deadline=$(( $(date +%s) + 420 ))
  while (( $(date +%s) < deadline )); do
    pod="$(newest_pod "${start}")"
    if [[ -n "${pod}" ]]; then
      ready_at="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json \
        | jq -r '[.status.conditions[]? | select(.type == "Ready" and .status == "True")] | first | .lastTransitionTime // empty')"
      if [[ -n "${ready_at}" ]]; then
        run_set "${dir}" '.fault.applied.pod = $p | .fault.applied.effect_end = $t' --arg p "${pod}" --arg t "${ready_at}"
        return 0
      fi
    fi
    sleep 3
  done
  note "${dir}" "the new model-server pod did not become Ready within 420s of the injection"
}

reset() { :; }

check() {
  local dir="$1" start variant pod ready_at init_s pull_s before after rc=0
  start="$(run_get "${dir}" '.timeline.inject_start')"
  variant="$(run_get "${dir}" '.fault.variant // empty')"
  pod="$(newest_pod "${start}")"
  if [[ -z "${pod}" ]]; then
    echo "FAIL  no model-server pod created after the injection (${start})"
    return 1
  fi
  ready_at="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json \
    | jq -r '[.status.conditions[]? | select(.type == "Ready" and .status == "True")] | first | .lastTransitionTime // empty')"
  if [[ -n "${ready_at}" ]]; then
    echo "PASS  pod ${pod} became Ready at ${ready_at} ($(( $(to_epoch "${ready_at}") - $(to_epoch "${start}") )) s after the injection)"
  else
    echo "FAIL  pod ${pod} is not Ready"
    rc=1
  fi
  case "${variant}" in
    model-download-*)
      init_s="$(kc -n "${GENAI_NAMESPACE}" get pod "${pod}" -o json | jq -r '
        [.status.initContainerStatuses[]? | select(.name == "fetch-model") | .state.terminated
         | select(.reason == "Completed") | ((.finishedAt | fromdate) - (.startedAt | fromdate))] | first // empty')"
      if [[ -n "${init_s}" && "${init_s}" -ge "${MIN_INIT_S}" ]]; then
        echo "PASS  initContainer fetch-model completed after ${init_s} s (>= ${MIN_INIT_S} s)"
      else
        echo "FAIL  initContainer fetch-model duration '${init_s}' (want a completed run >= ${MIN_INIT_S} s)"
        rc=1
      fi
      ;;
    image-not-cached)
      pull_s="$(events_since "${GENAI_NAMESPACE}" "${pod}" "${start}" | jq -r '
        ([.[] | select(.reason == "Pulling") | .time] | first) as $a | ([.[] | select(.reason == "Pulled") | .time] | last) as $b
        | if $a and $b then (($b | fromdate) - ($a | fromdate)) else empty end')"
      if [[ -n "${pull_s}" && "${pull_s}" -ge "${MIN_PULL_S}" ]]; then
        echo "PASS  image pulled in ${pull_s} s (Pulling -> Pulled, >= ${MIN_PULL_S} s)"
      else
        echo "FAIL  image pull duration '${pull_s}' (want >= ${MIN_PULL_S} s)"
        rc=1
      fi
      ;;
    *)
      echo "FAIL  no cold-start injection recorded in this run"
      rc=1
      ;;
  esac
  if [[ -n "${ready_at}" ]]; then
    before="$(loadgen_window "${start}" "${ready_at}" | jq '[.[] | .errors] | add // 0')"
    after="$(loadgen_window "${ready_at}" "$(now_ms)" | jq '[.[] | .ok] | add // 0')"
    if [[ "${before}" -ge 1 && "${after}" -ge 1 ]]; then
      echo "PASS  load generator: ${before} failed requests while starting, ${after} successful after Ready"
    else
      echo "FAIL  load generator: ${before} failed while starting, ${after} successful after Ready (want >= 1 each)"
      rc=1
    fi
  fi
  return "${rc}"
}

case "${1:-}" in
  inject) inject "$2" "$3" ;;
  reset) reset "$2" ;;
  check) check "$2" ;;
  *) die "usage: $0 inject RUN_DIR VARIANT | reset RUN_DIR | check RUN_DIR" ;;
esac
