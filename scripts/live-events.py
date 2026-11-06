#!/usr/bin/env python3
"""Follow the bridge's newest run log and print the events a person cares about, one line each.

Runs ON THE HOST (it tails the run directory there):

    ssh bridge-host 'python3 -u ~/echo-gpt-live/scripts/live-events.py'

It never touches the bridge, its port, or the session: it reads a file.
"""
import glob
import json
import os
import sys
import time

KEEP = (
    "offer_received", "offer_repeated", "live_session_started", "hold_released", "bridge_live",
    "live_first_audio", "farewell_heard", "farewell_withdrawn", "farewell_echo_ignored",
    "farewell_close", "rtcp_bye_sent", "live_leg_close", "live_hangup", "session_close",
    "alexa_session_disconnected", "alexa_session_connected", "idle_close", "live_leg_ended",
    "echo_transport_down", "delegation_start", "delegation_answered", "delegation_failed",
    "delegation_confirmed", "delegation_refused", "answer_done", "live_dial_failed",
    "budget_spent", "reflect_done", "reflect_saved", "reflect_failed", "reflect_nothing",
    "max_session", "live_max_age", "ledger_orphan", "warm_failed",
)
SHOW = ("phrase", "reason", "quiet_s", "waited_s", "ms_after_dial", "audio_ms", "packets", "tools",
        "chars", "ms", "status", "sources", "err", "spent_today_usd", "session_s", "idle_s", "now",
        "was")


def newest_log():
    runs = sorted(glob.glob(os.path.expanduser("~/echo-gpt-live/runs/svc-*/events.jsonl")),
                  key=os.path.getmtime)
    return runs[-1] if runs else None


def main():
    path = newest_log()
    if not path:
        print("no run log yet", flush=True)
        return
    print("following " + path, flush=True)
    f = open(path, "r", encoding="utf-8")
    f.seek(0, os.SEEK_END)
    t0 = None
    while True:
        line = f.readline()
        if not line:
            # A restart makes a new run directory; follow it.
            p = newest_log()
            if p and p != path:
                path = p
                f.close()
                f = open(path, "r", encoding="utf-8")
                t0 = None
                print("following " + path, flush=True)
                continue
            time.sleep(0.25)
            continue
        try:
            r = json.loads(line)
        except Exception:
            continue
        ev = r.get("ev", "")
        if ev == "offer_received":
            t0 = r.get("mono_ms")
        if not any(ev.startswith(k) for k in KEEP):
            continue
        rel = ""
        if t0 is not None and "mono_ms" in r:
            rel = "+%.1fs " % ((r["mono_ms"] - t0) / 1000)
        kv = {k: v for k, v in r.items() if k in SHOW}
        print(r.get("t", "")[11:19] + " " + rel + ev + " " + json.dumps(kv, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
