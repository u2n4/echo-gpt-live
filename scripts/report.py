#!/usr/bin/env python3
"""Turn one rtcdiag run into the timeline the diagnosis needs.

Usage: scripts/report.py [run-dir]        (default: newest dir under runs/)

Prints, per Alexa session: the offer/answer/connect/disconnect timeline with the exact
offset of every event from the SDP answer, what media actually flowed in each direction,
what RTCP arrived, and the verdict line to compare against the July baseline (26-28 s).
"""
import json
import os
import sys
import glob

KEEP_TIMELINE = {
    "rtcdiag_start", "offer_received", "ice_gathering_complete", "ice_gathering_timeout",
    "answer_summary", "answer_ready", "answer_delay_sleep", "local_candidate", "selected_pair",
    "ice_connection_state", "peer_connection_state", "dtls_state", "remote_track",
    "first_rtp_in", "first_video_rtp_in", "video_frame_sent", "alexa_session_connected",
    "alexa_session_disconnected", "hold_after_disconnect", "session_close", "max_session_reached",
    "audio_file_error", "video_file_error", "answer_failed", "signaling_auth_fail",
}


def load(run_dir):
    path = os.path.join(run_dir, "events.jsonl")
    out = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    out.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
    return out


def main():
    run = sys.argv[1] if len(sys.argv) > 1 else None
    if not run:
        dirs = [d for d in glob.glob("runs/*") if os.path.isdir(d)]
        if not dirs:
            sys.exit("no runs found")
        run = max(dirs, key=os.path.getmtime)
    evs = load(run)
    if not evs:
        sys.exit(f"{run}: no events")

    start = next((e for e in evs if e["ev"] == "rtcdiag_start"), None)
    print(f"== run {run}")
    if start:
        print(f"   config: {json.dumps(start.get('config', {}), sort_keys=True)}")

    # split into sessions on offer_received
    sessions, cur = [], None
    for e in evs:
        if e["ev"] == "offer_received":
            cur = []
            sessions.append(cur)
        if cur is not None:
            cur.append(e)

    if not sessions:
        print("   no Alexa offer has arrived yet in this run.")
        print("   (say the invocation phrase to the Echo while this run is active)")
        return

    for i, sess in enumerate(sessions, 1):
        sid = sess[0].get("sessionId", "?")
        print(f"\n-- session {i}  id={sid[:8]}")
        answer_ms = None
        for e in sess:
            if e["ev"] == "answer_ready":
                answer_ms = e["mono_ms"]
        rtcp = {}
        stats_final = None
        for e in sess:
            ev = e["ev"]
            if ev == "rtcp_in":
                rtcp[(e.get("side"), e.get("type"))] = rtcp.get((e.get("side"), e.get("type")), 0) + 1
                continue
            if ev == "stats_final":
                stats_final = e
            if ev == "video_frame_sent" and e.get("n", 0) not in (1, 10, 60):
                continue
            if ev not in KEEP_TIMELINE:
                continue
            rel = "" if answer_ms is None else f"{(e['mono_ms'] - answer_ms) / 1000:+7.2f}s"
            body = {k: v for k, v in e.items() if k not in ("t", "mono_ms", "ev", "sess", "config", "lines")}
            print(f"   {rel}  {ev:28s} {json.dumps(body, sort_keys=True)[:190]}")
        if rtcp:
            print(f"           RTCP received: {dict((f'{k[0]}/{k[1]}', v) for k, v in rtcp.items())}")

        disc = next((e for e in sess if e["ev"] == "alexa_session_disconnected"), None)
        conn = next((e for e in sess if e["ev"] == "alexa_session_connected"), None)
        first_in = next((e for e in sess if e["ev"] == "first_rtp_in"), None)
        vid_sent = 0
        for e in sess:
            if e["ev"] in ("stats_1s", "stats_final", "session_close"):
                vid_sent = max(vid_sent, e.get("video_frames_sent", 0) or 0)
        print("\n   VERDICT")
        print(f"     SessionConnected from Alexa : {'YES at %+.2fs' % (conn['since_answer_ms'] / 1000) if conn else 'no (Amazon docs: not always sent)'}")
        print(f"     inbound audio started       : {'%+.2fs after answer' % (first_in['since_answer_ms'] / 1000) if first_in else 'NEVER'}")
        print(f"     video frames we sent        : {vid_sent}")
        if disc:
            secs = disc["since_answer_ms"] / 1000
            base = "matches the July baseline (26-28 s) -> video did NOT change the outcome" if 24 <= secs <= 32 \
                else ("LONGER than the July baseline -> the video hypothesis is supported" if secs > 32
                      else "SHORTER than the July baseline -> different failure, inspect above")
            print(f"     Alexa disconnected at       : {secs:.2f}s after the answer  <-- {base}")
        else:
            print("     Alexa disconnected at       : not yet / never (session still open or ended locally)")
        if stats_final:
            keep = {k: v for k, v in stats_final.items() if k in ("in_pkts", "in_bytes", "out_audio", "out_video", "in_audio", "pair", "ice", "dtls", "pc")}
            print(f"     final stats: {json.dumps(keep, sort_keys=True)[:400]}")


if __name__ == "__main__":
    main()
