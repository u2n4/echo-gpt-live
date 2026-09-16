#!/usr/bin/env python3
"""Print the per-second transport health of one session from the bridge log.

Loss toward the Echo is reported by Amazon's own RTCP receiver reports, so it is the only
measurement of what actually arrived at the speaker. A cumulative average hides bursts, which are
exactly what a listener hears as grit, so the per-report fraction is printed too.

Usage: loss-report.py <log> <session-id-prefix>
"""
import json
import sys


def main(log, sess):
    rows = []
    for line in open(log):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if not str(e.get("sess", "")).startswith(sess) or e.get("ev") != "stats_1s":
            continue
        out = e.get("out_audio") or {}
        inn = e.get("in_audio") or {}
        pair = e.get("pair") or {}
        if not out:
            continue
        rows.append({
            "t": e.get("since_answer_ms", 0) / 1000.0,
            "sent": out.get("pkts_sent", 0),
            "lost_total": out.get("remote_lost", 0),
            "frac": out.get("remote_fraction_lost", 0.0),
            "out_jit_ms": (out.get("remote_jitter") or 0) * 1000,
            "in_lost": inn.get("lost", 0),
            "in_jit_ms": (inn.get("jitter") or 0) * 1000,
            "rtt_ms": (pair.get("rtt_s") or 0) * 1000,
        })

    if not rows:
        print("no stats samples for", sess)
        return

    head = "%6s %7s %9s %8s %9s %8s %8s %8s" % (
        "t(s)", "sent", "lost_tot", "frac", "outJitMs", "inLost", "inJitMs", "rttMs")
    print(head)
    for r in rows[::4]:
        print("%6.1f %7d %9d %8.4f %9.1f %8d %8.1f %8.1f" % (
            r["t"], r["sent"], r["lost_total"], r["frac"],
            r["out_jit_ms"], r["in_lost"], r["in_jit_ms"], r["rtt_ms"]))

    last = rows[-1]
    print()
    print("final: %d packets to the Echo, %d reported lost = %.2f%% over the call"
          % (last["sent"], last["lost_total"],
             100.0 * last["lost_total"] / max(1, last["sent"])))
    fr = sorted(r["frac"] for r in rows)
    # One row per one-second STATS SNAPSHOT, not per RTCP receiver report. Amazon sends reports
    # far less often, so consecutive snapshots repeat the same report and these counts are
    # snapshots that carried a nonzero fraction, not distinct reports. Codex caught the label.
    print("per-snapshot fraction lost: median %.4f  p90 %.4f  max %.4f  (n=%d)"
          % (fr[len(fr) // 2], fr[int(len(fr) * 0.9)], fr[-1], len(fr)))
    nz = [f for f in fr if f > 0]
    print("snapshots showing any loss: %d of %d (%.0f%%)" % (len(nz), len(fr), 100.0 * len(nz) / len(fr)))
    jit = sorted(r["out_jit_ms"] for r in rows)
    print("jitter the Echo measured on our stream: median %.1f ms, max %.1f ms"
          % (jit[len(jit) // 2], jit[-1]))
    ijit = sorted(r["in_jit_ms"] for r in rows)
    print("jitter we measured on the Echo's stream: median %.1f ms, max %.1f ms"
          % (ijit[len(ijit) // 2], ijit[-1]))


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
