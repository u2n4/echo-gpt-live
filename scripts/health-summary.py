#!/usr/bin/env python3
"""Print a rolling 24-hour summary using only operational counters and timings."""
import argparse
from collections import Counter
from datetime import datetime, timedelta, timezone
import json
import os
import tempfile
from pathlib import Path
import re
from statistics import median

SAFE_NAME = re.compile(r"[a-z][a-z0-9_]{0,63}")
SEARCH_DETAIL = re.compile(
    r"the search did not go through: (?:(?:keenable|direct|reader|wikipedia)="
    r"(?:http_[1-5][0-9]{2}|challenge|timeout|canceled|no_results|request_error) ?){1,4}"
)


def records(paths, since, now):
    for path in paths:
        if datetime.fromtimestamp(path.stat().st_mtime, timezone.utc) < since:
            continue
        with path.open(encoding="utf-8") as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                    stamp = datetime.fromisoformat(event["t"])
                    if since <= stamp <= now:
                        yield event
                except (ValueError, KeyError, TypeError):
                    continue


def failure_reason(event):
    reason = event.get("reason")
    if reason in ("timeout", "canceled", "tool_error") or (
        isinstance(reason, str) and SEARCH_DETAIL.fullmatch(reason)
    ):
        return reason
    if event.get("tool") == "web_search" and event.get("err") == (
        "the search did not go through: reader: answered 403"
    ):
        return "reader=http_403 (legacy)"
    return "unclassified"


def summarize(runs, tvd_log, now):
    since = now - timedelta(hours=24)
    tools = {}
    historical_failures = Counter()
    audio = []
    dial_failures = 0
    for event in records(sorted(runs.glob("svc-*/events.jsonl")), since, now):
        name = event.get("ev")
        tool = event.get("tool")
        if name in ("answer_tool_result", "answer_tool_failed") and isinstance(tool, str) and SAFE_NAME.fullmatch(tool):
            outcome = event.get("outcome")
            if name == "answer_tool_failed":
                historical_failures[f"{tool}: {failure_reason(event)}"] += 1
            elif outcome in ("ok", "failed", "confirmation"):
                row = tools.setdefault(tool, {"ok": 0, "failed": 0, "confirmation": 0, "reasons": Counter()})
                row[outcome] += 1
                if outcome == "failed":
                    row["reasons"][failure_reason(event)] += 1
        elif name == "live_first_audio":
            ms = event.get("ms_after_dial")
            if type(ms) is int and ms >= 0:
                audio.append(ms)
        elif name == "live_dial_failed":
            dial_failures += 1

    for row in tools.values():
        total = row["ok"] + row["failed"] + row["confirmation"]
        row.update(attempts=total, failure_pct=round(100 * row["failed"] / total, 1))

    tv_commands = {}
    for event in records([tvd_log] if tvd_log.is_file() else [], since, now):
        action = event.get("action")
        if event.get("ev") != "command" or not isinstance(action, str) or not SAFE_NAME.fullmatch(action):
            continue
        row = tv_commands.setdefault(action, {"attempts": 0, "failed": 0, "reasons": Counter()})
        row["attempts"] += 1
        if event.get("ok") is False:
            row["failed"] += 1
            reason = event.get("error")
            known = ("unreachable", "off", "timeout", "unknown", "oversize", "unconfigured",
                     "unverified_input", "input_mismatch", "bad_request", "internal", "busy", "unsupported")
            row["reasons"][reason if reason in known else "unclassified"] += 1
    for row in tv_commands.values():
        row["failure_pct"] = round(100 * row["failed"] / row["attempts"], 1)

    return {
        "window_hours": 24,
        "from": since.isoformat(),
        "to": now.isoformat(),
        "router_tools": tools,
        "legacy_failures_without_attempt_counts": historical_failures,
        "tvd_commands": tv_commands,
        "live_dial_failures": dial_failures,
        "first_audio_after_dial_ms": {
            "samples": len(audio),
            "median": median(audio) if audio else None,
            "max": max(audio, default=None),
        },
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runs", type=Path, default=Path.home() / "echo-gpt-live/runs")
    parser.add_argument("--tvd-log", type=Path, default=Path.home() / ".local/state/tvd/events.jsonl")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    report = json.dumps(summarize(args.runs, args.tvd_log, datetime.now(timezone.utc)), indent=2, sort_keys=True)
    if args.output is None:
        print(report)
        return
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=args.output.parent, delete=False) as stream:
        temp = Path(stream.name)
        try:
            print(report, file=stream)
            stream.close()
            os.replace(temp, args.output)
        finally:
            temp.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
