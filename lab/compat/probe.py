"""Probe what an OpenAI instrumentation package really emits.

Runs inside a throwaway container where exactly one instrumentation package
and one openai SDK version are installed (see run-matrix.sh). It sends one
successful chat completion and one failing call to an OpenAI-compatible
server, captures every span, metric and log record in memory, and writes
them to OUT_DIR/telemetry.json.

Nothing here is used by the lab app: it only records facts about the
packages, and the recorded telemetry doubles as fixtures for the P2 adapter.

Environment:
  BASE_URL      OpenAI-compatible endpoint, e.g. http://kp-llama:8080/v1
  MODEL         model name sent in the request
  OUT_DIR       where telemetry.json is written
  CASE          label of the matrix case
  OTEL_SEMCONV_STABILITY_OPT_IN, OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT
                passed through to the instrumentation untouched
"""

import importlib.metadata
import json
import os
import sys
import traceback

from opentelemetry import metrics, trace
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

# A unique marker placed in the prompt. If it shows up anywhere in the
# exported telemetry, prompt content leaked.
CANARY = "KPCANARY7F3A"
PROMPT = f"Reference {CANARY}. Reply with one short sentence about Kubernetes."

# --- providers ---------------------------------------------------------------
span_exporter = InMemorySpanExporter()
tracer_provider = TracerProvider()
tracer_provider.add_span_processor(SimpleSpanProcessor(span_exporter))
trace.set_tracer_provider(tracer_provider)

metric_reader = InMemoryMetricReader()
metrics.set_meter_provider(MeterProvider(metric_readers=[metric_reader]))

# Logs/events: content may be emitted as log records rather than span
# attributes. The SDK logs API is still underscored in some versions.
log_exporter = None
try:
    from opentelemetry._logs import set_logger_provider
    from opentelemetry.sdk._logs import LoggerProvider
    from opentelemetry.sdk._logs.export import InMemoryLogExporter, SimpleLogRecordProcessor

    log_exporter = InMemoryLogExporter()
    logger_provider = LoggerProvider()
    logger_provider.add_log_record_processor(SimpleLogRecordProcessor(log_exporter))
    set_logger_provider(logger_provider)
except ImportError as exc:  # recorded, not fatal
    print(f"logs SDK not available: {exc}", file=sys.stderr)

# --- instrumentation (exactly one package is installed) ----------------------
package = None
try:
    from opentelemetry.instrumentation.genai.openai import OpenAIInstrumentor

    package = "opentelemetry-instrumentation-genai-openai"
except ImportError:
    from opentelemetry.instrumentation.openai_v2 import OpenAIInstrumentor

    package = "opentelemetry-instrumentation-openai-v2"

OpenAIInstrumentor().instrument()

import openai  # noqa: E402  (must be imported after instrument())

client = openai.OpenAI(base_url=os.environ["BASE_URL"], api_key="unused", max_retries=0, timeout=60)
bad_client = openai.OpenAI(base_url="http://127.0.0.1:9/v1", api_key="unused", max_retries=0, timeout=5)

errors = []
completion_text = ""
try:
    resp = client.chat.completions.create(
        model=os.environ["MODEL"],
        messages=[{"role": "user", "content": PROMPT}],
        max_tokens=16,
        temperature=0,
    )
    completion_text = resp.choices[0].message.content or ""
except Exception:  # recorded in the output, the matrix decides
    errors.append({"call": "success_path", "traceback": traceback.format_exc()})

try:
    bad_client.chat.completions.create(
        model=os.environ["MODEL"],
        messages=[{"role": "user", "content": "unreachable"}],
        max_tokens=4,
    )
    errors.append({"call": "error_path", "traceback": "expected a connection error, got a response"})
except openai.APIConnectionError:
    pass  # expected: this call exists to record the error span
except Exception:
    errors.append({"call": "error_path", "traceback": traceback.format_exc()})


# --- dump --------------------------------------------------------------------
def plain(value):
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    return value


spans = []
for s in span_exporter.get_finished_spans():
    spans.append(
        {
            "name": s.name,
            "kind": str(s.kind),
            "status": {"code": str(s.status.status_code), "description": s.status.description},
            "attributes": {k: plain(v) for k, v in (s.attributes or {}).items()},
            "events": [
                {"name": e.name, "attributes": {k: plain(v) for k, v in (e.attributes or {}).items()}}
                for e in s.events
            ],
            "resource": {k: plain(v) for k, v in s.resource.attributes.items()},
            "scope": s.instrumentation_scope.name if s.instrumentation_scope else None,
        }
    )

metric_list = []
data = metric_reader.get_metrics_data()
for rm in data.resource_metrics if data else []:
    for sm in rm.scope_metrics:
        for m in sm.metrics:
            points = list(getattr(m.data, "data_points", []))
            metric_list.append(
                {
                    "name": m.name,
                    "unit": m.unit,
                    "type": type(m.data).__name__,
                    "scope": sm.scope.name,
                    "datapoints": [
                        {
                            "attributes": {k: plain(v) for k, v in (p.attributes or {}).items()},
                            "count": getattr(p, "count", None),
                            "sum": getattr(p, "sum", None),
                            "value": getattr(p, "value", None),
                        }
                        for p in points
                    ],
                }
            )

logs = []
if log_exporter is not None:
    for item in log_exporter.get_finished_logs():
        rec = getattr(item, "log_record", item)
        logs.append(
            {
                "event_name": getattr(rec, "event_name", None),
                "attributes": {k: plain(v) for k, v in (rec.attributes or {}).items()},
                "body": rec.body if isinstance(rec.body, (str, int, float, bool, type(None))) else repr(rec.body),
            }
        )

# Privacy probe: search the serialized telemetry for the prompt canary and
# for the completion text.
serialized = json.dumps({"spans": spans, "metrics": metric_list, "logs": logs})
content_found = {
    "prompt_canary": CANARY in serialized,
    "completion_text": bool(completion_text.strip()) and completion_text.strip() in serialized,
}

versions = {}
for name in [package, "openai", "opentelemetry-sdk", "opentelemetry-util-genai", "opentelemetry-semantic-conventions"]:
    try:
        versions[name] = importlib.metadata.version(name)
    except importlib.metadata.PackageNotFoundError:
        versions[name] = None

out = {
    "case": os.environ.get("CASE"),
    "package": package,
    "versions": versions,
    "env": {
        k: os.environ.get(k)
        for k in ["OTEL_SEMCONV_STABILITY_OPT_IN", "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"]
    },
    "errors": errors,
    "completion_text": completion_text,
    "content_found": content_found,
    "spans": spans,
    "metrics": metric_list,
    "logs": logs,
}
os.makedirs(os.environ["OUT_DIR"], exist_ok=True)
with open(os.path.join(os.environ["OUT_DIR"], "telemetry.json"), "w") as f:
    json.dump(out, f, indent=2, sort_keys=True, default=str)
print(json.dumps({"case": out["case"], "errors": len(errors), "spans": len(spans), "metrics": len(metric_list), "logs": len(logs)}))
