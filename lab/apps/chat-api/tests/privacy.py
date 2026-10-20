"""Detect prompt/completion content in exported telemetry.

Test helper. lab/scripts/check-genai.sh applies the same rules with jq
and reads the same key list (lab/genai/content-keys.json).
"""

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class Finding:
    where: str  # e.g. "span 'chat qwen…' attribute"
    key: str


def load_rules(path: str | Path) -> dict[str, list[str]]:
    with open(path) as f:
        return json.load(f)


def _content_key(key: str, rules: dict[str, list[str]]) -> bool:
    return key in rules["attribute_keys"] or any(key.startswith(p) for p in rules["attribute_key_prefixes"])


def find_content(spans: list[dict[str, Any]], logs: list[dict[str, Any]], rules: dict[str, list[str]],
                 markers: tuple[str, ...] = ()) -> list[Finding]:
    """Return every place where content keys, content events or markers appear.

    spans: [{"name", "attributes", "events": [{"name", "attributes"}]}]
    logs:  [{"event_name", "attributes", "body"}]
    """
    findings: list[Finding] = []
    for span in spans:
        for key in span.get("attributes", {}):
            if _content_key(key, rules):
                findings.append(Finding(f"span '{span['name']}' attribute", key))
        for event in span.get("events", []):
            if event["name"] in rules["event_names"]:
                findings.append(Finding(f"span '{span['name']}' event", event["name"]))
            for key in event.get("attributes", {}):
                if _content_key(key, rules):
                    findings.append(Finding(f"span '{span['name']}' event '{event['name']}' attribute", key))
    for log in logs:
        if log.get("event_name") in rules["event_names"]:
            findings.append(Finding("log record", log["event_name"]))
        for key in log.get("attributes", {}):
            if _content_key(key, rules):
                findings.append(Finding(f"log record '{log.get('event_name')}' attribute", key))
    serialized = json.dumps({"spans": spans, "logs": logs}, default=str)
    for marker in markers:
        if marker and marker in serialized:
            findings.append(Finding("serialized telemetry", f"marker {marker!r}"))
    return findings
