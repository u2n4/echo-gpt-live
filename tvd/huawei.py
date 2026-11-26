"""Huawei Vision TV ADB controller for HDMI input switching and fast screen capture.

Configured via TVD_HUAWEI_HOST (e.g. "192.168.x.x:5555"). When unset, HDMI switching and
screenshots return TVError("unconfigured") so the daemon keeps working for Xiaomi-only setups.

Why monkey scripts instead of repeated `adb shell input`:
Each `adb shell input` invocation spawns a fresh Android JVM (~1 s each). A single `monkey -f`
script starts one JVM and executes HOME -> tap CHANGE -> 3x LEFT -> N x RIGHT -> CENTER in ~2.5 s,
always landing cleanly in fullscreen com.huawei.tvplayer/.MainActivity with NO Huawei menu left on
screen.

Why raw `screencap` piped to `ffmpeg` instead of `screencap -p`:
PNG encoding on the TV CPU takes ~6.3 s (~4 MB). Raw RGBA `screencap` + host-side `ffmpeg` JPEG
resize (1024px wide) takes ~2.0 s total and produces a crisp ~45-90 KB JPEG ready for Vision APIs.
"""
import asyncio
import base64
import shutil
import struct
import tempfile
import time
import os
from pathlib import Path

from controller import TVError

# Canonical input name -> card index in com.huawei.tvplayer/.InputSourceListView (left to right).
INPUT_INDEX = {
    "hdmi1": 0,
    "hdmi2": 1,
    "hdmi3": 2,
    "av": 3,
}

# Friendly aliases accepted by switch_input.
INPUT_ALIASES = {
    "hdmi1": "hdmi1", "hdmi 1": "hdmi1", "hdmi_1": "hdmi1", "1": "hdmi1",
    "hdmi2": "hdmi2", "hdmi 2": "hdmi2", "hdmi_2": "hdmi2", "2": "hdmi2",
    "cameras": "hdmi2", "camera": "hdmi2", "cam": "hdmi2",
    "hdmi3": "hdmi3", "hdmi 3": "hdmi3", "hdmi_3": "hdmi3", "3": "hdmi3",
    "xiaomi": "hdmi3", "mi": "hdmi3", "box": "hdmi3", "living room": "hdmi3", "tv": "hdmi3",
    "av": "av", "4": "av",
}

# Coordinates of the "CHANGE" button on Huawei Vision Home (1920x1080 launcher).
CHANGE_BTN_X = 196
CHANGE_BTN_Y = 672


def normalize_input(name: str) -> str:
    if not isinstance(name, str):
        return ""
    return INPUT_ALIASES.get(name.strip().lower(), "")


def build_monkey_script(target_index: int) -> str:
    """Build a deterministic monkey script that switches to card target_index (0..3) and enters
    fullscreen com.huawei.tvplayer/.MainActivity with no Huawei menu overlay."""
    lines = [
        "type= user",
        "count= 1",
        "speed= 1.0",
        "start data >>",
        "DispatchPress(KEYCODE_HOME)",
        "UserWait(620)",
        f"Tap({CHANGE_BTN_X}, {CHANGE_BTN_Y})",
        "UserWait(680)",
        # Reset focus deterministically to leftmost card (HDMI 1, index 0).
        "DispatchPress(KEYCODE_DPAD_LEFT)",
        "UserWait(100)",
        "DispatchPress(KEYCODE_DPAD_LEFT)",
        "UserWait(100)",
        "DispatchPress(KEYCODE_DPAD_LEFT)",
        "UserWait(100)",
    ]
    for _ in range(target_index):
        lines.append("DispatchPress(KEYCODE_DPAD_RIGHT)")
        lines.append("UserWait(110)")
    lines.append("UserWait(80)")
    lines.append("DispatchPress(KEYCODE_DPAD_CENTER)")
    return "\n".join(lines) + "\n"


# Values read from Huawei's TV service, independent of the input picker card order.
SOURCE_IDS = {9: "hdmi1", 10: "hdmi2", 11: "hdmi3"}
INPUT_HELPER = "/data/local/tmp/tvd-huawei-input.dex"


