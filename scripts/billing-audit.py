#!/usr/bin/env python3
"""Account for every GPT-Live session this machine has ever opened, and what each one cost.

Written the moment the owner saw a bill he did not recognise. A per-call estimate is not an answer
to "where did the total come from": the answer has to cover every session from every process,
including the diagnostics, and it has to say what was NOT closed.

Reads the service log plus every runs/*/events.jsonl.
"""
import glob
import json
import os
import sys
from collections import defaultdict

RATE_PER_MIN = 0.05
INIT_SECONDS = 15  # billed at creation per OpenAI's Live docs


def events(paths):
    for p in paths:
        if not os.path.exists(p):
            continue
        with open(p, encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    yield p, json.loads(line)
                except ValueError:
                    continue


def main(logs):
    echo_to_oai = {}
    created = []          # (source, session_id)
    created_400 = 0
    started = {}          # session_id -> mono_ms
    closed = {}           # session_id -> (age_s, reason)
    hangups = 0
    swept = set()         # OpenAI session ids closed by a later process's ledger sweep
    per_source = defaultdict(lambda: {"creates": 0, "seconds": 0.0})

    for src, e in events(logs):
        ev = e.get("ev")
        sid = (src, e.get("sess"))
        if e.get("session_id"):
            echo_to_oai[sid] = e["session_id"]
        if ev == "live_create_session":
            if e.get("status") in (200, 201):
                created.append((src, sid))
                per_source[src]["creates"] += 1
            elif e.get("status") == 400:
                created_400 += 1
        elif ev == "live_session_started":
            started[sid] = e.get("mono_ms")
        elif ev == "live_leg_close":
            # Key on the SAME identifier as `started`, which is the Echo session's short id.
            # live_leg_close carries BOTH that and OpenAI's own session_id, and preferring
            # session_id here compares two different namespaces: every started session then looks
            # as though it never closed. It reported sixteen leaks on a machine with none.
            closed[sid] = (e.get("age_s", 0.0), e.get("reason"),
                                     e.get("session_id"))
            per_source[src]["seconds"] += float(e.get("age_s") or 0.0)
        elif ev == "live_hangup":
            hangups += 1
        elif ev == "ledger_orphan_closed":
            # A session the previous process left billing, closed by the next start's sweep. It is
            # a close like any other; not counting it reports the safety net working as a leak.
            swept.add(e.get("id"))

    total_creates = len(created)
    total_seconds = sum(v["seconds"] for v in per_source.values())
    init_seconds = total_creates * INIT_SECONDS

    print(f"sessions created (HTTP 201):          {total_creates}")
    print(f"creation attempts rejected (HTTP 400): {created_400}   "
          f"(storage retry; a rejected request opens nothing)")
    print(f"hangups sent:                          {hangups}")
    print(f"sessions with a recorded close:        {len(closed)}")
    print()
    print(f"connected time, summed:  {total_seconds:8.1f} s = {total_seconds / 60:6.2f} min"
          f"  -> ${total_seconds / 60 * RATE_PER_MIN:6.3f}")
    print(f"init time, if NOT credited: {init_seconds:6d} s = {init_seconds / 60:6.2f} min"
          f"  -> ${init_seconds / 60 * RATE_PER_MIN:6.3f}   ({total_creates} x {INIT_SECONDS}s)")
    print(f"{'':24}worst case total -> "
          f"${(total_seconds + init_seconds) / 60 * RATE_PER_MIN:6.3f}")
    print()

    # Two DIFFERENT failures, and the old code computed only the harmless one while calling it by
    # the dangerous one's name:
    #
    #   never_started : created at OpenAI but the media leg never came up. Cheap, and visible.
    #   never_closed  : it started, it billed, and NOTHING ever recorded a close. This is the case
    #                   this whole script exists for - its own docstring says "it has to say what
    #                   was NOT closed" - and it was never computed. Worse, connected time is summed
    #                   from live_leg_close.age_s, so a session that never closed contributes
    #                   exactly ZERO seconds. The worst case was being counted as free.
    never_started = [sid for _, sid in created if sid[1] and sid not in started]
    # None is what an older log line carries when it predates session tagging; it is not a session.
    # Sweeps use OpenAI IDs; matching counts across namespaces can hide a paid orphan.
    unmatched = [sid for sid in started if sid[1] and sid not in closed]
    never_closed_accounted = [sid for sid in unmatched if echo_to_oai.get(sid) in swept]
    never_closed = [sid for sid in unmatched if sid not in never_closed_accounted]
    swept_note = (f"   {len(never_closed_accounted)} matched session(s) were closed by a ledger sweep."
                  " Swept durations are not included in the connected-time total.")
    print("--- per source ---")
    for src in sorted(per_source, key=lambda s: -per_source[s]["seconds"]):
        v = per_source[src]
        label = "the live service" if "bridge.log" in src else src
        print(f"  {v['creates']:3d} sessions  {v['seconds']:8.1f} s  {label}")

    print()
    print("--- longest sessions ---")
    for sid, (age, reason, oai) in sorted(closed.items(), key=lambda kv: -kv[1][0])[:8]:
        print(f"  {age:7.1f} s  {reason:<20} {sid}  {oai or ''}")

    if never_closed_accounted:
        print()
        print(f"   {len(never_closed_accounted)} session(s) ended without a close event:")
        for sid in never_closed_accounted:
            print(f"     {sid}")
        print(swept_note)

    if never_started:
        print()
        print(f"   {len(never_started)} created sessions never reported a start "
              f"(no media leg; cheap, but they were created)")

    if never_closed:
        print()
        print(f"!! {len(never_closed)} sessions STARTED AND NEVER CLOSED.")
        print("   Each one billed for an unknown length of time, and none of it is in the total")
        print("   above, because connected time is summed from close events. The figures printed")
        print("   above are therefore a FLOOR, not the bill.")
        for sid in never_closed:
            print(f"     {sid}")
        print("   A session left open by a killed process is closed by the next start's ledger")
        print("   sweep; one that appears here and is not in the ledger was never swept.")
    else:
        print()
        print("   every recorded start has a close or an identity-matched sweep; totals cover recorded durations only.")


if __name__ == "__main__":
    logs = sys.argv[1:] or ([os.path.expanduser("~/.local/state/rtcdiag/bridge.log")]
                            + sorted(glob.glob("runs/*/events.jsonl")))
    main(logs)
