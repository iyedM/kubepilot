"""Load generator: `python -m chat_api.loadgen`.

Sends POST /chat to chat-api at a fixed interval, cycling through
LOADGEN_ORDER in a fixed order, one request at a time, and prints one JSON
line per request. Fixed-rate: if a request takes longer than the interval,
the next one starts immediately (no catch-up burst).

This is the steady background traffic of the lab, not the P2 benchmark
workload (which will use a versioned schedule).
"""

import itertools
import json
import os
import time
import urllib.error
import urllib.request
from collections.abc import Iterator

from chat_api.prompts import LOADGEN_ORDER


def prompt_sequence() -> Iterator[str]:
    return itertools.cycle(LOADGEN_ORDER)


def send(url: str, prompt_id: str, timeout_s: float) -> tuple[int, dict]:
    body = json.dumps({"prompt_id": prompt_id}).encode()
    req = urllib.request.Request(url, data=body, headers={"content-type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout_s) as resp:
            return resp.status, json.load(resp)
    except urllib.error.HTTPError as exc:
        try:
            return exc.code, json.load(exc)
        except ValueError:
            return exc.code, {}
    except (urllib.error.URLError, TimeoutError) as exc:
        return 0, {"error": type(exc).__name__}


def main() -> None:
    url = os.environ["CHAT_API_URL"].rstrip("/") + "/chat"
    interval = float(os.environ.get("LOADGEN_INTERVAL_S", "5"))
    timeout = float(os.environ.get("LOADGEN_TIMEOUT_S", "60"))
    next_at = time.monotonic()
    for prompt_id in prompt_sequence():
        start = time.monotonic()
        status, payload = send(url, prompt_id, timeout)
        print(
            json.dumps(
                {
                    "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                    "prompt_id": prompt_id,
                    "status": status,
                    "client_latency_ms": round((time.monotonic() - start) * 1000, 1),
                    "trace_id": payload.get("trace_id"),
                    "error": payload.get("error"),
                }
            ),
            flush=True,
        )
        next_at = max(next_at + interval, time.monotonic())
        time.sleep(max(0.0, next_at - time.monotonic()))


if __name__ == "__main__":
    main()
