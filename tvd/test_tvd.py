"""Tests for tvd without a real box: python -m unittest -v test_tvd (run inside tvd/)."""
import asyncio
import hashlib
import hmac
import json
import os
import stat
import tempfile
import time
import unittest

import server
from auth import FUTURE_SKEW_S, SCOPE, Verifier, sign
from controller import Controller, TVError

KEY = b"k" * 32


class FakeBox:
    """Network side of the box: asleep boxes do not answer the probe; WoL wakes it after N bursts."""

    def __init__(self, awake=False, wake_after=2):
        self.awake = awake
        self.wake_after = wake_after
        self.wol_count = 0
        self.probes = 0

    def wol(self):
        self.wol_count += 1
        if self.wol_count >= self.wake_after:
            self.awake = True

    async def probe(self, port, timeout):
        self.probes += 1
        return self.awake


class FakeRemote:
    def __init__(self, box, is_on=False, app="com.google.android.apps.tv.launcherx",
                 report_state=True, ignore_power=False, ignore_mute=False, wrong_app=False,
                 power_delay=0.05, report_app=True):
        self.box = box
        self.is_on = None  # unknown until the box reports it after connecting
        self._is_on_initial = is_on
        self.report_state = report_state
        self.ignore_power = ignore_power
        self.ignore_mute = ignore_mute
        self.wrong_app = wrong_app
        self.power_delay = power_delay
        self.report_app = report_app  # False: the box changes app without saying so
        self.current_app = app
        self.volume_info = {"level": 9, "max": 15, "muted": False}
        self.keys = []
        self.launched = []
        self.typed = []
        self.cbs = {"on": [], "avail": []}

    def add_is_on_updated_callback(self, cb): self.cbs["on"].append(cb)
    def add_is_available_updated_callback(self, cb): self.cbs["avail"].append(cb)
    def add_current_app_updated_callback(self, cb): self.cbs.setdefault("app", []).append(cb)
    def keep_reconnecting(self, cb=None): pass
    def disconnect(self): pass

    async def async_connect(self):
        if not self.box.awake:
            raise OSError("box asleep")
        await asyncio.sleep(0.01)
        if self.report_state:
            self._set_on(self._is_on_initial)

    def _set_on(self, value):
        self.is_on = value
        for cb in self.cbs["on"]:
            cb(value)

    def send_key_command(self, key):
        self.keys.append(key)
        loop = asyncio.get_running_loop()
        if key == "POWER" and not self.ignore_power:
            loop.call_later(self.power_delay, lambda: self._set_on(not self.is_on))
        elif key == "VOLUME_UP":
            self.volume_info = dict(self.volume_info, level=self.volume_info["level"] + 1)
        elif key == "VOLUME_DOWN":
            self.volume_info = dict(self.volume_info, level=self.volume_info["level"] - 1)
        elif key == "VOLUME_MUTE" and not self.ignore_mute:
            self.volume_info = dict(self.volume_info, muted=not self.volume_info["muted"])
        elif key == "HOME":
            self._set_app("com.google.android.apps.tv.launcherx")

    def _set_app(self, app):
        self.current_app = app
        if self.report_app:
            for cb in self.cbs.get("app", []):
                cb(app)

    def send_text(self, text):
        self.typed.append(text)

    def send_launch_app_command(self, link):
        self.launched.append(link)
        if self.wrong_app:
            self._set_app("com.example.other")
        elif "youtube" in link:
            self._set_app("com.google.android.youtube.tv")
        elif "shahid" in link:
            self._set_app("net.mbc.shahid")
        else:
            self._set_app("com.netflix.ninja")


def make(box, **remote_kw):
    remotes = []

    def factory():
        r = FakeRemote(box, **remote_kw)
        remotes.append(r)
        return r

    return Controller(make_remote=factory, probe=box.probe, wol=box.wol), remotes


