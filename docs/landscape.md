# Landscape check (P0)

- **Date of the check:** 2026-10-19
- **Question:** does any existing tool already do **deterministic correlation
  between a slow/failed GenAI trace (`gen_ai.*` spans) and Kubernetes
  events/metrics, with timestamped evidence and an "unknown" answer**?
- **Method:** GitHub REST API for license and activity (numbers below were
  fetched on the date above), official docs and READMEs for capabilities, a
  few secondary sources where marked. **No tool was installed or run** for
  this check; capability statements come from documentation, not from hands-on
  testing.
- **Marker:** statements about proprietary products (Datadog, Dynatrace,
  Komodor, Causely) that come from the vendor's own documentation, blog or
  press material are tagged *(vendor claim, untested)*.

## Summary

| Tool | What it is | License | Activity (2026-10-19) | Diagnosis engine | GenAI-trace ↔ K8s correlation |
|---|---|---|---|---|---|
| K8sGPT | CLI/operator scanning K8s objects, LLM explains findings | Apache-2.0 | 8.3k★, v0.4.40 (2026-10-09), 93 commits / 90 d | Rule-based analyzers per K8s kind + LLM explanation | No: no trace analyzer found |
| HolmesGPT | Agentic SRE assistant, CNCF Sandbox | Apache-2.0 | 3.6k★, 0.45.0 (2026-10-15), ≥100 commits / 90 d | LLM agent loop calling toolsets (incl. Tempo, Prometheus, K8s) | Possible ad hoc via the agent; not deterministic, not GenAI-specific |
| kagent | Framework to run AI agents in K8s, CNCF Sandbox | Apache-2.0 | 4.1k★, v1.0.0-alpha15 (2026-10-19), ≥100 commits / 90 d | LLM agents + MCP tools (K8s, Prometheus, Grafana…) | No dedicated feature; you would build an agent |
| Coroot | eBPF-based observability/APM with RCA | Apache-2.0 (Community); AI RCA is Enterprise/Cloud | 8.0k★, v1.27.4 (2026-10-15), ≥100 commits / 90 d | Deterministic/ML over dependency graph; LLM only explains findings | Closest architecture, but no GenAI/`gen_ai` focus found |
| Pixie | eBPF auto-telemetry for K8s, CNCF Sandbox | Apache-2.0 | 6.5k★, low activity (9 commits / 90 d), latest GitHub "release" 2025-01 | No RCA; scriptable queries (PxL) | No |
| Grafana Beyla / OBI | eBPF auto-instrumentation (Beyla = Grafana distribution of OTel eBPF Instrumentation) | Apache-2.0 | Beyla 2.1k★, v3.38.0 (2026-10-05); OBI v0.15.2 (2026-10-16) | None (instrumentation only) | No: produces spans, does not diagnose |
| Grafana Cloud AI Observability | Hosted dashboards for LLM apps (OpenLIT-based) | Proprietary SaaS (OpenLIT itself is open source) | n/a | Dashboards, no RCA described | No |
| Datadog LLM Observability (+ Watchdog RCA) | Proprietary SaaS | Proprietary | n/a | Watchdog RCA (ML, generic); LLM spans linked to APM spans *(vendor claim, untested)* | Partial: LLM ↔ APM navigation is documented *(vendor claim, untested)*; automated LLM-trace ↔ K8s RCA not found in docs |

**Verdict (see §Conclusion):** no tool found that does the exact thing
KubePilot targets. The niche looks **open but narrow**, with Coroot and
HolmesGPT as the most credible "could do it next" threats.

## Tool by tool

### K8sGPT

- Scans the cluster with analyzers written per resource kind (Pod, Deployment,
  Node, PVC, HPA, Service, Ingress, Gateway, webhooks…, see `pkg/analyzer` in
  the repo) and sends the findings to an LLM backend for explanation.
