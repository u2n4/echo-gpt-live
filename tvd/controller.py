"""Drives the Xiaomi box over Android TV Remote v2.

Rules that came out of the 2026-10-18 measurements and two Codex reviews:
- Only an explicit power_on sends Wake-on-LAN. Reconnecting never wakes the box.
- Power state is what the box itself last reported over the remote protocol (is_on), with the
  time it said so. An open or closed port is never taken as on or off (the box wakes by itself
  for a few seconds now and then, and a dead Wi-Fi link also closes ports).
- Commands run one at a time. A power_off cancels a power_on that started before it.
- Success is reported only after the box confirms the requested state, within the deadline;
  an unknown state is an error, never a guess, and never a reason to press POWER.
"""
import asyncio
import json
import os
import time
from typing import Any
from urllib.parse import urlparse

APPS = {
    "youtube": ("https://www.youtube.com", "com.google.android.youtube.tv"),
    "netflix": ("https://www.netflix.com/browse", "com.netflix.ninja"),
    "shahid": ("https://shahid.mbc.net", "net.mbc.shahid"),
}
ALLOWED_DEEP_LINK_HOSTS = {
    "youtube.com", "www.youtube.com", "m.youtube.com", "youtu.be",
    "netflix.com", "www.netflix.com",
    "shahid.mbc.net",
}
if _extra := os.environ.get("TVD_EXTRA_APPS", "").strip():
    try:
        for _k, _v in json.loads(_extra).items():
            if isinstance(_k, str) and isinstance(_v, list) and len(_v) == 2:
                APPS[_k.lower()] = (str(_v[0]), str(_v[1]))
    except ValueError:
        pass

# Remote keys a caller may press, by our own names; nothing else reaches send_key_command.
NAV_KEYS = {
    # D-pad, center/OK, navigation
    "up": "DPAD_UP", "down": "DPAD_DOWN", "left": "DPAD_LEFT", "right": "DPAD_RIGHT",
    "select": "DPAD_CENTER", "ok": "DPAD_CENTER", "enter": "ENTER",
    "back": "BACK", "home": "HOME", "menu": "MENU", "backspace": "DEL", "del": "DEL",
    # System & TV UI
    "settings": "SETTINGS", "search": "SEARCH", "info": "INFO",
    "captions": "CAPTIONS", "all_apps": "ALL_APPS", "app_switch": "APP_SWITCH",
    "guide": "GUIDE",
    # Media & playback
    "play": "MEDIA_PLAY", "pause": "MEDIA_PAUSE", "play_pause": "MEDIA_PLAY_PAUSE",
    "stop": "MEDIA_STOP", "fast_forward": "MEDIA_FAST_FORWARD", "rewind": "MEDIA_REWIND",
    "next": "MEDIA_NEXT", "previous": "MEDIA_PREVIOUS",
    "skip_forward": "MEDIA_SKIP_FORWARD", "skip_backward": "MEDIA_SKIP_BACKWARD",
    # Volume, channels & pages
    "volume_up": "VOLUME_UP", "volume_down": "VOLUME_DOWN", "mute_toggle": "VOLUME_MUTE",
    "channel_up": "CHANNEL_UP", "channel_down": "CHANNEL_DOWN",
    "page_up": "PAGE_UP", "page_down": "PAGE_DOWN",
}
SCREENSAVER_SUFFIX = "dreamx"  # com.google.android.apps.tv.dreamx swallowed an app launch
REMOTE_PORT = 6466
MAX_VOLUME_STEPS = 5
MAX_KEY_COUNT = 10
MAX_SEQUENCE_PRESSES = 12
DEFAULT_KEY_DELAY_MS = 80
STATE_WAIT_S = 1.5  # how long a fresh connection may take to report is_on
SETTLE_S = 3.0      # how long an unanswered POWER press may still take effect


class TVError(Exception):
    """A command could not be carried out. code is what the caller reports."""

    def __init__(self, code: str, detail: str = ""):
        super().__init__(f"{code}: {detail}" if detail else code)
        self.code = code


