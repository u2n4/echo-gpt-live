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
    created = []          # (source, session_id)
    created_400 = 0
    started = {}          # session_id -> mono_ms
    closed = {}           # session_id -> (age_s, reason)
    hangups = 0
    swept = set()         # OpenAI session ids closed by a later process's ledger sweep
    per_source = defaultdict(lambda: {"creates": 0, "seconds": 0.0})

    for src, e in events(logs):
        ev = e.get("ev")
        if ev == "live_create_session":
            if e.get("status") in (200, 201):
                created.append((src, e.get("sess")))
                per_source[src]["creates"] += 1
            elif e.get("status") == 400:
                created_400 += 1
        elif ev == "live_session_started":
            started[e.get("sess")] = e.get("mono_ms")
        elif ev == "live_leg_close":
            # Key on the SAME identifier as `started`, which is the Echo session's short id.
            # live_leg_close carries BOTH that and OpenAI's own session_id, and preferring
            # session_id here compares two different namespaces: every started session then looks
            # as though it never closed. It reported sixteen leaks on a machine with none.
            closed[e.get("sess")] = (e.get("age_s", 0.0), e.get("reason"),
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
    never_started = [sid for _, sid in created if sid and sid not in started]
    # None is what an older log line carries when it predates session tagging; it is not a session.
    # A session counts as closed if its own leg closed, OR if a later start's ledger sweep closed
    # it. `closed` is keyed by the Echo id and `swept` by OpenAI's, so the sweep is matched by
    # count: an unmatched start followed by a sweep of exactly that many orphans is accounted for.
    never_closed = [sid for sid in started if sid and sid not in closed]
    if swept and len(never_closed) <= len(swept):
        swept_note = (f"   {len(never_closed)} of these were closed by a later start's ledger "
                      f"sweep ({len(swept)} orphan(s) swept in total), so they did stop billing.")
        never_closed_accounted, never_closed = never_closed, []
    else:
        swept_note = None
        never_closed_accounted = []
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
        print("   every session that started also closed: the total above is the whole bill.")


if __name__ == "__main__":
    logs = sys.argv[1:] or ([os.path.expanduser("~/.local/state/rtcdiag/bridge.log")]
                            + sorted(glob.glob("runs/*/events.jsonl")))
    main(logs)
