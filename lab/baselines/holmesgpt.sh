#!/usr/bin/env bash
# Deploy, evaluate, and tear down HolmesGPT (P3.3 baseline).
#
#   lab/baselines/holmesgpt.sh up     start host LLM + port-forwards + warm cache
#   lab/baselines/holmesgpt.sh eval   run the 12-case evaluation into eval/baselines/holmesgpt/
#   lab/baselines/holmesgpt.sh down   stop host LLM + port-forwards
#
# By default, HolmesGPT uses a dedicated host-side llama-server container
# serving the pinned local GGUF model (.tools/models/qwen2.5-0.5b-instruct-q4_k_m.gguf)
# on 127.0.0.1:18081 so that faults injected into in-cluster genai/model-server
# never break HolmesGPT's own LLM backend. Set HOLMES_MODEL, OPENAI_API_BASE,
# and OPENAI_API_KEY in the environment to override with an external LLM.
#
# jq programs use single quotes on purpose.
# shellcheck disable=SC2016
# shellcheck source=lab/scripts/lib.sh
source "$(dirname "$0")/../scripts/lib.sh"
# shellcheck source=lab/scripts/checklib.sh
source "$(dirname "$0")/../scripts/checklib.sh"
# shellcheck source=lab/faults/lib.sh
source "$(dirname "$0")/../faults/lib.sh"

OUT_DIR="${ROOT_DIR}/eval/baselines/holmesgpt"
STATE_DIR="/tmp/kp-holmes"
LLM_CONTAINER="kp-holmes-llm"
LLM_PORT="18081"
PROM_PORT="19090"
TEMPO_PORT="13200"

HOLMES_MODEL="${HOLMES_MODEL:-openai/qwen2.5-0.5b-instruct}"
HOLMES_API_BASE="${OPENAI_API_BASE:-http://127.0.0.1:${LLM_PORT}/v1}"
HOLMES_API_KEY="${OPENAI_API_KEY:-local-lab}"
HOLMES_MAX_STEPS="${HOLMES_MAX_STEPS:-5}"

ensure_holmes_image() {
  local repo_tag="${HOLMESGPT_IMAGE%%@*}"
  if ! docker image inspect "${repo_tag}" >/dev/null 2>&1; then
    log "pulling ${HOLMESGPT_IMAGE}"
    docker pull --quiet "${HOLMESGPT_IMAGE}" >/dev/null
    docker tag "${HOLMESGPT_IMAGE}" "${repo_tag}"
  fi
}

start_port_forwards() {
  mkdir -p "${STATE_DIR}"
  if ! curl -fsS "http://127.0.0.1:${PROM_PORT}/-/ready" >/dev/null 2>&1; then
    kc -n "${OBS_NAMESPACE}" port-forward svc/prometheus-server "${PROM_PORT}:80" >/dev/null 2>&1 &
    echo $! > "${STATE_DIR}/prom-pf.pid"
  fi
  if ! curl -fsS "http://127.0.0.1:${TEMPO_PORT}/ready" >/dev/null 2>&1; then
    kc -n "${OBS_NAMESPACE}" port-forward svc/tempo "${TEMPO_PORT}:3200" >/dev/null 2>&1 &
    echo $! > "${STATE_DIR}/tempo-pf.pid"
  fi
  sleep 2
}

stop_port_forwards() {
  local pf
  for pf in "${STATE_DIR}/prom-pf.pid" "${STATE_DIR}/tempo-pf.pid"; do
    if [[ -f "${pf}" ]]; then
      kill "$(cat "${pf}")" >/dev/null 2>&1 || true
      rm -f "${pf}"
    fi
  done
  pkill -f "port-forward svc/prometheus-server ${PROM_PORT}:80" >/dev/null 2>&1 || true
  pkill -f "port-forward svc/tempo ${TEMPO_PORT}:3200" >/dev/null 2>&1 || true
}

