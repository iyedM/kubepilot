"""chat-api: a minimal GenAI service for the KubePilot lab.

POST /chat {"prompt_id": "..."} sends one prompt of the fixed catalog to the
OpenAI-compatible model server and returns token counts, latency and the
trace id of the request (so a check, or later `why-slow`, can fetch that
exact trace). The completion text is not returned.
"""

import os
import time
from dataclasses import dataclass

import openai
from fastapi import FastAPI
from fastapi.responses import JSONResponse
from opentelemetry import trace
from pydantic import BaseModel

from chat_api.prompts import CATALOG


@dataclass(frozen=True)
class Settings:
    model_base_url: str
    model_name: str = "qwen2.5-0.5b-instruct"
    request_timeout_s: float = 30.0
    seed: int = 42
    # Lab-only fault hooks (PUT /lab/delay). Off unless LAB_FAULT_HOOKS=1.
    lab_fault_hooks: bool = False

    @classmethod
    def from_env(cls) -> "Settings":
        return cls(
            model_base_url=os.environ["MODEL_BASE_URL"],
            model_name=os.environ.get("MODEL_NAME", cls.model_name),
            request_timeout_s=float(os.environ.get("MODEL_REQUEST_TIMEOUT_S", cls.request_timeout_s)),
            seed=int(os.environ.get("MODEL_SEED", cls.seed)),
            lab_fault_hooks=os.environ.get("LAB_FAULT_HOOKS") == "1",
        )


class ChatRequest(BaseModel):
    prompt_id: str


class DelayRequest(BaseModel):
    ms: int


def _trace_id() -> str:
    return format(trace.get_current_span().get_span_context().trace_id, "032x")


def create_app(settings: Settings) -> FastAPI:
    app = FastAPI(title="chat-api")
    # max_retries=0: a retry would hide a failing model server and add
    # latency that is not the model's; the lab must see failures as they are.
    client = openai.OpenAI(
        base_url=settings.model_base_url,
        api_key="unused",
        max_retries=0,
        timeout=settings.request_timeout_s,
    )

    # Artificial delay before the model call, set at runtime through the lab
    # hook. It only sleeps: no CPU, no memory, no Kubernetes object or event
    # changes, so it reproduces "slow for no infrastructure reason".
    delay = {"ms": 0}

    if settings.lab_fault_hooks:

        @app.put("/lab/delay")
        def set_delay(req: DelayRequest) -> dict[str, int]:
            delay["ms"] = max(0, req.ms)
            return delay

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        # Deliberately independent of the model server: a model outage must
        # not make chat-api unready, or faults would cascade and blur the
        # diagnosis.
        return {"status": "ok"}

    @app.post("/chat")
    def chat(req: ChatRequest) -> JSONResponse:
        prompt = CATALOG.get(req.prompt_id)
        if prompt is None:
            return JSONResponse(status_code=404, content={"error": "unknown prompt_id", "prompt_id": req.prompt_id})

        trace_id = _trace_id()
        start = time.perf_counter()
        if delay["ms"]:
            time.sleep(delay["ms"] / 1000)
        try:
            resp = client.chat.completions.create(
                model=settings.model_name,
                messages=[{"role": "user", "content": prompt.text}],
                max_tokens=prompt.max_tokens,
                temperature=0,
                seed=settings.seed,
                # llama.cpp extension: disable the prompt cache so token
                # counts and latencies are not skewed by cache hits.
                extra_body={"cache_prompt": False},
            )
        except openai.APIError as exc:
            return JSONResponse(
                status_code=502,
                content={
                    "error": type(exc).__name__,
                    "prompt_id": prompt.id,
                    "latency_ms": round((time.perf_counter() - start) * 1000, 1),
                    "trace_id": trace_id,
                },
            )

        usage = resp.usage
        return JSONResponse(
            content={
                "prompt_id": prompt.id,
                "finish_reason": resp.choices[0].finish_reason,
                "prompt_tokens": usage.prompt_tokens if usage else None,
                "completion_tokens": usage.completion_tokens if usage else None,
                "latency_ms": round((time.perf_counter() - start) * 1000, 1),
                "trace_id": trace_id,
            }
        )

    return app
