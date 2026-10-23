# KubePilot

> **Status: early development.** Nothing here is usable yet. There is no
> binary, no Helm chart and no benchmark result. This README describes the
> goal, not shipped features.

KubePilot aims to answer one question: **why was this GenAI request on
Kubernetes slow, or why did it fail?**

Given a trace id (or a time window), it will correlate:

- OpenTelemetry traces carrying `gen_ai.*` spans (from Tempo),
- Kubernetes events and object state,
- Prometheus metrics,
- later, eBPF signals,

and return the **top-3 probable causes**, each with **timestamped evidence**
and a **confidence score**. When the evidence is ambiguous, it answers
**"unknown"** instead of guessing.

## Design principles

- **Deterministic first.** The diagnosis is produced by explainable rules, not
  by an LLM. An LLM may later rephrase an already-evidenced report; it never
  decides the cause.
- **Read-only.** Minimal RBAC, no remediation, no autoscaling.
- **Privacy by default.** Prompt and completion text are not collected from
  traces unless explicitly enabled.
- **Convention-proof.** The OpenTelemetry GenAI semantic conventions are still
  in *Development* status, so every `gen_ai.*` name lives in one versioned
  adapter package. The lab app is instrumented with
  `opentelemetry-instrumentation-genai-openai==1.2b0`, a **pre-release (beta)**
  whose attribute names may still change; what it actually emits is recorded in
  [`docs/genai-conventions.md`](docs/genai-conventions.md).
- **Measured, not claimed.** Accuracy is reported only from a reproducible
  benchmark in [`eval/`](eval/), with its limits. Results will be measured on
  **faults injected in a local lab**, not on real production incidents, and
  on a held-out test set that is not used while writing the rules.

## Roadmap

1. Landscape check: [`docs/landscape.md`](docs/landscape.md)
2. Local lab with reproducible faults ([`lab/`](lab/), [`docs/lab.md`](docs/lab.md))
3. Diagnosis engine and `why-slow` command, with pre-registered benchmark results on injected faults and no-fault windows ([`eval/README.md`](eval/README.md))
4. Comparison against existing tools on the same cases (HolmesGPT, Coroot Community Edition): [`docs/p3-comparison.md`](docs/p3-comparison.md)
5. eBPF evidence evaluation (Grafana Beyla / OpenTelemetry eBPF Instrumentation): [`docs/p4-ebpf-evaluation.md`](docs/p4-ebpf-evaluation.md)
6. In-cluster service, Helm chart, Web UI, read-only operator (`GenAIIncident` CRD), and OpenCost estimation

## Related project

KubePilot builds on ideas from
[kubectl-why-fail](https://github.com/iyedM/kubectl-why-fail), a kubectl
plugin that explains why a pod is failing using deterministic rules.

## License

Apache-2.0, see [LICENSE](LICENSE).