up() {
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist"
  ensure_holmes_image
  mkdir -p "${STATE_DIR}/holmes-home" "${STATE_DIR}/out"

  if [[ "${HOLMES_API_BASE}" == "http://127.0.0.1:${LLM_PORT}/v1" ]]; then
    if ! curl -fsS "http://127.0.0.1:${LLM_PORT}/health" >/dev/null 2>&1; then
      log "starting host-side llama-server (${LLM_CONTAINER}) on 127.0.0.1:${LLM_PORT} with -c 32768 --jinja"
      docker rm -f "${LLM_CONTAINER}" >/dev/null 2>&1 || true
      docker run -d --name "${LLM_CONTAINER}" --network host \
        -v "${MODELS_DIR}:/models:ro" \
        "${LLAMACPP_IMAGE}" \
        -m "/models/${MODEL_FILE}" \
        --host 127.0.0.1 --port "${LLM_PORT}" -c 32768 -t 8 --jinja >/dev/null
      local deadline=$(( $(date +%s) + 30 ))
      while (( $(date +%s) < deadline )); do
        if curl -fsS "http://127.0.0.1:${LLM_PORT}/health" >/dev/null 2>&1; then
          break
        fi
        sleep 1
      done
      curl -fsS "http://127.0.0.1:${LLM_PORT}/health" >/dev/null \
        || die "host-side llama-server did not become healthy on port ${LLM_PORT}"
    else
      log "reusing running host-side llama-server on 127.0.0.1:${LLM_PORT}"
    fi
  fi

  start_port_forwards

  cat > "${STATE_DIR}/holmes-home/config.yaml" <<EOF
toolsets:
  kubernetes/core:
    enabled: true
  kubernetes/logs:
    enabled: true
  prometheus/metrics:
    enabled: true
    config:
      prometheus_url: http://127.0.0.1:${PROM_PORT}
      tool_calls_return_data: true
  grafana/tempo:
    enabled: true
    config:
      api_url: http://127.0.0.1:${TEMPO_PORT}
  helm/core:
    enabled: false
  internet:
    enabled: false
  connectivity_check:
    enabled: false
  kubernetes/kube-prometheus-stack:
    enabled: false
  bash:
    enabled: false
  skills:
    enabled: false
EOF
  log "HolmesGPT environment ready (model=${HOLMES_MODEL}, api_base=${HOLMES_API_BASE})"
}

down() {
  log "stopping HolmesGPT port-forwards and host-side LLM container"
  stop_port_forwards
  docker rm -f "${LLM_CONTAINER}" >/dev/null 2>&1 || true
  rm -rf "${STATE_DIR}"
  log "HolmesGPT baseline environment cleaned up"
}

matching_run_dir() {
  local cid="$1"
  if [[ -d "${ROOT_DIR}/eval/runs/dev/dev--${cid}--r1" ]]; then
    echo "${ROOT_DIR}/eval/runs/dev/dev--${cid}--r1"
  elif [[ -d "${ROOT_DIR}/eval/runs/test/test--${cid}--r1" ]]; then
    echo "${ROOT_DIR}/eval/runs/test/test--${cid}--r1"
  else
    die "no matching dev/test run directory for ${cid}"
  fi
}

kubepilot_for_case() {
  local rdir
  rdir="$(matching_run_dir "$1")"
  go run "${ROOT_DIR}/cmd/why-slow" --run-dir "${rdir}" --format json 2>/dev/null || echo 'null'
}

