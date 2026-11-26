"""Offline regressions: no bridge, sockets, credentials, cloud or SSH."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import wave

ROOT = Path(__file__).resolve().parent.parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class OfflineRegressions(unittest.TestCase):
    def test_unrelated_sweep_does_not_clear_an_open_session(self):
        audit = load("billing-audit")
        rows = [
            {"ev": "live_session_started", "sess": "echo-test", "session_id": "live_test"},
            {"ev": "ledger_orphan_closed", "id": "live_unrelated"},
        ]
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "events.jsonl"
            path.write_text("\n".join(map(json.dumps, rows)), encoding="utf-8")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                audit.main([str(path)])
            self.assertIn("1 sessions STARTED AND NEVER CLOSED", out.getvalue())
            rows[-1]["id"] = "live_test"
            path.write_text("\n".join(map(json.dumps, rows)), encoding="utf-8")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                audit.main([str(path)])
            self.assertNotIn("STARTED AND NEVER CLOSED", out.getvalue())
            self.assertIn("matched session(s)", out.getvalue())

    def test_stereo_wav_is_refused_explicitly(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "stereo.wav"
            with wave.open(str(path), "wb") as wav:
                wav.setnchannels(2)
                wav.setsampwidth(2)
                wav.setframerate(48000)
                wav.writeframes(b"\x00" * 200)
            for name in ("audio-report", "ns-signature"):
                with self.subTest(name=name), self.assertRaisesRegex(SystemExit, "mono 16-bit"):
                    load(name).main(str(path))

    def test_truncated_ogg_header_is_reported(self):
        with self.assertRaisesRegex(ValueError, "truncated Ogg page header"):
            list(load("oggdump").pages(b"OggS"))

    def test_report_accepts_connected_event_without_since_answer(self):
        rows = [
            {"ev": "offer_received", "mono_ms": 10, "sessionId": "offline-test"},
            {"ev": "answer_ready", "mono_ms": 20},
            {"ev": "alexa_session_connected", "mono_ms": 30},
            {"ev": "first_rtp_in", "mono_ms": 40},
            {"ev": "alexa_session_disconnected", "mono_ms": 50},
        ]
        with tempfile.TemporaryDirectory() as folder:
            (Path(folder) / "events.jsonl").write_text("\n".join(map(json.dumps, rows)), encoding="utf-8")
            result = subprocess.run([sys.executable, str(ROOT / "scripts/report.py"), folder], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("+0.01s after answer", result.stdout)

    def test_digest_preserves_negation_and_numbers(self):
        digest = load("fleet-digest")
        self.assertNotEqual(digest.normalise("port 8787 is safe"), digest.normalise("port 8787 is not safe"))
        self.assertNotEqual(digest.normalise("wait 15 seconds"), digest.normalise("wait 30 seconds"))

    def test_brief_heading_in_a_fence_is_not_a_new_brief(self):
        splitter = load("split-briefs")
        with tempfile.TemporaryDirectory() as folder:
            plan = Path(folder) / "plan.md"
            out = Path(folder) / "briefs"
            plan.write_text("## BRIEF C1\nreal\n```md\n## BRIEF G2\nquoted\n```\n", encoding="utf-8")
            with contextlib.redirect_stdout(io.StringIO()):
                splitter.main(str(plan), str(out), ".")
            self.assertEqual([p.name for p in out.iterdir()], ["codex-c1.md"])

    def test_port_guard_fails_closed_and_skips_live_port(self):
        bash = shutil.which("bash")
        if os.name == "nt":
            bash = str(Path(os.environ["ProgramFiles"]) / "Git/bin/bash.exe")
        if not bash or not Path(bash).exists():
            self.skipTest("bash unavailable")
        code = r'''
source scripts/lib/safe-port.sh
ss() { return 1; }
port_busy 8801 || exit 11
if pick_free_port 8801 >/dev/null 2>&1; then exit 12; fi
ss() { return 0; }
port="$(pick_free_port 8787)" || exit 13
[ "$port" = 8788 ] || exit 14
if (assert_not_live 08787) >/dev/null 2>&1; then exit 15; fi
'''
        result = subprocess.run([bash, "-c", code], cwd=ROOT, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
