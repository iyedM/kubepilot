# GenAI telemetry conventions: what is actually emitted

The OpenTelemetry GenAI semantic conventions are in **Development** status
and change often (see [landscape.md](landscape.md)). This page records what
the instrumentation used by the lab **really emits**, measured by
`make compat-matrix` (`lab/compat/`), and the decisions taken from it. The
raw telemetry is kept in
[`eval/fixtures/genai-instrumentation/`](../eval/fixtures/genai-instrumentation/)
as fixtures for the P2 adapter.

Measured on 2026-10-20 against the lab's llama-server (build 11723, commit
31288c9c6), Python 3.12, `opentelemetry-sdk` 1.45.1.

## Instrumentation package used by the lab

`opentelemetry-instrumentation-genai-openai==1.2b0`, with `openai==3.35.1`
and `opentelemetry-util-genai==1.2b0`.

> **Pre-release (beta).** Attribute and metric names may still change in a
> later release. Versions are pinned exactly, with hashes; any upgrade must
> re-run `make compat-matrix` and update this page.

It replaces `opentelemetry-instrumentation-openai-v2`, which upstream
deprecated on 2026-08-01 (security fixes only; PR
open-telemetry/opentelemetry-python-contrib#4904). The new package lives in
`open-telemetry/opentelemetry-python-genai`, uses the latest GenAI conventions
by default (no `OTEL_SEMCONV_STABILITY_OPT_IN` needed) and declares support
for `openai>=1.26,<4`.

## Compatibility matrix

| Case | Packages | Conventions mode | Result |
|---|---|---|---|
| A1 | `openai-v2` 2.4b0 + `openai` 3.35.1 | v1.30.0 (package default) | **fails at import** |
| A2 | same + `OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental` | latest (opt-in) | **fails at import** |
| B | `openai-v2` 2.4b0 + `openai` 2.54.0 | v1.30.0 (package default) | **fails at import** |
| C | `genai-openai` 1.2b0 + `openai` 3.35.1 | latest (package default) | works: 2 spans, 2 metrics, no error |
| C-capture | C + `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=span_and_event` | latest | works; content captured (privacy positive control) |

Exact causes of the failures (full logs in the fixtures):

- **A1, A2**: `openai_v2/utils.py` line 23 does `from httpx import URL`, but
  `openai` 3.x depends on `httpx2`, not `httpx`:
  `ModuleNotFoundError: No module named 'httpx'`.
- **B**: `openai-v2` 2.4b0 declares `opentelemetry-util-genai>=0.4b0.dev`
  (open range); pip installs util-genai 1.2b0, which no longer has the module
  it imports:
  `ModuleNotFoundError: No module named 'opentelemetry.util.genai.instruments'`.

These failures are why the lab does not use `openai-v2`. Recording fixtures
in the old v1.30.0 convention (pinning util-genai 0.4b0) was deliberately not
done yet; it will be if the P2 adapter needs them.

## Names emitted by `genai-openai` 1.2b0 (case C)

**Span** `chat {gen_ai.request.model}`, kind `CLIENT`:

| Attribute | Example |
|---|---|
| `gen_ai.operation.name` | `chat` |
| `gen_ai.provider.name` | `openai` (see note a) |
| `gen_ai.request.model` / `gen_ai.response.model` | `qwen2.5-0.5b-instruct` |
| `gen_ai.request.max_tokens`, `gen_ai.request.temperature` | `16`, `0` |
| `gen_ai.response.id`, `gen_ai.response.finish_reasons` | `chatcmpl-…`, `["length"]` |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | `47`, `16` |
| `gen_ai.usage.cache_read.input_tokens` | `24` (see note d) |
| `openai.response.system_fingerprint` | `b11723-31288c9c6` (see note b) |
| `server.address`, `server.port` | `kp-compat-llama`, `8080` |

A failed call gives the same span with status `ERROR` and
`error.type=openai.APIConnectionError` (no usage attributes).

**Metrics:**

| Name | Instrument | Unit | Data point attributes |
|---|---|---|---|
| `gen_ai.client.operation.duration` | histogram | `s` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `server.address`, `server.port`, `openai.response.system_fingerprint`, `error.type` (on failures) |
| `gen_ai.client.token.usage` | histogram | `{token}` | same, plus `gen_ai.token.type` |

**Logs/events:** none with the default settings.

## In the lab: Prometheus series and trace resource

Metrics go app → OTLP → collector (`k8sattributes`) → Prometheus OTLP
receiver. Series names actually present in Prometheus (v3.15.0, default
translation strategy), recorded on 2026-10-20:

| OTLP metric | Prometheus series |
|---|---|
| `gen_ai.client.operation.duration` (unit `s`) | `gen_ai_client_operation_duration_seconds_bucket`, `_count`, `_sum` |
| `gen_ai.client.token.usage` (unit `{token}`) | `gen_ai_client_token_usage_bucket`, `_count`, `_sum` (the `{token}` annotation adds no suffix) |
| `http.server.duration` (FastAPI instrumentation) | `http_server_duration_milliseconds_*` (older HTTP semantic conventions, unit `ms`) |

Labels on a GenAI series: attributes become labels with dots replaced by
underscores (`gen_ai_operation_name`, `gen_ai_request_model`,
`gen_ai_response_model`, `gen_ai_provider_name`, `server_address`,
`server_port`, `openai_response_system_fingerprint`, `error_type` on
failures), plus the promoted resource attributes `k8s_pod_name`,
`k8s_namespace_name`, `k8s_node_name`, `k8s_deployment_name`,
`service_name`. `job` is `<service.namespace>/<service.name>`
(`genai/chat-api`) and `instance` is `service.instance.id`.

Trace resource of a chat-api span in Tempo: the app sets `service.name`,
`k8s.pod.name`, `k8s.pod.uid`, `k8s.namespace.name`, `k8s.node.name`
(Downward API); the collector's `k8sattributes` adds, among others,
`k8s.deployment.name`, `k8s.replicaset.name`, `k8s.container.name`,
`k8s.pod.start_time`, `container.image.name`, `service.namespace`,
`service.version` (the image tag) and `service.instance.id`.

## Notes for the P2 adapter

a. **`gen_ai.provider.name=openai` describes the SDK, not the server.** The
   lab's server is llama.cpp, yet the attribute says `openai`. The adapter must
   identify the serving backend from `server.address`, `server.port` and
   `gen_ai.response.model`, never from `gen_ai.provider.name`.

b. **`openai.response.system_fingerprint` carries the llama.cpp build**
   (`b11723-31288c9c6`): useful evidence to correlate a regression with a
   server version change.

c. **Duration metric name drift.** 1.2b0 emits
   `gen_ai.client.operation.duration`. The semantic-conventions repository
   renamed it to `gen_ai.client.inference.duration` for inference operations
   on 2026-10-06, after this release (2026-09-24). The adapter must accept
   both names.

d. **Prompt cache affects token counts.** The probe did not disable llama.cpp's
   prompt cache, and the span reported `gen_ai.usage.cache_read.input_tokens=24`.
   The lab app therefore sends `cache_prompt: false` (through the SDK's
   `extra_body`) so that input token counts and latencies are not skewed by
   cache hits.

## Privacy: where content appears when capture is enabled

With `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=span_and_event`
(case C-capture) the prompt and completion appear in:

- span attributes `gen_ai.input.messages` and `gen_ai.output.messages`;
- a log record with event name `gen_ai.client.inference.operation.details`
  carrying the same keys.

With the default (case C), a unique marker placed in the prompt and the
completion text were found nowhere in spans, metrics or logs. The lab's
privacy check therefore inspects span attributes, span events **and** log
records.