# query_holmes CASE_ID FAULT VARIANT EXPECTED TARGET FROM_UTC TO_UTC KP_JSON
query_holmes() {
  local case_id="$1" fault="$2" variant="$3" expected="$4" target="$5"
  local from_utc="$6" to_utc="$7" kp_json="$8"
  local repo_tag="${HOLMESGPT_IMAGE%%@*}"
  local raw_out="${STATE_DIR}/out/${case_id}.raw.json"
  local stdout_log="${STATE_DIR}/out/${case_id}.stdout.log"
  rm -f "${raw_out}" "${stdout_log}"

  start_port_forwards

  local prompt="Investigate namespace genai (deployments chat-api and model-server). Users report that chat completions are currently failing or slow (window ${from_utc} to ${to_utc}). Identify whether there is an active issue in genai, which pod/deployment is affected, and the specific root cause."
  local t0 t1 query_ms rc=0
  t0="$(date +%s%3N)"
  docker run --rm --network host \
    -v "${KUBECONFIG}:/root/.kube/config:ro" \
    -v "${STATE_DIR}/holmes-home:/root/.holmes" \
    -v "${STATE_DIR}/out:/out" \
    -e OPENAI_API_BASE="${HOLMES_API_BASE}" \
    -e OPENAI_API_KEY="${HOLMES_API_KEY}" \
    -e OVERRIDE_MAX_CONTENT_SIZE="32768" \
    -e OVERRIDE_MAX_OUTPUT_TOKEN="1024" \
    "${repo_tag}" \
    ask --model "${HOLMES_MODEL}" \
    --config /root/.holmes/config.yaml \
    --max-steps "${HOLMES_MAX_STEPS}" \
    --no-interactive \
    --json-output-file "/out/${case_id}.raw.json" \
    "${prompt}" > "${stdout_log}" 2>&1 || rc=$?
  t1="$(date +%s%3N)"
  query_ms=$(( t1 - t0 ))

  local holmes_obj
  if [[ -s "${raw_out}" ]]; then
    holmes_obj="$(jq -c \
      --arg img "${HOLMESGPT_IMAGE}" \
      --arg model "${HOLMES_MODEL}" \
      --arg api_base "${HOLMES_API_BASE}" \
      --arg prompt "${prompt}" \
      --argjson qms "${query_ms}" \
      --argjson rc "${rc}" \
      '{
        image: $img,
        model: $model,
        llm_api_base: $api_base,
        exit_code: $rc,
        query_ms: $qms,
        prompt: $prompt,
        result: .result,
        num_llm_calls: .num_llm_calls,
        prompt_tokens: .prompt_tokens,
        completion_tokens: .completion_tokens,
        total_tokens: .total_tokens,
        finish_reason: .finish_reason,
        tool_calls: [.tool_calls[]? | {
          tool_name,
          toolset_name,
          description,
          status: .result.status,
          error: .result.error,
          return_code: .result.return_code,
          invocation: .result.invocation,
          params: .result.params,
          elapsed_seconds: .result.elapsed_seconds,
          data_preview: ((.result.data // "") | tostring | .[0:600])
        }]
      }' "${raw_out}")"
  else
    local err_tail
    err_tail="$(tail -n 30 "${stdout_log}" 2>/dev/null || true)"
    holmes_obj="$(jq -cn \
      --arg img "${HOLMESGPT_IMAGE}" \
      --arg model "${HOLMES_MODEL}" \
      --arg api_base "${HOLMES_API_BASE}" \
      --arg prompt "${prompt}" \
      --arg err "${err_tail}" \
      --argjson qms "${query_ms}" \
      --argjson rc "${rc}" \
      '{
        image: $img,
        model: $model,
        llm_api_base: $api_base,
        exit_code: $rc,
        query_ms: $qms,
        prompt: $prompt,
        result: null,
        error_output: $err,
        num_llm_calls: 0,
        prompt_tokens: 0,
        completion_tokens: 0,
        total_tokens: 0,
        tool_calls: []
      }')"
  fi

  jq -n \
    --arg cid "${case_id}" \
    --arg fault "${fault}" \
    --arg variant "${variant}" \
    --arg expected "${expected}" \
    --arg target "${target}" \
    --arg from_utc "${from_utc}" \
    --arg to_utc "${to_utc}" \
    --argjson holmes "${holmes_obj}" \
    --argjson kp "${kp_json}" \
    '{
      case_id: $cid,
      fault: $fault,
      variant: $variant,
      expected_cause: $expected,
      target_workload: $target,
      window: {
        from_utc: $from_utc,
        to_utc: $to_utc
      },
      holmesgpt: $holmes,
      kubepilot: $kp
    }' > "${OUT_DIR}/${case_id}.json"
  log "recorded ${OUT_DIR#"${ROOT_DIR}/"}/${case_id}.json (HolmesGPT ${query_ms}ms, exit=${rc})"
}

