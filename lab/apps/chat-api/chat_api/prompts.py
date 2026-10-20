"""Fixed prompt catalog (v0).

The app never accepts free text: callers send a prompt id. Prompts are
synthetic, so the lab never handles user data, and every request is
reproducible. The P2 benchmark will use its own versioned workload; this
catalog only covers the lab's three size classes plus two check prompts.
"""

from dataclasses import dataclass

# Unique marker used by the privacy checks: if it appears anywhere in the
# exported telemetry, prompt content leaked.
PRIVACY_CANARY = "KPCANARY7F3A"

_SENTENCE = "A Kubernetes readiness probe tells the kubelet when a container can receive traffic from a Service. "


@dataclass(frozen=True)
class Prompt:
    id: str
    text: str
    max_tokens: int


CATALOG: dict[str, Prompt] = {
    p.id: p
    for p in (
        Prompt("short-16", "Name one Kubernetes workload resource.", 16),
        Prompt("medium-64", _SENTENCE * 7 + "Explain this in two sentences.", 64),
        # Same long prompt as the context check of `make compat-matrix`:
        # 576 prompt tokens + 256 generated tokens fit in the 2048 context.
        Prompt("long-256", _SENTENCE * 30 + "Summarize the text above.", 256),
        Prompt("check", "Reply with one word: ready", 8),
        Prompt("privacy-canary", f"Reference {PRIVACY_CANARY}. Reply with one short sentence about Kubernetes.", 16),
    )
}

# Order in which the load generator cycles through the catalog.
LOADGEN_ORDER: tuple[str, ...] = ("short-16", "medium-64", "long-256")
