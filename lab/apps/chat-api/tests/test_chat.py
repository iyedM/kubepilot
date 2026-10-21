import json
import os
import subprocess
import sys
import textwrap
import urllib.request

from conftest import CONTENT_KEYS, wait_for_spans
from privacy import find_content, load_rules

from chat_api import loadgen, telemetry
from chat_api.prompts import CATALOG, LOADGEN_ORDER, PRIVACY_CANARY


def _chat_span(spans):
    return next(s for s in spans if s["attributes"].get("gen_ai.operation.name") == "chat")


def _server_span(spans):
    return next(s for s in spans if s["kind"] == "SERVER")


def _have_both(spans):
    return any(s["kind"] == "SERVER" for s in spans) and any(
        s["attributes"].get("gen_ai.operation.name") == "chat" for s in spans
    )


def test_healthz_does_not_call_the_model(app, fake_model):
    with urllib.request.urlopen(f"http://127.0.0.1:{app.port}/healthz", timeout=5) as resp:
        assert resp.status == 200
        assert json.load(resp) == {"status": "ok"}
    assert fake_model.requests == []


def test_success_span_is_child_of_http_server_span(app, tel):
    status, body = app.post("/chat", {"prompt_id": "short-16"})
    assert status == 200
    assert body["completion_tokens"] == 7 and body["prompt_tokens"] == 11

    spans = wait_for_spans(tel["spans"], _have_both)
    chat, server = _chat_span(spans), _server_span(spans)
    assert chat["name"] == "chat qwen2.5-0.5b-instruct"
    assert chat["kind"] == "CLIENT"
    assert chat["trace_id"] == server["trace_id"]
    assert chat["parent_id"] == server["span_id"]
    attrs = chat["attributes"]
    assert attrs["gen_ai.request.model"] == "qwen2.5-0.5b-instruct"
    assert attrs["gen_ai.request.max_tokens"] == CATALOG["short-16"].max_tokens
    assert attrs["gen_ai.usage.input_tokens"] == 11
    assert attrs["gen_ai.usage.output_tokens"] == 7
    assert attrs["server.address"] == "127.0.0.1"


def test_request_sent_to_the_model(app, fake_model):
    status, _ = app.post("/chat", {"prompt_id": "medium-64"})
    assert status == 200
    [sent] = fake_model.requests
    assert sent["cache_prompt"] is False  # llama.cpp prompt cache disabled
    assert sent["temperature"] == 0
    assert sent["seed"] == 42
    assert sent["max_tokens"] == 64
    assert sent["messages"] == [{"role": "user", "content": CATALOG["medium-64"].text}]


def test_returned_trace_id_is_the_exported_trace(app, tel):
    status, body = app.post("/chat", {"prompt_id": "short-16"})
    assert status == 200
    spans = wait_for_spans(tel["spans"], _have_both)
    assert {s["trace_id"] for s in spans} == {body["trace_id"]}


def test_no_content_in_telemetry_by_default(app, tel):
    status, _ = app.post("/chat", {"prompt_id": "privacy-canary"})
    assert status == 200
    spans = wait_for_spans(tel["spans"], _have_both)
    findings = find_content(spans, [], load_rules(CONTENT_KEYS),
                            markers=(PRIVACY_CANARY, "fake completion text for tests"))
    assert findings == []