run_case() {
  local case_id="$1" fault="$2" variant="$3" expected="$4" target="$5" wait_s="$6"
  if [[ "${FORCE:-0}" != 1 && -s "${OUT_DIR}/${case_id}.json" ]]; then
    log "skip ${case_id}: ${OUT_DIR#"${ROOT_DIR}/"}/${case_id}.json already exists"
    return 0
  fi

  local kind="fault"
  [[ "${fault}" == "nofault" ]] && kind="nofault"

  local hooks_enabled=0
  if [[ "${fault}" == "app-delay" && "$(chat_api_hooks)" == 0 ]]; then
    log "enabling chat-api lab fault hooks for ${case_id}"
    "${LAB_DIR}/scripts/lab-hooks.sh" on
    hooks_enabled=1
    sleep 10
  fi

  local tmp_dir
  tmp_dir="$(mktemp -d "/tmp/kp-p3-holmes-${case_id}.XXXXXX")"
  init_run "${tmp_dir}" "p3-holmes--${case_id}" manual "${kind}" "${fault}" "${variant}"

  local inject_start reset_start kp_json
  run_set "${tmp_dir}" '.timeline.baseline_start = $t' --arg t "$(date -u -d '-120 seconds' +%Y-%m-%dT%H:%M:%S.000Z)"
  mark "${tmp_dir}" inject_start
  inject_start="$(run_get "${tmp_dir}" '.timeline.inject_start')"

  log "injecting ${case_id} (${fault}/${variant})"
  if ! "${FAULTS_DIR}/${fault}.sh" inject "${tmp_dir}" "${variant}"; then
    "${FAULTS_DIR}/${fault}.sh" reset "${tmp_dir}" || true
    reapply_declared || true
    [[ "${hooks_enabled}" == 1 ]] && "${LAB_DIR}/scripts/lab-hooks.sh" off
    rm -rf "${tmp_dir}"
    die "failed to inject ${case_id}"
  fi
  mark "${tmp_dir}" inject_done

  log "waiting ${wait_s}s for active fault state on ${case_id}"
  sleep "${wait_s}"

  mark "${tmp_dir}" reset_start
  reset_start="$(run_get "${tmp_dir}" '.timeline.reset_start')"

  kp_json="$(kubepilot_for_case "${case_id}")"
  query_holmes "${case_id}" "${fault}" "${variant}" "${expected}" "${target}" \
    "${inject_start}" "${reset_start}" "${kp_json}"

  log "resetting ${case_id}"
  "${FAULTS_DIR}/${fault}.sh" reset "${tmp_dir}" || true
  reapply_declared
  kc -n "${GENAI_NAMESPACE}" delete pvc model-scratch --ignore-not-found --wait=true >/dev/null 2>&1 || true
  if [[ "${hooks_enabled}" == 1 ]]; then
    "${LAB_DIR}/scripts/lab-hooks.sh" off
  fi
  kc -n "${GENAI_NAMESPACE}" rollout status deploy/model-server deploy/chat-api --timeout=90s >/dev/null 2>&1 || true
  wait_chat_ok 180 || log "WARNING: chat-api did not return 200 within 180s after ${case_id}"
  rm -rf "${tmp_dir}"
}

