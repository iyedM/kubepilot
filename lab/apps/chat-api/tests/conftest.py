"""Shared fixtures: a fake OpenAI-compatible server, in-memory telemetry and
a real uvicorn server running chat-api in a background thread.

The app is exercised over real HTTP (urllib), not through an ASGI test
client, so the FastAPI server span and context propagation are the ones that
run in the cluster.
"""

import json
import socket
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
import uvicorn
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from chat_api import telemetry
from chat_api.app import Settings, create_app

REPO_ROOT = Path(__file__).resolve().parents[4]
CONTENT_KEYS = REPO_ROOT / "lab" / "genai" / "content-keys.json"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class FakeModel:
    """OpenAI-compatible /v1/chat/completions that records request bodies."""

    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.status = 200
        self.port = free_port()
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802 (http.server API)
                body = json.loads(self.rfile.read(int(self.headers["content-length"])))
                fake.requests.append(body)
                if fake.status != 200:
                    payload = {"error": {"message": "fake failure", "type": "server_error"}}
                else:
                    payload = {
                        "id": "chatcmpl-fake",
                        "object": "chat.completion",
                        "created": 0,
                        "model": body["model"],
                        "system_fingerprint": "fake-fp",
                        "choices": [
                            {"index": 0, "finish_reason": "length",
                             "message": {"role": "assistant", "content": "fake completion text for tests"}}
                        ],
                        "usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
                    }
                raw = json.dumps(payload).encode()
                self.send_response(fake.status)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *args):  # keep test output quiet
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", self.port), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}/v1"


@pytest.fixture(scope="session")
def tel():
    """Global in-memory providers; configure() may only run once per process."""
    span_exporter = InMemorySpanExporter()
    reader = InMemoryMetricReader()
    t = telemetry.configure(span_processor=SimpleSpanProcessor(span_exporter), metric_reader=reader)
    yield {"telemetry": t, "spans": span_exporter, "metrics": reader}


@pytest.fixture(scope="session")
def fake_model():
    fake = FakeModel()
    yield fake
    fake.server.shutdown()


class RunningApp:
    def __init__(self, model_base_url: str, tel) -> None:
        app = create_app(Settings(model_base_url=model_base_url, request_timeout_s=5))
        telemetry.instrument_app(app, tel["telemetry"])
        self.port = free_port()
        self.server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=self.port, log_level="warning"))
        self.thread = threading.Thread(target=self.server.run, daemon=True)
        self.thread.start()
        deadline = time.monotonic() + 10
        while not self.server.started:
            if time.monotonic() > deadline:
                raise RuntimeError("uvicorn did not start")
            time.sleep(0.05)

    def post(self, path: str, payload: dict) -> tuple[int, dict]:
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}", data=json.dumps(payload).encode(),
            headers={"content-type": "application/json"}, method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                return resp.status, json.load(resp)
        except urllib.error.HTTPError as exc:
            return exc.code, json.load(exc)

    def stop(self) -> None:
        self.server.should_exit = True
        self.thread.join(timeout=10)


@pytest.fixture(scope="session")
def app(fake_model, tel):
    running = RunningApp(fake_model.base_url, tel)
    yield running
    running.stop()


@pytest.fixture(scope="session")
def unreachable_app(tel):
    """chat-api pointed at a closed port: every model call is refused."""
    running = RunningApp(f"http://127.0.0.1:{free_port()}/v1", tel)
    yield running
    running.stop()


@pytest.fixture(autouse=True)
def _reset(tel, fake_model):
    tel["spans"].clear()
    fake_model.requests.clear()
    fake_model.status = 200
    yield


def spans_as_dicts(exporter: InMemorySpanExporter) -> list[dict]:
    return [
        {
            "name": s.name,
            "trace_id": format(s.context.trace_id, "032x"),
            "span_id": format(s.context.span_id, "016x"),
            "parent_id": format(s.parent.span_id, "016x") if s.parent else None,
            "kind": s.kind.name,
            "status": s.status.status_code.name,
            "attributes": dict(s.attributes or {}),
            "events": [{"name": e.name, "attributes": dict(e.attributes or {})} for e in s.events],
            "resource": dict(s.resource.attributes),
        }
        for s in exporter.get_finished_spans()
    ]


def wait_for_spans(exporter: InMemorySpanExporter, predicate, timeout_s: float = 5.0) -> list[dict]:
    """Server spans end after the response is sent: poll until they exist."""
    deadline = time.monotonic() + timeout_s
    while True:
        spans = spans_as_dicts(exporter)
        if predicate(spans) or time.monotonic() > deadline:
            return spans
        time.sleep(0.05)
