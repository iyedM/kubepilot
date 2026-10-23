SHELL := /usr/bin/env bash

include lab/versions.env

# kube-apiserver --event-ttl for the lab cluster (applied at creation only).
EVENT_TTL ?= 1h
SHELLCHECK_IMAGE ?= koalaman/shellcheck:v0.11.0
PYTHON_IMAGE ?= python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1
# INHIBIT_SLEEP=1 keeps the machine awake (no suspend, no lid-switch action)
# during long runs; off by default (docs/lab.md, "Suspend during a run").
INHIBIT_SLEEP ?= 0
ifeq ($(INHIBIT_SLEEP),1)
INHIBIT := systemd-inhibit --what=sleep:idle:handle-lid-switch --who=kubepilot --why="KubePilot fault runs" --mode=block
else
INHIBIT :=
endif
LAB_SCRIPTS := $(wildcard lab/scripts/*.sh lab/compat/*.sh lab/apps/*/*.sh lab/faults/*.sh lab/baselines/*.sh lab/ebpf/*.sh)

.PHONY: help tools models verify-pins lab-up lab-check bench-model compat-matrix test test-app eval-score lab-down lab-stats lint fault-cpu-throttle fault-oomkill fault-reset fault-check run-fault run-batch baseline-stability lab-hooks-on lab-hooks-off fault-unschedulable fault-readiness-fail fault-cold-start fault-config-regression fault-host-contention fault-app-delay

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

tools: ## Download pinned kind and kubectl into .tools/bin
	lab/scripts/tools.sh

models: tools ## Download the pinned GGUF model and verify its sha256
	lab/scripts/models.sh

verify-pins: tools ## Check that every pinned version/digest resolves
	lab/scripts/verify-pins.sh

lab-up: ## Create the kind cluster and install the observability stack
	EVENT_TTL=$(EVENT_TTL) lab/scripts/lab-up.sh

lab-check: ## End-to-end check of the lab (runs every check, fails if any failed)
	@rc=0; \
	lab/scripts/check-observability.sh || rc=1; \
	echo; \
	lab/scripts/check-model.sh || rc=1; \
	echo; \
	lab/scripts/check-genai.sh || rc=1; \
	exit $$rc

bench-model: ## Latency benchmark of the model server (BENCH_REQUESTS=50)
	lab/scripts/bench-model.sh

lab-stats: ## Memory/CPU used by the lab node containers
	docker stats --no-stream $$(docker ps --filter label=io.x-k8s.kind.cluster=kubepilot-lab --format '{{.Names}}')

lab-down: tools ## Delete the kind cluster
	.tools/bin/kind delete cluster --name kubepilot-lab --kubeconfig .tools/kubeconfig

lint: ## shellcheck the lab scripts (pinned image)
	docker run --rm -v "$(CURDIR):/src:ro" -w /src $(SHELLCHECK_IMAGE) -x $(LAB_SCRIPTS)

compat-matrix: ## Record what each OpenAI instrumentation package emits (fixtures for P2)
	lab/compat/run-matrix.sh

test: ## Run gofmt check, go vet and Go unit tests with the race detector
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on: $$unformatted" >&2; exit 1; fi
	go vet ./...
	go test -race -v ./...

test-app: ## Run the chat-api pytest suite in the pinned Python image
	docker run --rm --network=host --user "$$(id -u):$$(id -g)" -e HOME=/tmp -e PYTHONUSERBASE=/tmp/pyuser \
	  -e PYTHONDONTWRITEBYTECODE=1 -v "$(CURDIR):/src:ro" -w /src/lab/apps/chat-api \
	  $(PYTHON_IMAGE) sh -c "pip install --user --quiet --no-cache-dir --disable-pip-version-check \
	    --require-hashes --no-deps -r requirements-dev.txt && python -m pytest -p no:cacheprovider -v tests"

eval-score: ## Score completed runs of a split: SPLIT=dev (or SPLIT=test)
	go run ./cmd/eval-score --split $(if $(SPLIT),$(SPLIT),dev)

# --- Fault injection (docs/lab.md, eval/README.md) ---------------------------
fault-cpu-throttle: ## Inject cpu-throttle by hand (VARIANT=limit-300m), split "manual"
	lab/faults/manual.sh inject cpu-throttle $(VARIANT)

fault-oomkill: ## Inject oomkill by hand (VARIANT=noload-400mi), split "manual"
	lab/faults/manual.sh inject oomkill $(VARIANT)

fault-unschedulable: ## Inject unschedulable by hand (VARIANT=nodeselector-gpu), split "manual"
	lab/faults/manual.sh inject unschedulable $(VARIANT)

fault-readiness-fail: ## Inject readiness-fail by hand (VARIANT=probe-path-404), split "manual"
	lab/faults/manual.sh inject readiness-fail $(VARIANT)

fault-cold-start: ## Inject cold-start by hand (VARIANT=model-download-10mbs), split "manual"
	lab/faults/manual.sh inject cold-start $(VARIANT)

fault-config-regression: ## Inject config-regression by hand (VARIANT=threads-1), split "manual"
	lab/faults/manual.sh inject config-regression $(VARIANT)

fault-host-contention: ## Inject host-contention by hand (VARIANT=host-busy-loops-8), split "manual"
	lab/faults/manual.sh inject host-contention $(VARIANT)

fault-app-delay: ## Inject app-delay by hand (VARIANT=delay-2000ms; needs make lab-hooks-on), split "manual"
	lab/faults/manual.sh inject app-delay $(VARIANT)

fault-reset: ## Undo the active manual fault and export its record
	lab/faults/manual.sh reset

fault-check: ## Is the signal of FAULT really present? (FAULT=cpu-throttle)
	lab/faults/check-fault.sh $(FAULT)

baseline-stability: ## Latency spread over 5 quiet 300 s windows (loadgen only; INHIBIT_SLEEP=1 keeps the machine awake)
	$(INHIBIT) lab/faults/baseline-stability.sh

run-fault: ## One recorded run: SPLIT=dev FAULT=cpu-throttle VARIANT=limit-300m REP=1 (INHIBIT_SLEEP=1 keeps the machine awake)
	$(INHIBIT) lab/faults/run.sh $(SPLIT) $(FAULT) $(VARIANT) $(REP)

run-batch: ## Run or resume all runs of a split: SPLIT=dev (INHIBIT_SLEEP=1 holds one sleep lock for the whole batch)
	INHIBIT_SLEEP=$(INHIBIT_SLEEP) lab/faults/run-batch.sh $(SPLIT)

lab-hooks-on: ## Enable chat-api lab fault hooks (redeploys chat-api; between runs only)
	lab/scripts/lab-hooks.sh on

lab-hooks-off: ## Disable chat-api lab fault hooks (redeploys chat-api; between runs only)
	lab/scripts/lab-hooks.sh off