- Integrations exist (Trivy, Prometheus). The Prometheus integration adds
  config-validation analyzers (`PrometheusConfigValidate`,
  `PrometheusConfigRelabelReport`) per a secondary source, i.e. it checks the
  Prometheus setup, not application latency.
- No analyzer consumes traces. **Not trace-driven, not GenAI-specific.**
- Sources: [repo](https://github.com/k8sgpt-ai/k8sgpt),
  [analyzer directory](https://github.com/k8sgpt-ai/k8sgpt/tree/main/pkg/analyzer),
  [filters docs](https://docs.k8sgpt.ai/reference/cli/filters),
  [Palark overview (secondary)](https://palark.com/blog/chatgpt-tools-for-kubernetes/).

### HolmesGPT

- Agentic loop: an LLM chooses which tools to call, queries live data, refines
  hypotheses. Toolsets include **Prometheus** (PromQL), **Tempo** (find traces
  above a duration in a time range, fetch a trace by id), Kubernetes, Loki,
  Datadog, Grafana and many more.
- Claims read-only access respecting RBAC; an optional remediation toolset can
  scale/rollback/edit resources.
- Requires an LLM provider (OpenAI, Anthropic, Azure, Bedrock, Gemini…).
- The repo has an LLM eval harness (`tests/llm`, `generate_eval_report.py`);
  this check did **not** find a published accuracy number on GenAI latency
  incidents.
- **Relevance:** it is the natural baseline for P3 because it already has the
  Tempo + Prometheus + K8s toolsets. It differs from KubePilot in that the
  diagnosis is produced by an LLM (non-deterministic, needs an API key or a
  local model, cost per investigation) and it has no GenAI-specific signal
  model (TTFT, tokens/s, model-server queueing).
- Sources: [repo](https://github.com/HolmesGPT/holmesgpt),
  [Tempo toolset docs](https://docs.robusta.dev/master/configuration/holmesgpt/toolsets/grafanatempo.html),
  [CNCF blog, 2026-01-07](https://www.cncf.io/blog/2026/01/07/holmesgpt-agentic-troubleshooting-built-for-the-cloud-native-era/).

### kagent

- Framework (CRDs, UI, CLI) to define and run LLM agents inside Kubernetes.
  Ships an MCP server with tools for Kubernetes, Istio, Helm, Argo,
  Prometheus, Grafana, Cilium. Traces its *own* agents with OpenTelemetry.
- It is a platform for building agents, not a diagnosis product. A user could
  build a "why is my LLM slow" agent with it, but that agent would be
  LLM-driven.
- Sources: [repo README](https://github.com/kagent-dev/kagent),
  [what is kagent](https://kagent.dev/docs/kagent/introduction/what-is-kagent).

### Coroot

- Observability platform using eBPF plus OpenTelemetry (metrics, logs, traces,
  profiles), with service maps and SLO-based inspections.
- Its AI RCA is the **closest architectural match**: the analysis walks the
  dependency graph and compares telemetry with ML algorithms, **without an
  LLM**; the LLM is only used afterwards ("Explain with AI") on the findings.
- Limits relative to KubePilot's goal: AI RCA is an **Enterprise / Coroot Cloud**
  feature (Community Edition: 10 free investigations/month through Coroot
  Cloud); documentation found **no GenAI-specific analysis** (no mention of
  `gen_ai` spans, TTFT or model servers).
- Sources: [AI overview docs](https://docs.coroot.com/ai/overview),
  [blog: AI-powered RCA](https://coroot.com/blog/we-built-ai-powered-root-cause-analysis-that-actually-works/),
  [InfoQ, 2026-07](https://infoq.com/news/2026/07/ai-rca-context-engineering),
  [repo](https://github.com/coroot/coroot).

### Pixie

- eBPF-based, in-cluster telemetry with a query language (PxL). No root-cause
  engine.
- Repository is still receiving commits (most recent 2026-10-19) but at low
  volume (9 commits in 90 days); the latest object published as a GitHub
  "release" is from 2025-01 (vizier tags exist after that).
- Sources: [repo](https://github.com/pixie-io/pixie).

### Grafana Beyla / OpenTelemetry eBPF Instrumentation (OBI)

- Beyla is Grafana's distribution of OBI (code donated to OpenTelemetry in
  2025). Grafana docs state OBI can generate spans from GenAI clients, and
  that Grafana AI Observability needs extra collector-side enrichment.
- Instrumentation only: it **produces** the signals KubePilot consumes; it does
  not diagnose. Potentially useful later as a zero-code source of spans.
- Sources: [Beyla vs OBI docs](https://grafana.com/docs/beyla/latest/obi/),
  [Grafana agent observability with OTel](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/opentelemetry/),
  [Beyla repo](https://github.com/grafana/beyla),
  [OBI repo](https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation).

### Grafana Cloud AI Observability

- Hosted solution based on OpenLIT instrumentation (including an OpenLIT
  Operator for zero-code injection on Kubernetes). Bundled dashboards: GenAI
  Observability, Evaluations, VectorDB, MCP, GPU monitoring. Tracks tokens,
  cost, latency.
- Dashboards, not automated cause analysis. Proprietary SaaS.
- Sources: [setup docs](https://grafana.com/docs/grafana-cloud/monitor-applications/ai-observability/setup),
  [zero-code on Kubernetes](https://grafana.com/blog/ai-observability-zero-code),
  [OpenLIT + OTel blog](https://grafana.com/blog/ai-observability-llms-in-production).

### Datadog LLM Observability and Watchdog RCA

- LLM Observability is built on the APM tracer; LLM spans and APM spans are
  linked and navigable both ways. *(vendor claim, untested)*
- Watchdog RCA (GA) infers causal chains between application and
  infrastructure anomalies, including Kubernetes. *(vendor claim, untested)*
- Datadog's own vLLM integration post (2024-11-22) states the intent to
  correlate model-server metrics with infrastructure: *"By correlating LLM
  performance data with the infrastructure metrics that you're already
  collecting in Datadog"*, with the example *"Datadog can help you trace the
  issue back to its root cause, such as GPU saturation."* *(vendor claim, untested)* That post is about
  vLLM **metrics** and dashboards; it does not tie the correlation to LLM
  Observability **traces** or describe an automated causal ranking.
- This check **did not find** documentation showing an automated RCA that
  starts from an LLM span and explains it with Kubernetes events. It may exist
  in some form (Datadog also markets AI SRE features that were not reviewed
  here); treat this row as **unverified**. In any case it is proprietary SaaS,
  not self-hosted.
- Sources: [LLM Observability and APM](https://docs.datadoghq.com/llm_observability/guide/llm_observability_and_apm),
  [Watchdog RCA blog](https://datadoghq.com/blog/datadog-watchdog-automated-root-cause-analysis),
  [vLLM integration blog](https://www.datadoghq.com/blog/vllm-integration/).

## Adjacent work worth knowing

- **TriFleetRCA** (arXiv, 2026-09-20): on-premise RCA for Kubernetes from pod
  logs, events and cluster objects, using a local LLM (Qwen2.5-14B) to produce
  a root cause with evidence lines; the authors say they release their fault
  injector. Not trace-based, not GenAI-workload-specific, LLM in the diagnosis.
  Useful as related work and possibly for fault-injection ideas.
  [arXiv 2609.23766](https://arxiv.org/abs/2609.23766)
- **OpenTelemetry GenAI semantic conventions** moved to a dedicated repository,
  [open-telemetry/semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai)
  (created 2026-05-05). In `docs/gen-ai/gen-ai-metrics.md` every metric is
  still marked **Development**, and on **2026-10-06** a commit replaced
  `gen_ai.client.operation.duration` by `gen_ai.client.inference.duration` for
  inference operations. This is concrete evidence for rule 5 (single versioned
  adapter for `gen_ai.*` names).
- **LLM observability backends** (Langfuse, Arize Phoenix, OpenLIT, Traceloop
  OpenLLMetry) collect and display GenAI traces; none was found to correlate
  them with Kubernetes events. Not reviewed in depth.

## Conclusion

1. **Nobody found does the exact combination**: GenAI-trace-centric +
   Kubernetes events/metrics + deterministic rules + top-3 with evidence +
   explicit "unknown" + self-hosted open source.
2. **Closest threats (subsequently measured hands-on in [`docs/p3-comparison.md`](p3-comparison.md)):**
   - *Coroot* already has deterministic, LLM-free RCA with eBPF. If it adds
     GenAI-specific inspections, it overlaps strongly. Its RCA is not in the
     free edition today. (Hands-on P3 evaluation of Coroot CE `v1.28.1`:
     [`docs/p3-comparison.md`](p3-comparison.md).)
   - *HolmesGPT* can reach the same data (Tempo, Prometheus, K8s) today, but
     through an LLM agent. KubePilot's argument against it must be
     **measured** (P3), not asserted: reproducibility, cost, latency and
     accuracy on the same cases. (Hands-on P3 evaluation of HolmesGPT `v0.45.0`:
     [`docs/p3-comparison.md`](p3-comparison.md).)
   - *Dynatrace Davis* (not reviewed in depth) claims causal RCA that
     includes LLM telemetry *(vendor claim, untested)*. It is the main commercial reference point, but it
     is proprietary and not self-hosted open source.
3. **What KubePilot must bring to be worth existing:**
   - GenAI-specific signal model (TTFT, inter-token latency, token counts,
     model-server queueing, cold start) mapped to K8s causes.
   - A public, reproducible benchmark with fault injection and no-fault
     windows, which none of the reviewed tools publishes for this scenario.
   - Honest "unknown" behaviour, measured as false-positive rate.
4. **Risk to keep in mind:** the niche is narrow. For portfolio value this is
   fine; for adoption, the tool should also be useful on non-GenAI slow traces
   later, but that is out of scope until P2's exit criterion is met.

## Limits of this check

- Capabilities come from docs and READMEs; nothing was installed or run.
- Datadog's and Grafana's commercial offerings change quickly and were only
  partly reviewed.
- Activity counts use the GitHub API; "≥100" means the first page (100 items)
  was full, so the real number may be higher.
- Commercial AIOps/RCA products below were **not reviewed**; one line each
  from vendor material, not verified:
  - **Komodor (Klaudia):** proprietary AI SRE agent inside Komodor's K8s
    platform; combines ML with Komodor's history of changes, events and
    metrics; launch coverage says it was built on an LLM hosted on Amazon
    Bedrock. *(vendor claim, untested)* [Klaudia overview](https://help.komodor.com/hc/en-us/articles/22447540595218)
  - **Causely:** proprietary "causal AI" platform for Kubernetes
    applications, with Grafana (via Beyla) and Datadog Watchdog integrations
    and an MCP server (2025-11). *(vendor claim, untested)*
    [Datadog integration page](https://docs.datadoghq.com/integrations/causely/),
    [MCP server press release](https://www.businesswire.com/news/home/20251106134583/en/Causely-Introduces-MCP-Server-for-Automated-Remediation-Across-Kubernetes-and-Application-Code)
  - **Dynatrace Davis AI:** proprietary causal engine over Smartscape
    topology, with a generative layer on top; Dynatrace says it connects LLM
    telemetry to its causal topology *(vendor claim, untested)*. Likely the strongest commercial
    "deterministic RCA + LLM telemetry" offering; not self-hosted open source.
    [Dynatrace AI](https://dynatrace.com/ai),
    [TechTarget analysis](https://www.techtarget.com/searchapparchitecture/tip/Dynatrace-bets-on-causal-intelligence-for-AI-observability)