def test_privacy_detector_finds_content_when_capture_is_enabled(fake_model, tmp_path):
    """Positive control: the same detector must name the content key when
    capture is switched on. Runs in a subprocess because providers and the
    capture setting are process-global."""
    out = tmp_path / "spans.json"
    script = textwrap.dedent(
        f"""
        import json, openai
        from opentelemetry.sdk.metrics.export import InMemoryMetricReader
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        from chat_api import telemetry
        exp = InMemorySpanExporter()
        telemetry.configure(span_processor=SimpleSpanProcessor(exp), metric_reader=InMemoryMetricReader())
        client = openai.OpenAI(base_url={fake_model.base_url!r}, api_key="x", max_retries=0)
        client.chat.completions.create(model="m", messages=[{{"role": "user", "content": {PRIVACY_CANARY!r}}}],
                                       max_tokens=4)
        spans = [{{"name": s.name, "attributes": dict(s.attributes or {{}}),
                   "events": [{{"name": e.name, "attributes": dict(e.attributes or {{}})}} for e in s.events]}}
                 for s in exp.get_finished_spans()]
        json.dump(spans, open({str(out)!r}, "w"), default=str)
        """
    )
    env = {**os.environ, "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "span_only"}
    subprocess.run([sys.executable, "-c", script], env=env, check=True, timeout=60)
    findings = find_content(json.loads(out.read_text()), [], load_rules(CONTENT_KEYS), markers=(PRIVACY_CANARY,))
    keys = {f.key for f in findings}
    assert "gen_ai.input.messages" in keys
    assert f"marker {PRIVACY_CANARY!r}" in keys


def test_model_error_gives_502_and_error_span_in_the_same_trace(app, tel, fake_model):
    fake_model.status = 500
    status, body = app.post("/chat", {"prompt_id": "short-16"})
    assert status == 502
    assert body["error"] == "InternalServerError"

    spans = wait_for_spans(tel["spans"], _have_both)
    chat, server = _chat_span(spans), _server_span(spans)
    # This is the failure trace P2 will analyse: one trace, HTTP span on top,
    # GenAI span below it, marked as an error with its type.
    assert {s["trace_id"] for s in spans} == {body["trace_id"]}
    assert chat["parent_id"] == server["span_id"]
    assert chat["status"] == "ERROR"
    assert chat["attributes"]["error.type"]


def test_unreachable_model_is_reported_as_connection_error(unreachable_app, tel):
    status, body = unreachable_app.post("/chat", {"prompt_id": "short-16"})
    assert status == 502
    assert body["error"] == "APIConnectionError"
    spans = wait_for_spans(tel["spans"], _have_both)
    chat = _chat_span(spans)
    assert chat["status"] == "ERROR"
    assert chat["attributes"]["error.type"] == "openai.APIConnectionError"
    assert chat["trace_id"] == body["trace_id"]


def test_unknown_prompt_is_404_without_model_call(app, fake_model):
    status, body = app.post("/chat", {"prompt_id": "free text is not accepted"})
    assert status == 404
    assert fake_model.requests == []


def test_metric_names_emitted(app, tel):
    status, _ = app.post("/chat", {"prompt_id": "short-16"})
    assert status == 200
    data = tel["metrics"].get_metrics_data()
    names = {m.name for rm in data.resource_metrics for sm in rm.scope_metrics for m in sm.metrics}
    # Names recorded in docs/genai-conventions.md for genai-openai 1.2b0. If an
    # upgrade renames them, this test fails on purpose.
    assert "gen_ai.client.operation.duration" in names
    assert "gen_ai.client.token.usage" in names


def test_resource_comes_from_downward_api_env(monkeypatch):
    # Same format as lab/genai/chat-api.yaml, with the Downward API values
    # already substituted by the kubelet.
    monkeypatch.setenv("OTEL_SERVICE_NAME", "chat-api")
    monkeypatch.setenv(
        "OTEL_RESOURCE_ATTRIBUTES",
        "k8s.pod.name=chat-api-abc,k8s.pod.uid=0f3c-uid,k8s.namespace.name=genai,k8s.node.name=worker",
    )
    attrs = telemetry.build_resource().attributes
    assert attrs["service.name"] == "chat-api"
    assert attrs["k8s.pod.uid"] == "0f3c-uid"
    assert attrs["k8s.pod.name"] == "chat-api-abc"
    assert attrs["k8s.namespace.name"] == "genai"
    assert attrs["k8s.node.name"] == "worker"


def test_loadgen_cycles_in_a_fixed_order():
    seq = loadgen.prompt_sequence()
    first = [next(seq) for _ in range(2 * len(LOADGEN_ORDER))]
    assert first == list(LOADGEN_ORDER) * 2
    assert all(p in CATALOG for p in LOADGEN_ORDER)


def test_long_prompt_budget_is_the_one_measured():
    # 576 prompt tokens + 256 generated tokens were measured to fit in the
    # 2048 context (make compat-matrix); keep the catalog entry identical.
    assert CATALOG["long-256"].max_tokens == 256
    assert CATALOG["long-256"].text.count("readiness probe") == 30


def test_lab_delay_hook_is_off_by_default(app):
    status, _ = app.post("/lab/delay", {"ms": 500}, method="PUT")
    assert status == 404


def test_lab_delay_shows_as_a_gap_before_the_genai_span(hooked_app, tel, fake_model):
    status, body = hooked_app.post("/lab/delay", {"ms": 400}, method="PUT")
    assert (status, body) == (200, {"ms": 400, "every_n": 1})
    try:
        status, body = hooked_app.post("/chat", {"prompt_id": "short-16"})
        assert status == 200
        assert body["latency_ms"] >= 400
        spans = wait_for_spans(tel["spans"], _have_both)
        chat, server = _chat_span(spans), _server_span(spans)
        # The delay is spent in the app before the model call: no extra span,
        # only a gap between the HTTP span start and the GenAI span start.
        assert (chat["start_ns"] - server["start_ns"]) / 1e6 >= 400
        assert len([s for s in spans if s["kind"] == "CLIENT"]) == 1
        # The model request itself is unchanged.
        [sent] = fake_model.requests
        assert sent["max_tokens"] == CATALOG["short-16"].max_tokens
    finally:
        hooked_app.post("/lab/delay", {"ms": 0}, method="PUT")


def test_lab_intermittent_delay_delays_one_request_in_n(hooked_app):
    status, body = hooked_app.post("/lab/delay", {"ms": 300, "every_n": 3}, method="PUT")
    assert (status, body) == (200, {"ms": 300, "every_n": 3})
    try:
        latencies = []
        for _ in range(4):
            status, body = hooked_app.post("/chat", {"prompt_id": "short-16"})
            assert status == 200
            latencies.append(body["latency_ms"])
        assert latencies[0] >= 300
        assert latencies[1] < 200
        assert latencies[2] < 200
        assert latencies[3] >= 300
    finally:
        hooked_app.post("/lab/delay", {"ms": 0}, method="PUT")
