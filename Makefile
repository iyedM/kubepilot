SHELL := /usr/bin/env bash

include lab/versions.env

# kube-apiserver --event-ttl for the lab cluster (applied at creation only).
EVENT_TTL ?= 1h
SHELLCHECK_IMAGE ?= koalaman/shellcheck:v0.11.0
PYTHON_IMAGE ?= python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1
LAB_SCRIPTS := $(wildcard lab/scripts/*.sh lab/compat/*.sh lab/apps/*/*.sh)

.PHONY: help tools models verify-pins lab-up lab-check bench-model compat-matrix test-app lab-down lab-stats lint

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

test-app: ## Run the chat-api pytest suite in the pinned Python image
	docker run --rm --user "$$(id -u):$$(id -g)" -e HOME=/tmp -e PYTHONUSERBASE=/tmp/pyuser \
	  -e PYTHONDONTWRITEBYTECODE=1 -v "$(CURDIR):/src:ro" -w /src/lab/apps/chat-api \
	  $(PYTHON_IMAGE) sh -c "pip install --user --quiet --no-cache-dir --disable-pip-version-check \
	    --require-hashes --no-deps -r requirements-dev.txt && python -m pytest -p no:cacheprovider -v tests"