build_summary() {
  python3 - "${OUT_DIR}" <<'PY'
import glob, json, os, sys

out_dir = sys.argv[1]
files = sorted(glob.glob(os.path.join(out_dir, "*.json")))
files = [f for f in files if os.path.basename(f) != "summary.json"]

cases = []
counts = {"pass": 0, "partial": 0, "miss": 0, "false_positive": 0, "healthy_ok": 0}

for path in files:
    with open(path) as f:
        rec = json.load(f)
    cid = rec["case_id"]
    expected = rec["expected_cause"]
    h = rec["holmesgpt"]
    res_text = (h.get("result") or "").lower()
    tcalls = h.get("tool_calls") or []
    tc_names = [t.get("tool_name", "") for t in tcalls]
    tc_statuses = [t.get("status", "") for t in tcalls]
    tc_data = " ".join((t.get("data_preview") or "") for t in tcalls).lower()

    grade = "miss"
    rationale = ""

    # Check if tool calls retrieved relevant evidence or if final answer identified root cause
    if expected == "none":
        # Did HolmesGPT falsely claim a root cause fault on genai?
        if any(w in res_text for w in ("oomkilled", "crashloopbackoff", "throttl", "unschedulable", "readiness probe failed")) and "no issue" not in res_text:
            grade = "false_positive"
            rationale = "Reported a fault on genai during healthy decoy window."
            counts["false_positive"] += 1
        else:
            grade = "healthy_ok"
            rationale = f"Did not report any false-positive root cause on genai (tool_calls={tc_names}, statuses={tc_statuses})."
            counts["healthy_ok"] += 1
    else:
        # Fault cases
        matched_cause = False
        matched_partial = False
        if expected == "cpu_throttling":
            matched_cause = "throttl" in res_text or ("cpu" in res_text and "limit" in res_text and "300m" in res_text)
            matched_partial = "cpu" in res_text or "model-server" in tc_data
        elif expected == "oom_killed":
            matched_cause = "oom" in res_text or "137" in res_text or "out of memory" in res_text
            matched_partial = "crashloop" in res_text or "oom" in tc_data
        elif expected == "unschedulable":
            matched_cause = ("taint" in res_text or "pvc" in res_text or "persistentvolumeclaim" in res_text or "unschedulable" in res_text) and "pending" in res_text
            matched_partial = "pending" in res_text or "unschedulable" in tc_data
        elif expected == "readiness_failing":
            matched_cause = "readiness" in res_text and ("404" in res_text or "600" in res_text or "probe" in res_text)
            matched_partial = "readiness" in res_text or "not ready" in res_text or "readiness" in tc_data
        elif expected == "cold_start":
            matched_cause = ("initcontainer" in res_text or "fetch-model" in res_text or "cold start" in res_text)
            matched_partial = "init" in tc_data or "fetch-model" in tc_data
        elif expected == "config_change":
            matched_cause = ("threads" in res_text or "rollout" in res_text or "config" in res_text) and "model-server" in res_text
            matched_partial = "threads" in tc_data
        elif expected == "host_cpu_contention":
            matched_cause = ("neighbor" in res_text or "contention" in res_text or "pressure" in res_text)
            matched_partial = "cpu-neighbor" in tc_data
        elif expected == "unknown_no_infrastructure_cause":
            matched_cause = ("span" in res_text or "application" in res_text or "pre-inference" in res_text) and "chat-api" in res_text
            matched_partial = False

        if matched_cause:
            grade = "pass"
            rationale = f"Identified expected cause '{expected}' in response (tool_calls={tc_names})."
            counts["pass"] += 1
        elif matched_partial:
            grade = "partial"
            rationale = f"Surfaced partial signal without isolating exact cause '{expected}' (tool_calls={tc_names}, statuses={tc_statuses})."
            counts["partial"] += 1
        else:
            grade = "miss"
            rationale = f"Did not isolate '{expected}' (tool_calls={tc_names}, statuses={tc_statuses}, result_excerpt={(h.get('result') or '')[:160]!r})."
            counts["miss"] += 1

    kp = rec.get("kubepilot") or {}
    kp_top1 = (kp.get("topCauses") or [{}])[0] if isinstance(kp, dict) else {}

    cases.append({
        "case_id": cid,
        "fault": rec["fault"],
        "variant": rec["variant"],
        "expected_cause": expected,
        "target_workload": rec["target_workload"],
        "window": rec["window"],
        "holmesgpt": {
            "model": h.get("model"),
            "query_ms": h.get("query_ms"),
            "num_llm_calls": h.get("num_llm_calls"),
            "prompt_tokens": h.get("prompt_tokens"),
            "completion_tokens": h.get("completion_tokens"),
            "total_tokens": h.get("total_tokens"),
            "tool_calls": [
                {
                    "tool_name": t.get("tool_name"),
                    "toolset_name": t.get("toolset_name"),
                    "status": t.get("status"),
                    "error": t.get("error"),
                    "params": t.get("params")
                }
                for t in tcalls
            ],
            "result": h.get("result"),
            "grade": grade,
            "rationale": rationale
        },
        "kubepilot": {
            "status": kp.get("status") if isinstance(kp, dict) else None,
            "top1_cause": kp_top1.get("cause"),
            "top1_confidence": kp_top1.get("confidence"),
            "top1_component": kp_top1.get("component"),
            "top1_summary": kp_top1.get("summary")
        }
    })

first_h = json.load(open(files[0]))["holmesgpt"] if files else {}
summary = {
    "tool": "HolmesGPT",
    "holmesgpt_image": first_h.get("image"),
    "model": first_h.get("model"),
    "llm_api_base": first_h.get("llm_api_base"),
    "total_cases": len(cases),
    "fault_cases": len([c for c in cases if c["expected_cause"] != "none"]),
    "nofault_cases": len([c for c in cases if c["expected_cause"] == "none"]),
    "counts": counts,
    "cases": cases
}

with open(os.path.join(out_dir, "summary.json"), "w") as f:
    json.dump(summary, f, indent=2)
    f.write("\n")
print(f"Wrote {os.path.join(out_dir, 'summary.json')} ({len(cases)} cases): {counts}")
PY
}

