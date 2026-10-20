"""OpenTelemetry wiring for chat-api.

Traces and metrics only. No LoggerProvider is configured on purpose: the
GenAI instrumentation emits prompt/completion content as log records when
content capture is enabled, so without a log pipeline that content cannot
leave the process even if capture is switched on by mistake (span
attributes are still covered by the privacy check in lab-check).

The resource comes from the standard environment variables
(OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES); in Kubernetes the manifest
fills OTEL_RESOURCE_ATTRIBUTES from the Downward API (pod name and uid,
namespace, node).
"""

from dataclasses import dataclass

from fastapi import FastAPI
from opentelemetry import metrics, trace
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.instrumentation.genai.openai import OpenAIInstrumentor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import MetricReader, PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor


@dataclass
class Telemetry:
    tracer_provider: TracerProvider
    meter_provider: MeterProvider

    def shutdown(self) -> None:
        self.tracer_provider.shutdown()
        self.meter_provider.shutdown()


def build_resource() -> Resource:
    return Resource.create()


def configure(span_processor: SpanProcessor | None = None, metric_reader: MetricReader | None = None) -> Telemetry:
    """Install global providers and instrument the OpenAI SDK.

    Without arguments, exports over OTLP (endpoint from
    OTEL_EXPORTER_OTLP_ENDPOINT). Tests pass in-memory processor/reader.
    Must be called once per process, before the OpenAI client is used.
    """
    if span_processor is None:
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter

        span_processor = BatchSpanProcessor(OTLPSpanExporter())
    if metric_reader is None:
        from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter

        # Export interval from OTEL_METRIC_EXPORT_INTERVAL (ms).
        metric_reader = PeriodicExportingMetricReader(OTLPMetricExporter())

    resource = build_resource()
    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(span_processor)
    meter_provider = MeterProvider(resource=resource, metric_readers=[metric_reader])
    trace.set_tracer_provider(tracer_provider)
    metrics.set_meter_provider(meter_provider)

    OpenAIInstrumentor().instrument(tracer_provider=tracer_provider, meter_provider=meter_provider)
    return Telemetry(tracer_provider, meter_provider)


def instrument_app(app: FastAPI, telemetry: Telemetry) -> None:
    """Create the HTTP server span that the GenAI client span nests under."""
    FastAPIInstrumentor.instrument_app(
        app, tracer_provider=telemetry.tracer_provider, meter_provider=telemetry.meter_provider
    )
