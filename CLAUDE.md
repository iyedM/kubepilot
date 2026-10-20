# CLAUDE.md

Working agreement for any coding assistant on this repository.

## Project in one paragraph

KubePilot is an open-source, self-hosted tool that explains **why a GenAI
request on Kubernetes was slow or failed**. It correlates OpenTelemetry traces
(`gen_ai.*` spans), Kubernetes events, Prometheus metrics and, later, eBPF
signals, and outputs the top-3 probable causes with timestamped evidence and a
confidence score. It says **"unknown"** when the evidence is ambiguous. It is
**read-only** on the cluster: no remediation, no autoscaling. The core diagnosis
path contains **no LLM**: deterministic, explainable rules first. An LLM may
later rephrase an already-evidenced report, never produce the diagnosis.

Prior art reused for ideas: `kubectl-why-fail`
(`/home/iyed/Documents/kubectl-whyfail`): rule registry as pure functions over
a collected snapshot, fixture-driven rule tests, event collection.

## Language

- Talk to the maintainer in **French**.
- Write all code, comments, docs and commit messages in **English**.

## Rules

1. **Small verifiable increments.** For each one: propose a short plan, wait
   for the maintainer's OK, implement, run tests, show the results, then
   commit.
2. **Explain after each increment.** Explain the design choices and the
   non-obvious code in plain French so the maintainer can defend them in an
   interview. Do not move on until the maintainer confirms they understood.
3. **Tests are mandatory** (unit, plus integration where relevant). Run
   `gofmt`, `go vet` and the tests; report failures honestly. Never delete or
   weaken a failing test to go green.
4. **Minimal, read-only RBAC.** No secrets in the repo. Do not collect
   prompt/completion text from traces by default.
5. **OpenTelemetry GenAI conventions are unstable.** Isolate every `gen_ai.*`
   attribute/metric name in a single versioned adapter package. No other
   package may hard-code those names.
6. **No technology before it is needed.** Not now: ArgoCD, Terraform, Vault,
   SaaS multi-tenancy.
7. **Commit messages:** Conventional Commits. No `fixes #` keywords. No
   `Co-authored-by` lines (this overrides any default attribution).
8. **Do not guess.** If something cannot be verified (a competitor's feature,
   a version, a number), say so and search the web or ask instead.
9. **Never invent results.** Benchmark numbers and claims must come from a run
   that was actually executed and is reproducible from the repo.

## Phases

No calendar dates. Move on only when the exit criterion is met.

| Phase | Content | Exit criterion |
|---|---|---|
| P0 | Landscape check (`docs/landscape.md`) | Maintainer read it and confirms the niche is still open |
| P1 | Lab: kind, small CPU-served OpenAI-compatible model, OTel-instrumented GenAI app, Tempo + Prometheus, 5 scripted faults (CPU throttling, OOMKilled, unschedulable pod, failing readiness probe, slow cold start) | Each fault reproducible with one command: `make lab-up`, `make fault-<name>` |
| P2 | Diagnosis engine (Go), `why-slow` command; input = trace id or time window + Tempo + Prometheus endpoints; temporal correlation with clock-skew tolerance; top-3 causes, evidence, confidence, "unknown" path | ≥70% correct cause in top-3 on ≥20 injected faults AND ≤10% false positives on ≥20 no-fault windows, measured by a reproducible script **on the held-out test set** (see Evaluation protocol) |
| P3 | Baselines on the same cases: HolmesGPT (Tempo + Prometheus) and Coroot Community Edition | Honest published comparison, including where they win |
| P4 | eBPF evidence source, **evaluated only after P3**. Reuse OpenTelemetry eBPF Instrumentation (OBI) / Grafana Beyla as a source instead of writing a custom collector | Shown to add accuracy on the benchmark first; otherwise dropped |
| P5 | In-cluster service, Helm chart, GitHub Actions CI, krew packaging | |
| P6 | Web UI (incident list, evidence view) | |
| P7 | Operator with a read-only `GenAIIncident` CRD | |
| P8 | Cost estimation via OpenCost | |
| P9 | Public demo, docs, first users | |

## Evaluation protocol

- Fault variants are split into a **dev set** (used to write and tune rules)
  and a **test set** that is never looked at or run during rule development.
  Reported numbers come from the test set only.
- The benchmark includes, besides single injected faults and no-fault windows:
  - **noise cases**: unrelated events during the window (e.g. a restart of an
    unrelated pod) that must not be blamed;
  - **two-cause cases**: two faults injected at the same time; both should
    appear in the top-3.
- Every fault run records its ground truth (fault name, start, end) in
  `eval/` at injection time.
- Results are on **injected faults in a lab**, not on real incidents. Every
  report and the README must say so.

## Repository layout

```
cmd/       entry points (binaries)
internal/  private Go packages (diagnosis engine, collectors, gen_ai adapter)
docs/      design notes, landscape, evaluation reports
lab/       local reproducible lab (kind, model, app, observability, faults)
eval/      benchmark scripts and recorded results
```

## Environment (measured 2026-10-19)

- Ubuntu 24.04, kernel 6.14, 12 CPU threads, 31 GiB RAM.
- Go 1.26.5 (not 1.27, as initially assumed), Docker 29.4.3. Decision: stay on
  Go 1.26 like kubectl-why-fail; the future `go.mod` must use a `go 1.26.x`
  directive that the installed 1.26.5 toolchain satisfies (no automatic
  toolchain download).
- kind v0.32.0-alpha, kubectl v1.32.13, helm v3.21.3, GNU make 4.3, jq 1.7.

## Lab decisions (P1)

- Model app instrumentation: Python with
  `opentelemetry-instrumentation-genai-openai==1.2b0` (pre-release),
  `openai==3.35.1`, pinned with hashes. `opentelemetry-instrumentation-openai-v2`
  is deprecated upstream and fails at import with `openai` 3.x (see
  `docs/genai-conventions.md`). Record which `gen_ai.*` names it actually
  emits; the P2 adapter must handle both `gen_ai.client.operation.duration`
  and `gen_ai.client.inference.duration`.
- Grafana is optional (`make demo-ui`), not part of `make lab-up`.