class Controller:
    def __init__(self, make_remote, probe, wol, clock=time.monotonic, wall=time.time, log=None,
                 state_file: str | None = None):
        """make_remote() -> AndroidTVRemote-like object; probe(port, timeout) -> awaitable bool;
        wol() sends one burst of magic packets. state_file keeps the box's last reported power
        state across restarts (a standby box never answers, so it could not be learned again)."""
        self._make_remote = make_remote
        self._probe = probe
        self._wol = wol
        self._clock = clock
        self._wall = wall
        self._log = log or (lambda *a, **k: None)
        self._lock = asyncio.Lock()
        self._off_gen = 0  # bumped by every power_off; a power_on that saw an older value stops
        self._remote: Any = None
        self._reconnecting = False  # the library's own reconnect loop is running
        self._available = False
        self._app_reports = 0  # app reports from the current connection, to tell fresh from stale
        self._last_on: bool | None = None  # last is_on the box reported, on any connection
        self._last_on_at = 0.0
        # A POWER press not yet answered: when, and which state it was pressed for. Until it lands
        # or SETTLE_S passes, nothing reads the state or presses POWER again (_settle).
        self._unconfirmed_power_at = float("-inf")
        self._pending_target: bool | None = None
        self._maintain_task = None
        self._state_file = state_file
        self._load_state()

    # ---- remembered power state -----------------------------------------------------------

    def _load_state(self) -> None:
        if not self._state_file:
            return
        try:
            with open(self._state_file) as f:
                saved = json.load(f)
            if isinstance(saved.get("on"), bool) and isinstance(saved.get("at"), (int, float)):
                self._last_on, self._last_on_at = saved["on"], float(saved["at"])
        except (OSError, ValueError, AttributeError):
            pass  # nothing remembered: the state stays unknown until the box says it

    def _record_on(self, value) -> None:
        self._last_on, self._last_on_at = bool(value), self._wall()
        if not self._state_file:
            return
        try:
            tmp = self._state_file + ".tmp"
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w") as f:
                json.dump({"on": self._last_on, "at": self._last_on_at}, f)
            os.replace(tmp, self._state_file)
        except OSError as e:
            self._log("state_save_failed", error=type(e).__name__)

    # ---- connection ---------------------------------------------------------------------

    def _attach(self, remote):
        # Only the current connection may change the state: a replaced one could still deliver a
        # late callback after disconnect().
        def on_avail(ok):
            if remote is self._remote:
                self._available = ok

        def on_is_on(value):
            if remote is self._remote:
                self._record_on(value)

        def on_app(app):
            # Logged so a launch can be traced against what the box itself put in front.
            if remote is self._remote:
                self._app_reports += 1
                self._log("app", app=app)

        remote.add_is_available_updated_callback(on_avail)
        remote.add_is_on_updated_callback(on_is_on)
        remote.add_current_app_updated_callback(on_app)

    async def _connect(self, timeout: float) -> None:
        """Fresh connection. Never sends Wake-on-LAN."""
        if self._remote is not None:
            self._remote.disconnect()  # also cancels its reconnect loop
        self._available = False
        self._reconnecting = False
        remote = self._make_remote()
        self._attach(remote)
        self._remote = remote
        await asyncio.wait_for(remote.async_connect(), max(0.1, timeout))
        self._available = True
        if remote.is_on is not None:
            self._record_on(remote.is_on)

        def on_invalid_auth():
            self._reconnecting = False
            self._log("needs_pairing")

        # Reconnects only open TCP to the box; they never send Wake-on-LAN.
        remote.keep_reconnecting(on_invalid_auth)
        self._reconnecting = True

    def start(self) -> None:
        """Background task: connect once the box is reachable, without waking it."""
        self._maintain_task = asyncio.get_running_loop().create_task(self._maintain())

    async def _maintain(self) -> None:
        delay = 5.0
        while True:
            # Once connected, the library's reconnect loop takes over; this loop only covers the
            # first connection and the case where that loop has given up.
            if not self._available and not self._reconnecting and not self._lock.locked():
                try:
                    if await self._probe(REMOTE_PORT, 1.0):
                        async with self._lock:
                            if not self._available:
                                await self._connect(5.0)
                                self._log("connected", source="maintain")
                        delay = 5.0
                except Exception as e:  # noqa: BLE001 - keep the loop alive, report it
                    self._log("connect_failed", error=type(e).__name__)
                delay = min(delay * 2, 60.0)
                await asyncio.sleep(delay)
            else:
                delay = 5.0
                await asyncio.sleep(5.0)

    async def _wait_for(self, cond, deadline: float, cancelled=None) -> bool:
        while not cond():
            if cancelled is not None and cancelled():
                raise TVError("cancelled", "power_off arrived")
            if self._clock() > deadline:
                return False
            await asyncio.sleep(0.1)
        return True

    async def _ensure_awake(self, deadline: float, cancelled) -> None:
        """Wake-on-LAN until the remote port answers, then connect. Raises TVError.
        The first burst goes out before the first probe: an asleep box never answers a probe,
        and an awake one ignores the packet."""
        self._check_deadline(deadline)
        self._wol()
        while not await self._probe(REMOTE_PORT, 0.25):
            if cancelled():
                raise TVError("cancelled", "power_off arrived while waking")
            if self._clock() > deadline:
                raise TVError("unreachable", "no answer to Wake-on-LAN")
            self._wol()
            await asyncio.sleep(0.1)
        # Only called when there is no usable connection (none, or one that missed the box
        # going to sleep), so always start a fresh one.
        await self._connect(deadline - self._clock())

    async def _reported_state(self, deadline: float, cancelled=None) -> bool:
        """The box's own is_on on the current connection, or TVError('unknown')."""
        ok = await self._wait_for(lambda: self._remote.is_on is not None,
                                  min(deadline, self._clock() + STATE_WAIT_S), cancelled)
        if not ok:
            raise TVError("unknown", "box did not report its power state")
        return bool(self._remote.is_on)

    async def _settle(self, deadline: float) -> None:
        """An earlier POWER press that went unanswered may still land, in either direction. Until
        it has (the box reports the state it was pressed for) or SETTLE_S has passed, reading the
        state and pressing POWER again would both be wrong, so refuse with 'busy' rather than
        guess. The caller's deadline never shortens the settle window itself."""
        settle_until = self._unconfirmed_power_at + SETTLE_S
        if self._clock() >= settle_until:
            return
        target = self._pending_target
        if self._available and self._remote is not None and target is not None:
            if await self._wait_for(lambda: self._remote.is_on is target, min(deadline, settle_until)):
                self._unconfirmed_power_at, self._pending_target = float("-inf"), None  # it landed
                return
        if self._clock() < settle_until:
            raise TVError("busy", "an earlier POWER press may still take effect")
        self._unconfirmed_power_at, self._pending_target = float("-inf"), None

    def _check_deadline(self, deadline: float) -> None:
        """Called right before anything with a side effect (Wake-on-LAN, POWER, a key): a command
        that waited past its deadline in the queue must not act after its caller gave up."""
        if self._clock() > deadline:
            raise TVError("timeout", "deadline passed before acting")

    async def _press_power(self, target: bool, deadline: float) -> None:
        """POWER, then wait for the box to report target. The press is marked unanswered BEFORE
        the wait, so a timeout or a cancel during the wait still leaves _settle guarding it."""
        self._check_deadline(deadline)
        self._send("POWER")
        self._unconfirmed_power_at, self._pending_target = self._clock(), target
        if not await self._wait_for(lambda: self._remote.is_on is target, deadline):
            raise TVError("timeout", "box did not report " + ("on" if target else "standby"))
        self._unconfirmed_power_at, self._pending_target = float("-inf"), None

    def _snapshot(self) -> dict:
        r = self._remote
        live = r is not None and self._available
        if self._last_on is None or (self._last_on and not live):
            # Never reported, or reported "on" but we lost it since: we do not know.
            power = "unknown"
        else:
            power = "on" if self._last_on else "standby"
        vol = r.volume_info if live else None
        return {
            "power": power,
            "connected": live,
            "observed_at": round(self._last_on_at, 3),
            "app": r.current_app if live else None,
            "volume": dict(vol) if vol else None,
        }

    def _send(self, key: str) -> None:
        if self._remote is None or not self._available:
            raise TVError("unreachable", "not connected")
        self._remote.send_key_command(key)

    # ---- commands ------------------------------------------------------------------------

    async def power_on(self, timeout: float) -> dict:
        gen = self._off_gen
        cancelled = lambda: self._off_gen != gen  # noqa: E731
        # The deadline starts now, not after the lock: time spent queued behind another command
        # counts, so this never runs past the caller's own timeout and gets cut off mid-way.
        deadline = self._clock() + timeout
        async with self._lock:
            await self._power_on_locked(deadline, cancelled)
            return self._snapshot()

    async def _power_on_locked(self, deadline: float, cancelled) -> bool:
        """The body of power_on, for a caller that already holds the lock. Returns whether the TV
        had to be switched on (it was off or asleep)."""
        if cancelled():
            raise TVError("cancelled", "power_off arrived")
        self._check_deadline(deadline)
        await self._settle(deadline)
        # Not connected: straight to waking (no probe first). Connected: a short probe tells
        # a live link from one that has not noticed the box went to sleep.
        if not (self._available and await self._probe(REMOTE_PORT, 0.3)):
            await self._ensure_awake(deadline, cancelled)
        if await self._reported_state(deadline, cancelled):
            return False
        if cancelled():
            raise TVError("cancelled", "power_off arrived")
        # Once POWER is out, a cancel must not release the lock before the box answers:
        # a power_off would read the old "off" and the toggle would land after it.
        await self._press_power(True, deadline)
        return True

    async def power_off(self, timeout: float) -> dict:
        self._off_gen += 1  # stops any power_on that started before this
        deadline = self._clock() + timeout
        async with self._lock:
            self._check_deadline(deadline)
            await self._settle(deadline)
            if not await self._probe(REMOTE_PORT, 0.7):
                if self._last_on is False:
                    return self._snapshot()  # the box said off, and it has gone quiet since
                raise TVError("unreachable", "box not answering and last known state is not off")
            if not self._available:
                await self._connect(deadline - self._clock())
            if await self._reported_state(deadline):
                await self._press_power(False, deadline)
            return self._snapshot()

    async def _require_on(self, deadline: float) -> None:
        if not self._available and await self._probe(REMOTE_PORT, 0.7):
            # Awake but not connected yet (the background loop backs off): connect, never wake.
            await self._connect(deadline - self._clock())
        if not self._available or self._remote is None:
            raise TVError("off", "TV is not on")
        if not await self._reported_state(deadline):
            raise TVError("off", "TV is not on")

    async def volume_step(self, steps, timeout: float = 3.0) -> dict:
        if not isinstance(steps, int) or isinstance(steps, bool) or steps == 0 or abs(steps) > MAX_VOLUME_STEPS:
            raise TVError("bad_request", "steps must be 1..5 or -1..-5")
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            self._check_deadline(deadline)
            key = "VOLUME_UP" if steps > 0 else "VOLUME_DOWN"
            vol = self._remote.volume_info or {}
            before, top = vol.get("level"), vol.get("max")
            for i in range(abs(steps)):
                if i:
                    await asyncio.sleep(0.12)
                    self._check_deadline(deadline)  # every press, not only the first
                self._send(key)
            # Return as soon as the box reports the level the last press should give (clamped to
            # its range, where presses stop moving it), with a 0.6 s cap. Without a known level,
            # give the box a moment instead.
            if isinstance(before, int) and isinstance(top, int):
                target = max(0, min(top, before + steps))
                await self._wait_for(lambda: (self._remote.volume_info or {}).get("level") == target,
                                     self._clock() + 0.6)
            else:
                await asyncio.sleep(0.3)
            return self._snapshot()

    async def set_mute(self, mute, timeout: float = 3.0) -> dict:
        if not isinstance(mute, bool):
            raise TVError("bad_request", "mute must be true or false")
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            muted = lambda: (self._remote.volume_info or {}).get("muted")  # noqa: E731
            if muted() is None:  # never toggle blind
                raise TVError("unknown", "box did not report its mute state")
            if muted() != mute:
                self._check_deadline(deadline)
                self._send("VOLUME_MUTE")
                if not await self._wait_for(lambda: muted() == mute, deadline):
                    raise TVError("timeout", "mute state did not change")
            return self._snapshot()

    async def toggle_mute(self, timeout: float = 3.0) -> dict:
        """Toggle mute directly with VOLUME_MUTE, even when the box does not report mute state."""
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            self._check_deadline(deadline)
            self._send("VOLUME_MUTE")
            return self._snapshot()

    async def playback(self, op: str, timeout: float = 3.0, count: int = 1,
                       delay_ms: int = DEFAULT_KEY_DELAY_MS) -> dict:
        keys = {
            "play": "MEDIA_PLAY", "pause": "MEDIA_PAUSE",
            "play_pause": "MEDIA_PLAY_PAUSE", "stop": "MEDIA_STOP",
        }
        if op not in keys:
            raise TVError("bad_request", "unknown playback operation")
        count, delay_s = self._validate_repeat(count, delay_ms)
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            for i in range(count):
                if i:
                    await asyncio.sleep(delay_s)
                self._check_deadline(deadline)
                self._send(keys[op])
            return self._snapshot()

    @staticmethod
    def _validate_repeat(count, delay_ms) -> tuple[int, float]:
        if count is None:
            count = 1
        if delay_ms is None:
            delay_ms = DEFAULT_KEY_DELAY_MS
        if (not isinstance(count, int) or isinstance(count, bool)
                or count < 1 or count > MAX_KEY_COUNT):
            raise TVError("bad_request", f"count must be 1..{MAX_KEY_COUNT}")
        if (not isinstance(delay_ms, int) or isinstance(delay_ms, bool)
                or delay_ms < 0 or delay_ms > 1000):
            raise TVError("bad_request", "delay_ms must be 0..1000")
        return count, delay_ms / 1000.0

    async def key(self, name, timeout: float = 3.0, count: int = 1,
                  delay_ms: int = DEFAULT_KEY_DELAY_MS) -> dict:
        """One or more remote-control presses from NAV_KEYS (arrows, select, media, menu, etc.)."""
        if not isinstance(name, str) or name not in NAV_KEYS:
            raise TVError("bad_request", "unknown key")
        count, delay_s = self._validate_repeat(count, delay_ms)
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            for i in range(count):
                if i:
                    await asyncio.sleep(delay_s)
                self._check_deadline(deadline)
                self._send(NAV_KEYS[name])
            return self._snapshot()

    async def sequence(self, steps, timeout: float = 5.0) -> dict:
        """Run an atomic sequence of remote key presses under one lock, e.g.
        [{"key": "down", "count": 2}, {"key": "select", "count": 1}]."""
        if not isinstance(steps, list) or len(steps) == 0:
            raise TVError("bad_request", "steps must be a non-empty list")
        expanded: list[tuple[str, float]] = []
        for step in steps:
            if not isinstance(step, dict):
                raise TVError("bad_request", "each step must be an object")
            name = step.get("key")
            if not isinstance(name, str) or name not in NAV_KEYS:
                raise TVError("bad_request", f"unknown key: {name}")
            count, delay_s = self._validate_repeat(step.get("count", 1), step.get("delay_ms", DEFAULT_KEY_DELAY_MS))
            for _ in range(count):
                expanded.append((NAV_KEYS[name], delay_s))
                if len(expanded) > MAX_SEQUENCE_PRESSES:
                    raise TVError("bad_request", f"at most {MAX_SEQUENCE_PRESSES} presses per sequence")
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            for i, (keycode, delay_s) in enumerate(expanded):
                if i and delay_s > 0:
                    await asyncio.sleep(delay_s)
                self._check_deadline(deadline)
                self._send(keycode)
            return self._snapshot()

    async def send_text(self, text, timeout: float = 8.0, pace_ms: int = 0) -> dict:
        """Type text into the currently focused search/input field on the TV.
        When pace_ms > 0 (e.g. 180 ms for Netflix's custom JS keyboard), sends one character at a
        time with a short gap so rapid bursts never drop letters."""
        if not isinstance(text, str) or not text.strip() or len(text) > 200:
            raise TVError("bad_request", "text must be 1..200 characters")
        if not isinstance(pace_ms, int) or isinstance(pace_ms, bool) or pace_ms < 0 or pace_ms > 500:
            raise TVError("bad_request", "pace_ms must be 0..500")
        deadline = self._clock() + timeout
        async with self._lock:
            await self._require_on(deadline)
            self._check_deadline(deadline)
            if not hasattr(self._remote, "send_text"):
                raise TVError("unsupported", "remote does not support text input")
            if pace_ms > 0 and len(text) > 1:
                delay_s = pace_ms / 1000.0
                for i, ch in enumerate(text):
                    if i:
                        await asyncio.sleep(delay_s)
                    self._check_deadline(deadline)
                    self._remote.send_text(ch)
            else:
                self._remote.send_text(text)
            return self._snapshot()

    async def launch(self, app, timeout: float) -> dict:
        """Open an app, switching the TV on first when it is off or asleep (e.g. "شغّل يوتيوب"
        works even with the TV off). A power_off that arrives meanwhile cancels it,
        exactly as it cancels power_on."""
        if app not in APPS:
            raise TVError("bad_request", "unknown app")
        link, package = APPS[app]
        gen = self._off_gen
        cancelled = lambda: self._off_gen != gen  # noqa: E731
        deadline = self._clock() + timeout
        async with self._lock:
            woke = await self._power_on_locked(deadline, cancelled)

            def go_on() -> None:
                # A power_off that arrived while the TV was coming on wins: stop here and let it
                # run, instead of opening the app and holding the lock past its deadline (Codex).
                if cancelled():
                    raise TVError("cancelled", "power_off arrived")
                self._check_deadline(deadline)

            self._log("launch_check", app=app, woke=woke, front=self._remote.current_app)
            # A box that has just been switched on still reports the app that was in front when
            # it went to sleep (measured 2026-10-18: "launch YouTube" with YouTube as that app
            # only switched the TV on). Only a TV that was already on may skip the launch.
            if not woke and self._remote.current_app == package:
                return self._snapshot()  # already in front: sending the link again could reset it
            if (self._remote.current_app or "").endswith(SCREENSAVER_SUFFIX):
                go_on()
                self._send("HOME")
                await self._wait_for(
                    lambda: not (self._remote.current_app or "").endswith(SCREENSAVER_SUFFIX),
                    min(deadline, self._clock() + 1.5), cancelled,
                )
            go_on()
            reports = self._app_reports
            self._remote.send_launch_app_command(link)
            in_front = lambda: self._remote.current_app == package  # noqa: E731
            # After a wake, the stale app above can already equal package: only a report sent
            # after this launch confirms it.
            confirmed = lambda: self._app_reports != reports and in_front()  # noqa: E731
            if woke and not await self._wait_for(confirmed, min(deadline, self._clock() + 1.5), cancelled):
                # A box that has just woken can drop a launch while its home screen loads: one
                # more, if there is time (launching the same app twice changes nothing).
                go_on()
                self._remote.send_launch_app_command(link)
            # The box reports every change of app (measured: launcher, then the app), so after a
            # wake a stale report never counts as success, even after the resend (Codex).
            if not await self._wait_for(confirmed if woke else in_front, deadline, cancelled):
                raise TVError("timeout", f"{package} did not come to the front")
            return self._snapshot()

    async def launch_url(self, url, timeout: float = 6.8) -> dict:
        """Open a specific deep link on an allowlisted media app (YouTube, Netflix, Shahid),
        waking the box first if needed."""
        if not isinstance(url, str) or len(url) > 512:
            raise TVError("bad_request", "invalid url")
        parsed = urlparse(url.strip())
        host = (parsed.hostname or "").lower()
        if parsed.scheme != "https" or host not in ALLOWED_DEEP_LINK_HOSTS:
            raise TVError("bad_request", f"url host not allowed: {host or 'none'}")
        clean_url = url.strip()
        gen = self._off_gen
        cancelled = lambda: self._off_gen != gen  # noqa: E731
        deadline = self._clock() + timeout
        async with self._lock:
            await self._power_on_locked(deadline, cancelled)
            if cancelled():
                raise TVError("cancelled", "power_off arrived")
            self._check_deadline(deadline)
            if (self._remote.current_app or "").endswith(SCREENSAVER_SUFFIX):
                self._send("HOME")
                await self._wait_for(
                    lambda: not (self._remote.current_app or "").endswith(SCREENSAVER_SUFFIX),
                    min(deadline, self._clock() + 1.5), cancelled,
                )
            self._check_deadline(deadline)
            self._log("launch_url", host=host)
            self._remote.send_launch_app_command(clean_url)
            return self._snapshot()

    async def state(self) -> dict:
        """Never wakes the box, never probes it, never waits on the command lock."""
        return self._snapshot()