eval_all() {
  mkdir -p "${OUT_DIR}"
  up

  # Case 1: cpu-throttle / limit-300m
  run_case "cpu-throttle--limit-300m" "cpu-throttle" "limit-300m" "cpu_throttling" "deployment/model-server" 15
  # Case 2: oomkill / noload-400mi
  run_case "oomkill--noload-400mi" "oomkill" "noload-400mi" "oom_killed" "deployment/model-server" 15
  # Case 3: oomkill / app-oom-40mi
  run_case "oomkill--app-oom-40mi" "oomkill" "app-oom-40mi" "oom_killed" "deployment/chat-api" 15
  # Case 4: unschedulable / node-taint
  run_case "unschedulable--node-taint" "unschedulable" "node-taint" "unschedulable" "deployment/model-server" 10
  # Case 5: unschedulable / pvc-unbound
  run_case "unschedulable--pvc-unbound" "unschedulable" "pvc-unbound" "unschedulable" "deployment/model-server" 10
  # Case 6: readiness-fail / probe-path-404
  run_case "readiness-fail--probe-path-404" "readiness-fail" "probe-path-404" "readiness_failing" "deployment/model-server" 15
  # Case 7: readiness-fail / initial-delay-600
  run_case "readiness-fail--initial-delay-600" "readiness-fail" "initial-delay-600" "readiness_failing" "deployment/model-server" 15
  # Case 8: cold-start / model-download-10mbs
  run_case "cold-start--model-download-10mbs" "cold-start" "model-download-10mbs" "cold_start" "deployment/model-server" 5
  # Case 9: config-regression / threads-1
  run_case "config-regression--threads-1" "config-regression" "threads-1" "config_change" "deployment/model-server" 15
  # Case 10: host-contention / neighbor-pod-same-node
  run_case "host-contention--neighbor-pod-same-node" "host-contention" "neighbor-pod-same-node" "host_cpu_contention" "deployment/model-server" 15
  # Case 11: app-delay / delay-2000ms
  run_case "app-delay--delay-2000ms" "app-delay" "delay-2000ms" "unknown_no_infrastructure_cause" "deployment/chat-api" 15
  # Case 12: nofault / decoy-same-node-oom
  run_case "nofault--decoy-same-node-oom" "nofault" "decoy-same-node-oom" "none" "none" 10

  build_summary
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  eval) eval_all ;;
  summary) build_summary ;;
  *) die "usage: $0 up | eval | down | summary" ;;
esac