class HuaweiTV:
    """Async ADB wrapper for a Huawei Vision TV."""

    def __init__(self, host: str, adb_bin: str = "adb", ffmpeg_bin: str = "ffmpeg", log=None):
        self.host = host.strip()
        self.adb = adb_bin
        self.ffmpeg = ffmpeg_bin
        self._log = log or (lambda *a, **k: None)
        self._lock = asyncio.Lock()
        self._pushed: set[int] = set()
        self._current_input: str = ""
        self._last_ok_at: float = 0.0

    async def _exec(self, *args: str, timeout: float = 5.0, stdin_data: bytes | None = None) -> tuple[int, bytes, bytes]:
        proc = await asyncio.create_subprocess_exec(
            *args,
            stdin=asyncio.subprocess.PIPE if stdin_data is not None else asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        try:
            out, err = await asyncio.wait_for(proc.communicate(stdin_data), timeout)
        except asyncio.TimeoutError:
            proc.kill()
            await proc.wait()
            raise TVError("timeout", f"adb timed out ({args[0]})") from None
        except BaseException:
            proc.kill()
            await proc.wait()
            raise
        return proc.returncode or 0, out, err

    async def _ensure_connected(self, timeout: float = 2.5, force: bool = False) -> None:
        if not self.host:
            raise TVError("unconfigured", "TVD_HUAWEI_HOST is not set")
        if not force and self._last_ok_at and (time.monotonic() - self._last_ok_at) < 20.0:
            return
        # Note: adb requires `-s <host>` BEFORE the subcommand `get-state`!
        rc, out, _ = await self._exec(self.adb, "-s", self.host, "get-state", timeout=min(timeout, 1.2))
        if rc == 0 and b"device" in out:
            self._last_ok_at = time.monotonic()
            return
        rc, out, err = await self._exec(self.adb, "connect", self.host, timeout=timeout)
        txt = (out + err).decode("utf-8", errors="replace").lower()
        if rc == 0 and ("connected" in txt or "already" in txt):
            self._last_ok_at = time.monotonic()
            return
        # Stale ADB TCP socket: disconnect and retry connect once cleanly.
        await self._exec(self.adb, "disconnect", self.host, timeout=1.0)
        rc, out, err = await self._exec(self.adb, "connect", self.host, timeout=timeout)
        txt = (out + err).decode("utf-8", errors="replace").lower()
        if rc != 0 or ("connected" not in txt and "already" not in txt):
            self._last_ok_at = 0.0
            raise TVError("unreachable", f"huawei adb connect failed: {txt.strip()[:120]}")
        self._last_ok_at = time.monotonic()

    async def _ensure_script(self, idx: int, timeout: float = 2.0) -> str:
        remote_path = f"/data/local/tmp/tvd_hdmi_{idx}.mks"
        if idx in self._pushed:
            return remote_path
        script = build_monkey_script(idx).encode("ascii")
        fd, local_path = tempfile.mkstemp(prefix=f"tvd_hdmi_{idx}_", suffix=".mks")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(script)
            rc, _, err = await self._exec(self.adb, "-s", self.host, "push", local_path, remote_path, timeout=timeout)
            if rc != 0:
                raise TVError("unreachable", f"adb push failed: {err.decode('utf-8', 'replace')[:120]}")
            self._pushed.add(idx)
        finally:
            try:
                os.unlink(local_path)
            except OSError:
                pass
        return remote_path

    async def switch_input(self, target: str, timeout: float = 8.5) -> dict:
        canonical = normalize_input(target)
        if not canonical or canonical not in INPUT_INDEX:
            raise TVError("bad_request", f"unknown input: {target!r}")
        idx = INPUT_INDEX[canonical]
        async with self._lock:
            await self._ensure_connected(min(2.5, timeout))
            remote_path = await self._ensure_script(idx, min(2.0, timeout))
            rc, out, err = await self._exec(
                self.adb, "-s", self.host, "shell", "monkey", "-f", remote_path, "1",
                timeout=max(2.0, timeout - 1.5),
            )
            if rc != 0:
                # Stale ADB socket or script cleaned from /data/local/tmp after TV reboot: reconnect & re-push once.
                self._last_ok_at = 0.0
                await self._ensure_connected(min(2.5, timeout), force=True)
                self._pushed.discard(idx)
                remote_path = await self._ensure_script(idx, 2.0)
                rc, out, err = await self._exec(
                    self.adb, "-s", self.host, "shell", "monkey", "-f", remote_path, "1",
                    timeout=max(2.0, timeout - 2.0),
                )
            if rc != 0:
                self._last_ok_at = 0.0
                msg = (out + err).decode("utf-8", "replace").strip()[:160]
                raise TVError("unreachable", f"monkey switch failed: {msg}")
            self._last_ok_at = time.monotonic()
            self._current_input = canonical
            self._log("huawei_switch", input=canonical, index=idx)
            return {"input": canonical, "index": idx}

    async def _read_input(self) -> str:
        command = (f"test -r {INPUT_HELPER} || exit 44; "
                   f"CLASSPATH={INPUT_HELPER}:/system/framework/hwtvshare.jar "
                   "app_process /system/bin HuaweiInput")
        argv = (self.adb, "-s", self.host)
        rc, out, _ = await self._exec(*argv, "shell", command, timeout=1.5)
        if rc == 44:
            dex = Path(__file__).with_name("huawei-input.dex")
            if dex.is_file():
                pushed, _, _ = await self._exec(*argv, "push", str(dex), INPUT_HELPER, timeout=1.5)
                if pushed == 0:
                    rc, out, _ = await self._exec(*argv, "shell", command, timeout=1.5)
        parts = out.split()
        if rc or len(parts) != 2 or not parts[0].isdigit() or parts[1] != b"0":
            return "unknown"
        return SOURCE_IDS.get(int(parts[0]), "unknown")

    async def _check_input(self, required: str) -> str:
        observed = await self._read_input()
        if required and observed != required:
            code = "unverified_input" if observed == "unknown" else "input_mismatch"
            raise TVError(code, "TV source could not be verified as the requested HDMI input")
        return observed

    async def screenshot(self, max_width: int = 896, quality: int = 7, timeout: float = 6.5,
                         required_input: str = "") -> dict:
        """Capture the live TV screen as a compact base64 JPEG (~30-60 KB in ~1.4 s)."""
        max_width = max(320, min(1920, int(max_width or 896)))
        quality = max(2, min(15, int(quality or 7)))
        if required_input not in ("", "hdmi2", "hdmi3"):
            raise TVError("bad_request", "unsupported required HDMI input")
        async with self._lock:
            await self._ensure_connected(min(2.0, timeout))
            has_ffmpeg = shutil.which(self.ffmpeg) is not None
            if has_ffmpeg:
                before = await self._check_input(required_input)
                rc, raw, err = await self._exec(
                    self.adb, "-s", self.host, "exec-out", "screencap",
                    timeout=max(2.5, timeout - 1.0),
                )
                if rc != 0:
                    self._last_ok_at = 0.0
                    await self._ensure_connected(min(2.0, timeout), force=True)
                    before = await self._check_input(required_input)
                    rc, raw, err = await self._exec(
                        self.adb, "-s", self.host, "exec-out", "screencap",
                        timeout=max(2.5, timeout - 1.5),
                    )
                if rc == 0 and len(raw) >= 16:
                    after = await self._check_input(required_input)
                    self._last_ok_at = time.monotonic()
                    w, h, fmt = struct.unpack("<III", raw[:12])
                    pixel_bytes = w * h * 4
                    hdr_len = len(raw) - pixel_bytes
                    if fmt == 1 and w > 0 and h > 0 and hdr_len in (12, 16):
                        rc2, jpg, _ = await self._exec(
                            self.ffmpeg, "-loglevel", "error",
                            "-f", "rawvideo", "-pix_fmt", "rgba", "-s", f"{w}x{h}", "-i", "-",
                            "-vf", f"scale={max_width}:-1", "-q:v", str(quality),
                            "-f", "image2pipe", "-vcodec", "mjpeg", "-",
                            timeout=2.5,
                            stdin_data=raw[hdr_len:],
                        )
                        if rc2 == 0 and len(jpg) > 256:
                            return {
                                "image_b64": base64.b64encode(jpg).decode("ascii"),
                                "bytes": len(jpg),
                                "width": w,
                                "height": h,
                                "input": before if before == after else "unknown",
                                "input_verified": before == after and before != "unknown",
                            }
            # Fallback: PNG via screencap -p (bounded to fit within client's 512 KiB JSON limit).
            before = await self._check_input(required_input)
            rc, png, err = await self._exec(
                self.adb, "-s", self.host, "exec-out", "screencap", "-p",
                timeout=max(3.0, timeout),
            )
            if rc != 0 or len(png) < 256:
                raise TVError("unreachable", f"screencap failed: {err.decode('utf-8', 'replace')[:120]}")
            after = await self._check_input(required_input)
            if len(png) > 340_000:
                raise TVError("oversize", "screenshot requires ffmpeg to produce a bounded JPEG")
            return {
                "image_b64": base64.b64encode(png).decode("ascii"),
                "bytes": len(png),
                "input": before if before == after else "unknown",
                "input_verified": before == after and before != "unknown",
            }