class AuthTests(unittest.TestCase):
    def setUp(self):
        self.now = 1_800_000_000
        self.v = Verifier(KEY, clock=lambda: self.now)

    def signed(self, body=b'{"v":1}', ts=None):
        ts = str(self.now if ts is None else ts)
        return ts, sign(KEY, ts, body), body

    def test_valid(self):
        self.assertIsNone(self.v.verify(*self.signed()))

    def test_wrong_key(self):
        ts, _, body = self.signed()
        self.assertEqual(self.v.verify(ts, sign(b"x" * 32, ts, body), body), "forbidden")

    def test_bridge_style_signature_is_rejected(self):
        # The bridge signs ts + "." + body with no scope line; that must never pass here.
        ts, _, body = self.signed()
        bridge = hmac.new(KEY, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
        self.assertEqual(self.v.verify(ts, bridge, body), "forbidden")
        self.assertTrue(SCOPE.startswith(b"tv-v1"))

    def test_tampered_body(self):
        ts, sig, _ = self.signed()
        self.assertEqual(self.v.verify(ts, sig, b'{"v":2}'), "forbidden")

    def test_stale_and_future(self):
        self.assertEqual(self.v.verify(*self.signed(ts=self.now - 31)), "stale")
        self.assertEqual(self.v.verify(*self.signed(ts=self.now + FUTURE_SKEW_S + 1)), "stale")
        self.assertIsNone(self.v.verify(*self.signed(ts=self.now + FUTURE_SKEW_S)))

    def test_signed_before_start_is_refused(self):
        v = Verifier(KEY, clock=lambda: self.now)
        self.now += 5
        ts, sig, body = self.signed(ts=self.now - 10)
        self.assertEqual(v.verify(ts, sig, body), "stale")

    def test_missing_and_malformed(self):
        _, sig, body = self.signed()
        self.assertEqual(self.v.verify(None, sig, body), "unauthenticated")
        self.assertEqual(self.v.verify("12a", sig, body), "unauthenticated")
        self.assertEqual(self.v.verify(str(self.now), None, body), "unauthenticated")
        self.assertEqual(self.v.verify(str(self.now), "zz" * 32, body), "unauthenticated")

    def test_too_large(self):
        self.assertEqual(self.v.verify(*self.signed(body=b"x" * (64 * 1024 + 1))), "too_large")

    def test_nonce_once(self):
        self.assertIsNone(self.v.claim_nonce("abcdefghijklmnop"))
        self.assertEqual(self.v.claim_nonce("abcdefghijklmnop"), "replay")
        self.assertEqual(self.v.claim_nonce("short"), "bad_request")
        self.assertEqual(self.v.claim_nonce(None), "bad_request")
        # "$" would accept the trailing newline, which the nonce file loses on reload
        self.assertEqual(self.v.claim_nonce("qrstuvwxyzabcdef\n"), "bad_request")

    def test_non_ascii_digits_and_newlines_in_headers(self):
        _, sig, body = self.signed()
        for ts in ("²", "1800000000\n", "１８００"):
            self.assertEqual(self.v.verify(ts, sig, body), "unauthenticated")
        self.assertEqual(self.v.verify(str(self.now), sig + "\n" + "0", body), "unauthenticated")

    def test_replay_blocked_across_restart_even_in_the_same_second(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "nonces")
            v1 = Verifier(KEY, clock=lambda: self.now, store_path=path)
            ts, sig, body = self.signed(ts=self.now + 3)  # future-dated, still inside the skew
            self.assertIsNone(v1.verify(ts, sig, body))
            self.assertIsNone(v1.claim_nonce("restart-nonce-0001"))
            v2 = Verifier(KEY, clock=lambda: self.now, store_path=path)  # restarted, same second
            self.assertIsNone(v2.verify(ts, sig, body))
            self.assertEqual(v2.claim_nonce("restart-nonce-0001"), "replay")
            self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
            self.now += 60  # expired entries are dropped on the next load
            Verifier(KEY, clock=lambda: self.now, store_path=path)
            with open(path) as f:
                self.assertEqual(f.read(), "")

    def test_short_key_refused(self):
        with self.assertRaises(ValueError):
            Verifier(b"short")


class ControllerTests(unittest.IsolatedAsyncioTestCase):
    async def test_power_on_from_standby(self):
        box = FakeBox(awake=False, wake_after=2)
        ctl, remotes = make(box, is_on=False)
        state = await ctl.power_on(3.0)
        self.assertEqual(state["power"], "on")
        self.assertGreaterEqual(box.wol_count, 2)
        self.assertEqual(remotes[-1].keys, ["POWER"])

    async def test_power_on_when_already_on_sends_nothing(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(3.0)  # first call connects
        remotes[-1].keys.clear()
        box.wol_count = 0
        state = await ctl.power_on(3.0)
        self.assertEqual(state["power"], "on")
        self.assertEqual(remotes[-1].keys, [])
        self.assertEqual(box.wol_count, 0)

    async def test_unknown_state_never_presses_power(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, report_state=False)
        for op in (ctl.power_on, ctl.power_off):
            with self.assertRaises(TVError) as cm:
                await op(3.0)
            self.assertEqual(cm.exception.code, "unknown")
        self.assertTrue(all(r.keys == [] for r in remotes))

    async def test_ignored_power_is_a_timeout_not_a_success(self):
        box = FakeBox(awake=True)
        ctl, _ = make(box, is_on=False, ignore_power=True)
        with self.assertRaises(TVError) as cm:
            await ctl.power_on(1.0)
        self.assertEqual(cm.exception.code, "timeout")

    async def test_power_on_unreachable_fails_inside_deadline(self):
        box = FakeBox(awake=False, wake_after=10**9)
        ctl, _ = make(box)
        t0 = time.monotonic()
        with self.assertRaises(TVError) as cm:
            await ctl.power_on(0.8)
        self.assertEqual(cm.exception.code, "unreachable")
        self.assertLess(time.monotonic() - t0, 2.0)

    async def test_power_off_cancels_a_wake_in_progress(self):
        box = FakeBox(awake=False, wake_after=10**9)
        ctl, _ = make(box)
        ctl._last_on = False  # the box last said "off", so a quiet box is standby
        on = asyncio.create_task(ctl.power_on(5.0))
        await asyncio.sleep(0.3)
        off = await ctl.power_off(2.0)
        self.assertEqual(off["power"], "standby")
        with self.assertRaises(TVError) as cm:
            await on
        self.assertEqual(cm.exception.code, "cancelled")

    async def test_power_off_after_power_was_pressed_waits_for_it(self):
        # Codex N1: cancelling after POWER went out let power_off read the old "off", and the
        # toggle then turned the box on behind it.
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=False)
        await ctl.state()
        on = asyncio.create_task(ctl.power_on(3.0))
        while not (remotes and remotes[-1].keys):
            await asyncio.sleep(0.005)  # POWER has been sent; the box answers 50 ms later
        off = await ctl.power_off(3.0)
        self.assertEqual((await on)["power"], "on")
        self.assertEqual(off["power"], "standby")
        self.assertEqual(remotes[-1].keys, ["POWER", "POWER"])
        await asyncio.sleep(0.2)
        self.assertIs(remotes[-1].is_on, False)

    async def test_a_late_power_press_is_never_guessed_over(self):
        # Codex round 3: POWER lands after 1.2 s; power_on(0.5) times out; a quick power_off
        # must not say "standby" (the box is about to turn on) and must not press POWER blind.
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=False, power_delay=1.2)
        await ctl.state()
        with self.assertRaises(TVError) as cm:
            await ctl.power_on(0.5)
        self.assertEqual(cm.exception.code, "timeout")
        with self.assertRaises(TVError) as cm:
            await ctl.power_off(0.3)
        self.assertEqual(cm.exception.code, "busy")
        self.assertEqual(remotes[-1].keys, ["POWER"])
        off = await ctl.power_off(3.0)  # waits for the late press to land, then turns it off
        self.assertEqual(off["power"], "standby")
        self.assertEqual(remotes[-1].keys, ["POWER", "POWER"])
        await asyncio.sleep(1.4)
        self.assertIs(remotes[-1].is_on, False)

    async def test_an_unanswered_power_off_is_guarded_too(self):
        # Grok: only a late "on" was guarded; a late "off" let the next POWER toggle it back.
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True, power_delay=1.2)
        await ctl.state()
        with self.assertRaises(TVError) as cm:
            await ctl.power_off(0.5)
        self.assertEqual(cm.exception.code, "timeout")
        with self.assertRaises(TVError) as cm:
            await ctl.power_on(0.3)
        self.assertEqual(cm.exception.code, "busy")
        self.assertEqual(remotes[-1].keys, ["POWER"])

    async def test_a_command_that_expired_in_the_queue_does_nothing(self):
        # Codex: power_on(0.8) waited 0.85 s behind the lock, then still pressed POWER.
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=False)
        await ctl.state()
        async with ctl._lock:  # another command holds the box
            on = asyncio.create_task(ctl.power_on(0.3))
            key = asyncio.create_task(ctl.key("right", timeout=0.3))
            await asyncio.sleep(0.5)
        with self.assertRaises(TVError) as cm:
            await on
        self.assertEqual(cm.exception.code, "timeout")
        with self.assertRaises(TVError) as cm:
            await key
        self.assertIn(cm.exception.code, ("timeout", "off"))  # either way: nothing pressed
        self.assertTrue(all(r.keys == [] for r in remotes))
        self.assertEqual(box.wol_count, 0)

    async def test_power_state_survives_a_restart(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "last_power.json")
            box = FakeBox(awake=True)
            ctl = Controller(make_remote=lambda: FakeRemote(box, is_on=True), probe=box.probe,
                             wol=box.wol, state_file=path)
            await ctl.power_off(2.0)
            self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
            box.awake = False  # asleep: a restarted tvd cannot ask it
            ctl2 = Controller(make_remote=lambda: FakeRemote(box), probe=box.probe, wol=box.wol, state_file=path)
            self.assertEqual((await ctl2.state())["power"], "standby")
            self.assertEqual((await ctl2.power_off(1.0))["power"], "standby")

    async def test_a_replaced_connection_cannot_change_the_state(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(2.0)          # first connection, reports on
        old = remotes[-1]
        await ctl._connect(2.0)          # replaced by a second connection
        self.assertEqual(len(remotes), 2)
        old._set_on(False)               # a late callback from the replaced connection
        self.assertEqual((await ctl.state())["power"], "on")

    async def test_power_on_after_a_finished_power_off_is_not_cancelled(self):
        box = FakeBox(awake=True)
        ctl, _ = make(box, is_on=True)
        await ctl.power_off(2.0)
        state = await ctl.power_on(2.0)
        self.assertEqual(state["power"], "on")

    async def test_power_off_when_quiet_needs_a_last_known_off(self):
        box = FakeBox(awake=False)
        ctl, remotes = make(box)
        with self.assertRaises(TVError) as cm:
            await ctl.power_off(2.0)  # never heard from the box: cannot claim standby
        self.assertEqual(cm.exception.code, "unreachable")
        ctl._last_on = False
        self.assertEqual((await ctl.power_off(2.0))["power"], "standby")
        self.assertEqual(remotes, [])

    async def test_power_off_when_on(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        state = await ctl.power_off(2.0)
        self.assertEqual(state["power"], "standby")
        self.assertEqual(remotes[-1].keys, ["POWER"])

    async def test_waking_always_reconnects(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_off(2.0)
        box.awake = False  # it went to sleep; our old connection still looks alive
        state = await ctl.power_on(3.0)
        self.assertEqual(state["power"], "on")
        self.assertEqual(len(remotes), 2)

    async def test_state_and_maintain_never_wake_or_probe(self):
        box = FakeBox(awake=False, wake_after=1)
        ctl, _ = make(box)
        self.assertEqual((await ctl.state())["power"], "unknown")
        self.assertEqual(box.probes, 0)
        ctl.start()
        await asyncio.sleep(0.2)
        ctl._maintain_task.cancel()
        self.assertEqual(box.wol_count, 0)

    async def test_lost_connection_after_on_is_unknown(self):
        box = FakeBox(awake=True)
        ctl, _ = make(box, is_on=True)
        await ctl.power_on(2.0)
        ctl._available = False  # Wi-Fi gone, last word was "on"
        self.assertEqual((await ctl.state())["power"], "unknown")

    async def test_volume_steps_and_bounds(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(2.0)
        state = await ctl.volume_step(-2)
        self.assertEqual(remotes[-1].keys, ["VOLUME_DOWN", "VOLUME_DOWN"])
        self.assertEqual(state["volume"]["level"], 7)
        for bad in (0, 6, -6, True, "2", None):
            with self.assertRaises(TVError):
                await ctl.volume_step(bad)

    async def test_commands_need_the_tv_on(self):
        box = FakeBox(awake=True)
        ctl, _ = make(box, is_on=False)
        with self.assertRaises(TVError) as cm:
            await ctl.playback("pause")
        self.assertEqual(cm.exception.code, "off")

    async def test_navigation_keys_are_an_allowlist(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(2.0)
        for k in ("right", "left", "up", "down", "select", "back", "home"):
            await ctl.key(k)
        self.assertEqual(remotes[-1].keys, ["DPAD_RIGHT", "DPAD_LEFT", "DPAD_UP", "DPAD_DOWN",
                                            "DPAD_CENTER", "BACK", "HOME"])
        for bad in ("POWER", "DPAD_UP", "power", "", None, ["up"], "__class__"):
            with self.assertRaises(TVError):
                await ctl.key(bad)
        box2 = FakeBox(awake=True)
        ctl2, _ = make(box2, is_on=False)
        with self.assertRaises(TVError) as cm:
            await ctl2.key("right")
        self.assertEqual(cm.exception.code, "off")

    async def test_mute_is_idempotent_and_verified(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(2.0)
        await ctl.set_mute(True)
        await ctl.set_mute(True)
        self.assertEqual(remotes[-1].keys.count("VOLUME_MUTE"), 1)
        box2 = FakeBox(awake=True)
        ctl2, _ = make(box2, is_on=True, ignore_mute=True)
        with self.assertRaises(TVError) as cm:
            await ctl2.set_mute(True, timeout=1.0)
        self.assertEqual(cm.exception.code, "timeout")

    async def test_launch_leaves_screensaver_and_checks_the_app(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True, app="com.google.android.apps.tv.dreamx")
        await ctl.power_on(2.0)
        state = await ctl.launch("youtube", 2.0)
        self.assertEqual(remotes[-1].keys, ["HOME"])
        self.assertEqual(state["app"], "com.google.android.youtube.tv")
        with self.assertRaises(TVError):
            await ctl.launch("calculator", 2.0)
        box2 = FakeBox(awake=True)
        ctl2, _ = make(box2, is_on=True, wrong_app=True)
        with self.assertRaises(TVError) as cm:
            await ctl2.launch("netflix", 1.0)
        self.assertEqual(cm.exception.code, "timeout")

    async def test_launch_switches_a_sleeping_tv_on_first(self):
        # "شغّل يوتيوب" with the TV off wakes it, switches it on, then opens the app.
        box = FakeBox(awake=False, wake_after=2)
        ctl, remotes = make(box, is_on=False)
        state = await ctl.launch("youtube", 4.0)
        self.assertGreaterEqual(box.wol_count, 2)
        self.assertEqual(remotes[-1].keys, ["POWER"])
        self.assertEqual(remotes[-1].launched, ["https://www.youtube.com"])
        self.assertEqual(state["power"], "on")
        self.assertEqual(state["app"], "com.google.android.youtube.tv")

    async def test_launch_after_a_wake_ignores_the_app_from_before_sleep(self):
        # Measured 2026-10-18: a box in standby still reports the app that was in front when it
        # went to sleep. "Launch YouTube" with YouTube as that stale app only switched the TV on.
        box = FakeBox(awake=False, wake_after=2)
        ctl, remotes = make(box, is_on=False, app="com.google.android.youtube.tv")
        state = await ctl.launch("youtube", 4.0)
        self.assertEqual(remotes[-1].keys, ["POWER"])
        self.assertEqual(remotes[-1].launched, ["https://www.youtube.com"])
        self.assertEqual(state["app"], "com.google.android.youtube.tv")

    async def test_launch_after_a_wake_never_takes_the_stale_app_as_success(self):
        # The same stale app and no app report after either link (both dropped): one resend, then
        # an honest timeout instead of "ok" with the TV on its home screen (Codex).
        box = FakeBox(awake=False, wake_after=2)
        ctl, remotes = make(box, is_on=False, app="com.google.android.youtube.tv", report_app=False)
        with self.assertRaises(TVError) as cm:
            await ctl.launch("youtube", 3.0)
        self.assertEqual(cm.exception.code, "timeout")
        self.assertEqual(remotes[-1].launched, ["https://www.youtube.com"] * 2)

    async def test_launch_on_an_awake_standby_box_presses_power_then_opens(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=False)
        state = await ctl.launch("netflix", 3.0)
        self.assertEqual(remotes[-1].keys, ["POWER"])
        self.assertEqual(state["app"], "com.netflix.ninja")
        self.assertEqual(box.wol_count, 1)  # the first burst always goes out; harmless when awake

    async def test_power_off_cancels_a_launch_that_is_still_waking(self):
        box = FakeBox(awake=False, wake_after=10**9)
        ctl, remotes = make(box)
        ctl._last_on = False
        task = asyncio.create_task(ctl.launch("youtube", 5.0))
        await asyncio.sleep(0.3)
        await ctl.power_off(2.0)
        with self.assertRaises(TVError) as cm:
            await task
        self.assertEqual(cm.exception.code, "cancelled")
        self.assertTrue(all(r.launched == [] for r in remotes))

    async def test_power_off_during_a_launch_turns_the_tv_off(self):
        # Codex: the TV came on, the app was slow, power_off arrived: the TV must end up off.
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=False, wrong_app=True)  # the app never comes to the front
        task = asyncio.create_task(ctl.launch("youtube", 6.8))
        while not (remotes and remotes[-1].is_on):
            await asyncio.sleep(0.02)  # it is on now, and waiting for the app
        off = await ctl.power_off(5.5)
        with self.assertRaises(TVError) as cm:
            await task
        self.assertEqual(cm.exception.code, "cancelled")
        self.assertEqual(off["power"], "standby")
        self.assertIs(remotes[-1].is_on, False)

    async def test_launch_of_the_app_already_in_front_sends_nothing(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(2.0)
        await ctl.launch("youtube", 2.0)
        await ctl.launch("youtube", 2.0)
        self.assertEqual(remotes[-1].launched, ["https://www.youtube.com"])

    async def test_mute_never_toggles_blind(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.power_on(2.0)
        remotes[-1].volume_info = None
        with self.assertRaises(TVError) as cm:
            await ctl.set_mute(True)
        self.assertEqual(cm.exception.code, "unknown")
        self.assertNotIn("VOLUME_MUTE", remotes[-1].keys)


class FakeLog:
    def __init__(self):
        self.lines = []

    def __call__(self, ev, **f):
        self.lines.append((ev, f))

    def rejected(self, **f):
        self.lines.append(("rejected", f))


class ServerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        from aiohttp import web
        from aiohttp.test_utils import TestClient, TestServer

        self.calls = []

        class FakeCtl:
            async def volume_step(inner, steps, timeout):
                self.calls.append(("volume_step", steps))
                await asyncio.sleep(0.3)
                return {"power": "on"}

            async def power_on(inner, timeout):
                self.calls.append(("power_on", timeout))
                return {"power": "on"}

            async def state(inner):
                return {"power": "standby"}

        self.log = FakeLog()
        app = web.Application()
        app.router.add_post("/tv/cmd", server.make_handler(Verifier(KEY), FakeCtl(), self.log, server.MessageIds()))
        self.client = TestClient(TestServer(app))
        await self.client.start_server()

    async def asyncTearDown(self):
        await self.client.close()

    async def post(self, payload, key=KEY, ts=None):
        body = json.dumps(payload).encode()
        ts = str(int(time.time()) if ts is None else ts)
        return await self.client.post("/tv/cmd", data=body, headers={
            "X-TV-Timestamp": ts, "X-TV-Signature": sign(key, ts, body)})

    def req(self, action="state", nonce="n" * 16, **kw):
        return {"v": 1, "nonce": nonce, "action": action, **kw}

    async def test_signed_state(self):
        r = await self.post(self.req())
        self.assertEqual(r.status, 200)
        self.assertEqual((await r.json())["state"]["power"], "standby")

    async def test_unsigned_and_wrong_key(self):
        r = await self.client.post("/tv/cmd", data=b"{}")
        self.assertEqual(r.status, 401)
        r = await self.client.post("/tv/cmd", data=b"{}", headers={  # Codex N2: used to be a 500
            "X-TV-Timestamp": "²", "X-TV-Signature": "a" * 64})
        self.assertEqual(r.status, 401)
        r = await self.post(self.req(), key=b"z" * 32)
        self.assertEqual(r.status, 403)

    async def test_replay(self):
        self.assertEqual((await self.post(self.req())).status, 200)
        self.assertEqual((await self.post(self.req())).status, 409)

    async def test_unknown_action(self):
        r = await self.post(self.req(action="shell", nonce="m" * 16))
        self.assertEqual(r.status, 400)

    async def test_oversized_bodies(self):
        ts = str(int(time.time()))
        headers = {"X-TV-Timestamp": ts, "X-TV-Signature": "a" * 64}
        r = await self.client.post("/tv/cmd", data=b"x" * (64 * 1024 + 10), headers=headers)
        self.assertEqual(r.status, 413)

        async def chunks():
            for _ in range(20):
                yield b"y" * 8192

        r = await self.client.post("/tv/cmd", data=chunks(), headers=headers)  # chunked, no length
        self.assertEqual(r.status, 413)

    async def test_slow_body(self):
        old = server.BODY_READ_S
        server.BODY_READ_S = 0.2
        try:
            async def trickle():
                yield b"{"
                await asyncio.sleep(1.0)
                yield b"}"

            ts = str(int(time.time()))
            r = await self.client.post("/tv/cmd", data=trickle(),
                                       headers={"X-TV-Timestamp": ts, "X-TV-Signature": "a" * 64})
            self.assertEqual(r.status, 408)
        finally:
            server.BODY_READ_S = old

    async def test_rejections_are_logged_at_a_bounded_rate(self):
        for _ in range(5):
            await self.client.post("/tv/cmd", data=b"{}")
        with tempfile.TemporaryDirectory() as d:
            log = server.EventLog(os.path.join(d, "e.jsonl"))
            for _ in range(5):
                log.rejected(reason="x", status=401)
            log.close()
            with open(os.path.join(d, "e.jsonl")) as f:
                self.assertEqual(len(f.read().splitlines()), 1)

    async def test_deadline_shortens_or_refuses(self):
        r = await self.post(self.req("power_on", nonce="p" * 16, deadline=time.time() + 0.5))
        self.assertEqual((await r.json())["error"], "timeout")
        self.assertEqual(self.calls, [])
        r = await self.post(self.req("power_on", nonce="q" * 16, deadline=time.time() + 4.0))
        self.assertTrue((await r.json())["ok"])
        self.assertLess(self.calls[-1][1], 4.0)
        r = await self.post(self.req("power_on", nonce="r" * 16, deadline=time.time() + 3600))
        self.assertEqual(r.status, 400)

    async def test_duplicate_alexa_message_runs_once(self):
        a, b = await asyncio.gather(
            self.post(self.req("volume_step", nonce="a" * 16, msg_id="M1", args={"steps": 1})),
            self.post(self.req("volume_step", nonce="b" * 16, msg_id="M1", args={"steps": 1})),
        )
        self.assertEqual(self.calls, [("volume_step", 1)])
        dups = sorted([(await a.json())["duplicate"], (await b.json())["duplicate"]])
        self.assertEqual(dups, [False, True])

    async def test_requests_beyond_the_cap_are_refused_before_their_body(self):
        from aiohttp import web
        from aiohttp.test_utils import TestClient, TestServer

        release = asyncio.Event()

        class SlowCtl:
            async def state(inner):
                await release.wait()
                return {"power": "on"}

        app = web.Application()
        app.router.add_post("/tv/cmd", server.make_handler(Verifier(KEY), SlowCtl(), FakeLog(), server.MessageIds(),
                                                           max_in_flight=1))
        client = TestClient(TestServer(app))
        await client.start_server()
        try:
            def signed(nonce):
                body = json.dumps({"v": 1, "nonce": nonce, "action": "state"}).encode()
                ts = str(int(time.time()))
                return client.post("/tv/cmd", data=body, headers={"X-TV-Timestamp": ts, "X-TV-Signature": sign(KEY, ts, body)})
            first = asyncio.create_task(signed("s" * 16))
            await asyncio.sleep(0.1)
            second = await signed("t" * 16)
            self.assertEqual(second.status, 503)
            release.set()
            self.assertEqual((await first).status, 200)
        finally:
            await client.close()

    async def test_a_caller_that_hangs_up_cancels_its_command(self):
        from aiohttp import web
        from aiohttp.test_utils import TestClient, TestServer

        state = {"cancelled": False}

        class SlowCtl:
            async def key(inner, name, timeout):
                try:
                    await asyncio.sleep(5)
                except asyncio.CancelledError:
                    state["cancelled"] = True
                    raise
                return {"power": "on"}

        app = web.Application()
        app.router.add_post("/tv/cmd", server.make_handler(Verifier(KEY), SlowCtl(), FakeLog(), server.MessageIds()))
        client = TestClient(TestServer(app, handler_cancellation=True))
        await client.start_server()
        try:
            body = json.dumps({"v": 1, "nonce": "u" * 16, "action": "key", "args": {"key": "right"}}).encode()
            ts = str(int(time.time()))
            req = asyncio.create_task(client.post("/tv/cmd", data=body, headers={
                "X-TV-Timestamp": ts, "X-TV-Signature": sign(KEY, ts, body)}))
            await asyncio.sleep(0.3)
            req.cancel()  # the caller gives up and closes its connection
            for _ in range(30):
                if state["cancelled"]:
                    break
                await asyncio.sleep(0.05)
            self.assertTrue(state["cancelled"], "the command kept running after its caller left")
        finally:
            await client.close()

    async def test_a_duplicate_giving_up_does_not_break_the_first(self):
        first = asyncio.create_task(self.post(self.req("volume_step", nonce="c" * 16, msg_id="M2", args={"steps": 1})))
        await asyncio.sleep(0.05)
        with self.assertRaises(asyncio.TimeoutError):
            await asyncio.wait_for(
                self.post(self.req("volume_step", nonce="d" * 16, msg_id="M2", args={"steps": 1})), 0.05)
        r = await first
        self.assertEqual(r.status, 200)
        self.assertTrue((await r.json())["ok"])
        self.assertFalse(any(ev == "error" for ev, _ in self.log.lines))


class ExtendedRemoteTests(unittest.IsolatedAsyncioTestCase):
    async def test_key_repeat_and_play_pause_and_sequence_and_text(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.key("up", count=2, delay_ms=0)
        await ctl.key("select", count=2, delay_ms=0)
        await ctl.playback("play_pause", count=2, delay_ms=0)
        await ctl.toggle_mute()
        await ctl.sequence([{"key": "down", "count": 2, "delay_ms": 0}, {"key": "right", "count": 1, "delay_ms": 0}, {"key": "menu", "count": 1, "delay_ms": 0}])
        await ctl.send_text("سورة البقرة")
        await ctl.send_text("abc", pace_ms=1)
        self.assertEqual(remotes[0].keys, [
            "DPAD_UP", "DPAD_UP",
            "DPAD_CENTER", "DPAD_CENTER",
            "MEDIA_PLAY_PAUSE", "MEDIA_PLAY_PAUSE",
            "VOLUME_MUTE",
            "DPAD_DOWN", "DPAD_DOWN", "DPAD_RIGHT", "MENU",
        ])
        self.assertEqual(remotes[0].typed, ["سورة البقرة", "a", "b", "c"])

    async def test_sequence_cap_enforced(self):
        box = FakeBox(awake=True)
        ctl, _ = make(box, is_on=True)
        with self.assertRaises(TVError) as ctx:
            await ctl.sequence([{"key": "down", "count": 10}, {"key": "up", "count": 5}])
        self.assertEqual(ctx.exception.code, "bad_request")

    async def test_launch_shahid(self):
        box = FakeBox(awake=True)
        ctl, _ = make(box, is_on=True)
        st = await ctl.launch("shahid", 5.0)
        self.assertEqual(st["app"], "net.mbc.shahid")

    async def test_launch_url_allowlist_and_rejection(self):
        box = FakeBox(awake=True)
        ctl, remotes = make(box, is_on=True)
        await ctl.launch_url("https://www.youtube.com/watch?v=dQw4w9WgXcQ", 5.0)
        await ctl.launch_url("https://www.netflix.com/title/80192098?source=30", 5.0)
        self.assertEqual(remotes[0].launched, [
            "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
            "https://www.netflix.com/title/80192098?source=30",
        ])
        for bad in ("http://www.youtube.com/watch?v=1", "https://evil.example/x", "file:///etc/passwd", ""):
            with self.assertRaises(TVError) as ctx:
                await ctl.launch_url(bad, 5.0)
            self.assertEqual(ctx.exception.code, "bad_request")

    async def test_huawei_monkey_script_and_aliases(self):
        import huawei
        self.assertEqual(huawei.normalize_input("cameras"), "hdmi2")
        self.assertEqual(huawei.normalize_input("xiaomi"), "hdmi3")
        self.assertEqual(huawei.normalize_input("HDMI 2"), "hdmi2")
        self.assertEqual(huawei.normalize_input("bad"), "")
        s2 = huawei.build_monkey_script(1)
        self.assertEqual(s2.count("KEYCODE_DPAD_LEFT"), 3)
        self.assertEqual(s2.count("KEYCODE_DPAD_RIGHT"), 1)
        self.assertIn("KEYCODE_DPAD_CENTER", s2)
        s3 = huawei.build_monkey_script(2)
        self.assertEqual(s3.count("KEYCODE_DPAD_RIGHT"), 2)

    async def test_server_switch_input_and_screenshot(self):
        from aiohttp import web
        from aiohttp.test_utils import TestClient, TestServer

        class FakeCtl:
            async def state(self):
                return {"power": "on"}

        class FakeHuawei:
            async def switch_input(self, target, timeout):
                return {"input": "hdmi2", "index": 1}

            async def screenshot(self, max_width, quality, timeout):
                return {"image_b64": "AAAA", "input": "hdmi3"}

        app = web.Application()
        app.router.add_post("/tv/cmd", server.make_handler(
            Verifier(KEY), FakeCtl(), FakeLog(), server.MessageIds(), huawei=FakeHuawei()))
        client = TestClient(TestServer(app))
        await client.start_server()
        try:
            for nonce, action, args in (
                ("h" * 16, "switch_input", {"input": "cameras"}),
                ("i" * 16, "screenshot", {}),
            ):
                body = json.dumps({"v": 1, "nonce": nonce, "action": action, "args": args}).encode()
                ts = str(int(time.time()))
                r = await client.post("/tv/cmd", data=body, headers={
                    "X-TV-Timestamp": ts, "X-TV-Signature": sign(KEY, ts, body)})
                self.assertEqual(r.status, 200)
                data = await r.json()
                self.assertTrue(data["ok"])
                if action == "switch_input":
                    self.assertEqual(data["input"], "hdmi2")
                else:
                    self.assertEqual(data["image_b64"], "AAAA")
        finally:
            await client.close()


if __name__ == "__main__":
    unittest.main()
