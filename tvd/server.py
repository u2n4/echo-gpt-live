"""tvd: a small signed HTTP front for the TV controller.

    python server.py serve                      run the daemon (systemd: tvd.service)
    python server.py call ACTION [JSON_ARGS]    sign and send one command (manual tests on the host)

Listens on 127.0.0.1 only. Tailscale Funnel maps the public /tv path here for the Alexa Lambda;
Sol reaches it on loopback. Both sign every request (see auth.py). Allowed actions are a fixed list.
Everything before the signature check is treated as hostile: headers are shape-checked first,
the body is read with a size cap and a time limit, and rejections are logged at a bounded rate.
"""
import asyncio
import json
import os
import secrets
import socket
import sys
import time
import urllib.error
import urllib.request

from auth import MAX_BODY, Verifier, headers_ok, sign
from controller import Controller, TVError
from huawei import HuaweiTV

HOME = os.path.expanduser("~")
STATE_DIR = os.environ.get("TVD_STATE_DIR", f"{HOME}/.local/state/tvd")
CONF = {
    "host": os.environ.get("TVD_TV_HOST", ""),
    "mac": os.environ.get("TVD_TV_MAC", ""),
    "broadcast": os.environ.get("TVD_BROADCAST", "255.255.255.255"),
    "client_name": os.environ.get("TVD_CLIENT_NAME", "tvd"),
    "huawei_host": os.environ.get("TVD_HUAWEI_HOST", ""),
    "listen_host": "127.0.0.1",
    "listen_port": int(os.environ.get("TVD_PORT", "8831")),
    "key_file": os.environ.get("TVD_KEY_FILE", f"{HOME}/.config/tvd/signing.key"),
    "cert_dir": os.environ.get("TVD_CERT_DIR", f"{HOME}/.config/tvd"),
    "log_file": f"{STATE_DIR}/events.jsonl",
    "nonce_file": f"{STATE_DIR}/nonces",
    "power_file": f"{STATE_DIR}/last_power.json",
}
# At most this many requests in flight. The rest are refused before their body is read: the
# public /tv path must not be able to load the host that also carries the live bridge (Grok).
MAX_IN_FLIGHT = 4
FORBIDDEN_PORTS = {8787, 8811, 8821}  # the live bridge and its diagnostic harness ports
ACTIONS = {
    "power_on", "power_off", "volume_step", "set_mute", "toggle_mute",
    "play", "pause", "play_pause", "stop", "launch", "launch_url",
    "key", "sequence", "text", "state",
    "switch_input", "screenshot",
}
COMMAND_TIMEOUT_S = 5.5  # upper bound for one command; a caller's deadline can only shorten it
# Opening an app may first have to wake the box and switch it on (~2.5-4 s measured), then wait
# for the app to come to the front (~0.3-1 s).
LAUNCH_TIMEOUT_S = 6.8
HUAWEI_TIMEOUT_S = 9.0
REPLY_MARGIN_S = 0.4     # time kept back for our reply to travel back before the caller gives up
MIN_BUDGET_S = 0.8       # less than this left: refuse instead of starting something we cannot finish
BODY_READ_S = 3.0
MSG_ID_TTL_S = 300
LOG_MAX_BYTES = 5 * 1024 * 1024


def load_key(path: str) -> bytes:
    with open(path, "rb") as f:
        raw = f.read().strip()
    if len(raw) < 32:
        raise SystemExit(f"{path}: key shorter than 32 bytes")
    return raw


class EventLog:
    """JSON lines, rotated to one .1 file at 5 MB. Rejections are rate-limited: at most one
    line per 10 s, carrying how many were folded into it."""

    def __init__(self, path: str, clock=time.monotonic):
        os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
        self._path = path
        self._clock = clock
        self._f = open(path, "a", buffering=1)
        self._reject_next = 0.0
        self._reject_folded = 0

    def __call__(self, ev: str, **fields) -> None:
        if self._f.tell() > LOG_MAX_BYTES:
            self._f.close()
            os.replace(self._path, self._path + ".1")
            self._f = open(self._path, "a", buffering=1)
        rec = {"t": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "ev": ev, **fields}
        self._f.write(json.dumps(rec, ensure_ascii=False) + "\n")

    def close(self) -> None:
        self._f.close()

    def rejected(self, **fields) -> None:
        now = self._clock()
        if now < self._reject_next:
            self._reject_folded += 1
            return
        self("rejected", folded=self._reject_folded, **fields)
        self._reject_folded = 0
        self._reject_next = now + 10.0


class MessageIds:
    """Alexa may deliver a directive twice. A repeated message id gets the first result back
    instead of running again (a volume step must not happen twice)."""

    def __init__(self, clock=time.monotonic):
        self._clock = clock
        self._seen: dict[str, tuple[float, asyncio.Future]] = {}

    def get_or_start(self, msg_id: str):
        now = self._clock()
        self._seen = {k: v for k, v in self._seen.items() if v[0] > now}
        if msg_id in self._seen:
            return self._seen[msg_id][1], False
        fut = asyncio.get_running_loop().create_future()
        self._seen[msg_id] = (now + MSG_ID_TTL_S, fut)
        return fut, True


class BodyError(Exception):
    pass


async def read_body(request) -> bytes:
    """The whole body, or BodyError('too_large' | 'slow')."""
    if request.content_length is not None and request.content_length > MAX_BODY:
        raise BodyError("too_large")
    chunks, total = [], 0

    async def pull():
        nonlocal total
        while True:
            chunk = await request.content.read(8192)
            if not chunk:
                return
            total += len(chunk)
            if total > MAX_BODY:
                raise BodyError("too_large")
            chunks.append(chunk)

    try:
        await asyncio.wait_for(pull(), BODY_READ_S)
    except asyncio.TimeoutError:
        raise BodyError("slow") from None
    return b"".join(chunks)


def budget_for(req: dict, wall=time.time) -> float | None:
    """Seconds this command may use, from the caller's absolute deadline (epoch seconds)."""
    act = req.get("action")
    if act in ("switch_input", "screenshot", "text"):
        cap = HUAWEI_TIMEOUT_S
    elif act in ("launch", "launch_url"):
        cap = LAUNCH_TIMEOUT_S
    else:
        cap = COMMAND_TIMEOUT_S
    deadline = req.get("deadline")
    if deadline is None:
        return cap
    if not isinstance(deadline, (int, float)) or isinstance(deadline, bool):
        return None
    left = float(deadline) - wall() - REPLY_MARGIN_S
    if left > 15:  # nobody legitimate asks for more than Alexa's 8 s
        return None
    return min(cap, left)


LOCAL_ONLY_ACTIONS = {"screenshot", "switch_input", "text"}


def _safe_log_args(args: dict) -> dict:
    """Redact user-typed text and URLs before writing command logs."""
    out = {}
    for k, v in args.items():
        if k == "text" and isinstance(v, str):
            out["text_len"] = len(v)
        elif k == "url" and isinstance(v, str):
            out["url_len"] = len(v)
        else:
            out[k] = v
    return out


def make_handler(verifier: Verifier, ctl: Controller, log, msg_ids: MessageIds, wall=time.time,
                 max_in_flight: int = MAX_IN_FLIGHT, huawei: HuaweiTV | None = None):
    from aiohttp import web

    slots = asyncio.Semaphore(max_in_flight)

    def reject(status: int, err: str):
        log.rejected(reason=err, status=status)
        return web.json_response({"ok": False, "error": err}, status=status)

    async def run(action: str, args: dict, budget: float) -> dict:
        if action == "power_on":
            return await ctl.power_on(budget)
        if action == "power_off":
            return await ctl.power_off(budget)
        if action == "volume_step":
            return await ctl.volume_step(args.get("steps"), budget)
        if action == "set_mute":
            return await ctl.set_mute(args.get("mute"), budget)
        if action == "toggle_mute":
            return await ctl.toggle_mute(budget)
        if action in ("play", "pause", "play_pause", "stop"):
            if "count" in args or "delay_ms" in args:
                return await ctl.playback(action, budget, args.get("count", 1), args.get("delay_ms", 80))
            return await ctl.playback(action, budget)
        if action == "launch":
            if huawei is not None and huawei._current_input == "hdmi2":
                # Auto-switch Huawei TV back from cameras (HDMI 2) to Xiaomi (HDMI 3) in parallel!
                async def _back_to_xiaomi():
                    try:
                        await huawei.switch_input("xiaomi", timeout=min(budget, 6.0))
                    except Exception:
                        pass
                snap, _ = await asyncio.gather(ctl.launch(args.get("app"), budget), _back_to_xiaomi())
                return snap
            return await ctl.launch(args.get("app"), budget)
        if action == "launch_url":
            if huawei is not None and huawei._current_input == "hdmi2":
                async def _back_to_xiaomi():
                    try:
                        await huawei.switch_input("xiaomi", timeout=min(budget, 6.0))
                    except Exception:
                        pass
                snap, _ = await asyncio.gather(ctl.launch_url(args.get("url"), budget), _back_to_xiaomi())
                return snap
            return await ctl.launch_url(args.get("url"), budget)
        if action == "key":
            if "count" in args or "delay_ms" in args:
                return await ctl.key(args.get("key"), budget, args.get("count", 1), args.get("delay_ms", 80))
            return await ctl.key(args.get("key"), budget)
        if action == "sequence":
            return await ctl.sequence(args.get("steps"), budget)
        if action == "text":
            if "pace_ms" in args:
                return await ctl.send_text(args.get("text"), budget, args.get("pace_ms", 0))
            return await ctl.send_text(args.get("text"), budget)
        if action == "switch_input":
            if huawei is None:
                raise TVError("unconfigured", "Huawei TV host is not configured")
            return await huawei.switch_input(args.get("input", ""), budget)
        if action == "screenshot":
            if huawei is None:
                raise TVError("unconfigured", "Huawei TV host is not configured")
            return await huawei.screenshot(args.get("max_width", 1024), args.get("quality", 6), budget,
                                           required_input=args.get("required_input", ""))
        return await ctl.state()

    async def handle(request: web.Request) -> web.Response:
        ts, sig = request.headers.get("X-TV-Timestamp"), request.headers.get("X-TV-Signature")
        if not headers_ok(ts, sig):
            return reject(401, "unauthenticated")
        if slots.locked():
            return reject(503, "busy")
        async with slots:
            return await handle_admitted(request, ts, sig)

    async def handle_admitted(request: web.Request, ts: str, sig: str) -> web.Response:
        t0 = time.monotonic()
        try:
            body = await read_body(request)
        except BodyError as e:
            return reject(413 if str(e) == "too_large" else 408, str(e))
        err = verifier.verify(ts, sig, body)
        if err:
            return reject({"too_large": 413, "forbidden": 403}.get(err, 401), err)
        try:
            req = json.loads(body)
            assert isinstance(req, dict) and req.get("v") == 1
        except Exception:  # noqa: BLE001
            return web.json_response({"ok": False, "error": "bad_request"}, status=400)
        err = verifier.claim_nonce(req.get("nonce"))
        if err:
            return reject({"replay": 409, "busy": 503}.get(err, 400), err)
        action, args, msg_id = req.get("action"), req.get("args") or {}, req.get("msg_id")
        budget = budget_for(req, wall)
        if (action not in ACTIONS or not isinstance(args, dict) or budget is None
                or (msg_id is not None and not isinstance(msg_id, str))):
            return web.json_response({"ok": False, "error": "bad_request"}, status=400)
        # Never expose screenshots, HDMI switching, or raw typing over a public Funnel proxy.
        if action in LOCAL_ONLY_ACTIONS and (
            request.headers.get("X-Forwarded-For")
            or request.headers.get("X-Forwarded-Host")
            or request.headers.get("Tailscale-User-Login")
        ):
            return reject(403, "local_only")

        async def execute() -> dict:
            if budget < MIN_BUDGET_S and action != "state":
                return {"ok": False, "action": action, "error": "timeout"}
            try:
                out = await asyncio.wait_for(run(action, args, budget), budget + 0.2)
                if action == "screenshot":
                    return {"ok": True, "action": action, "image_b64": out.get("image_b64", ""),
                            "input": out.get("input", ""), "input_verified": out.get("input_verified", False),
                            "state": await ctl.state()}
                if action == "switch_input":
                    return {"ok": True, "action": action, "input": out.get("input", ""),
                            "state": await ctl.state()}
                return {"ok": True, "action": action, "state": out}
            except TVError as e:
                return {"ok": False, "action": action, "error": e.code}
            except asyncio.TimeoutError:
                return {"ok": False, "action": action, "error": "timeout"}
            except asyncio.CancelledError:
                # The caller hung up mid-command (handler_cancellation): what was done so far is
                # unknown from here, so say that it happened before letting the cancel through.
                log("command_cancelled", action=action, args=_safe_log_args(args), ms=int((time.monotonic() - t0) * 1000))
                raise
            except Exception as e:  # noqa: BLE001 - a library error must not kill the daemon
                log("error", action=action, error=type(e).__name__, detail=str(e)[:200])
                return {"ok": False, "action": action, "error": "internal"}

        if msg_id and action != "state":
            fut, first = msg_ids.get_or_start(msg_id[:200])
            if first:
                result = {"ok": False, "action": action, "error": "cancelled"}
                try:
                    result = await execute()
                finally:
                    if not fut.done():  # also when the leader is cancelled: release duplicates
                        fut.set_result(result)
                result = dict(result, duplicate=False)
            else:
                # shield: a duplicate caller going away must not cancel the shared result
                result = dict(await asyncio.shield(fut), duplicate=True)
        else:
            result = await execute()
        ms = int((time.monotonic() - t0) * 1000)
        log("command", action=action, args=_safe_log_args(args), ok=result["ok"], error=result.get("error"),
            power=(result.get("state") or {}).get("power"), ms=ms, dup=result.get("duplicate", False))
        return web.json_response(dict(result, ms=ms))

    return handle


def wol_sender(mac: str, broadcast: str, host: str):
    cleaned = mac.replace(":", "").replace("-", "").strip()
    raw_mac = bytes.fromhex(cleaned) if len(cleaned) == 12 else b""
    pkt = (b"\xff" * 6 + raw_mac * 16) if len(raw_mac) == 6 else b""

    def send() -> None:
        if not pkt or not host:
            raise TVError("unconfigured", "TVD_TV_HOST and TVD_TV_MAC must be configured")
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
            for dst in ((broadcast, 9), ("255.255.255.255", 9), (host, 9)):
                s.sendto(pkt, dst)
        finally:
            s.close()

    return send


def port_prober(host: str):
    async def probe(port: int, timeout: float) -> bool:
        try:
            _, w = await asyncio.wait_for(asyncio.open_connection(host, port), timeout)
        except (OSError, asyncio.TimeoutError):
            return False
        w.close()
        return True

    return probe


async def serve() -> None:
    from aiohttp import web
    from androidtvremote2 import AndroidTVRemote

    if CONF["listen_port"] in FORBIDDEN_PORTS:
        raise SystemExit(f"refusing to listen on {CONF['listen_port']}")
    key = load_key(CONF["key_file"])
    cert, keyf = f"{CONF['cert_dir']}/cert.pem", f"{CONF['cert_dir']}/key.pem"
    for p in (cert, keyf):
        if not os.path.exists(p):
            raise SystemExit(f"missing {p}: pair first")
    log = EventLog(CONF["log_file"])
    ctl = Controller(
        make_remote=lambda: AndroidTVRemote(CONF["client_name"], cert, keyf, CONF["host"]),
        probe=port_prober(CONF["host"]),
        wol=wol_sender(CONF["mac"], CONF["broadcast"], CONF["host"]),
        log=log,
        state_file=CONF["power_file"],
    )
    huawei = HuaweiTV(CONF["huawei_host"], log=log) if CONF["huawei_host"] else None
    handler = make_handler(Verifier(key, store_path=CONF["nonce_file"]), ctl, log, MessageIds(), huawei=huawei)
    app = web.Application(client_max_size=MAX_BODY)
    # Funnel may forward /tv/cmd as-is or with the mount prefix stripped; the signed scope is the
    # same constant either way, so both paths reach one handler.
    app.router.add_post("/tv/cmd", handler)
    app.router.add_post("/cmd", handler)
    # A caller that gave up (the Lambda at its deadline, the bridge when a call ends) cancels its
    # command here too. Safe at any await: a POWER press is marked unanswered before its wait
    # (_press_power), the lock is released by its context manager, and a duplicate waiting on a
    # cancelled leader gets "cancelled" (make_handler).
    runner = web.AppRunner(app, access_log=None, handler_cancellation=True)
    await runner.setup()
    site = web.TCPSite(runner, CONF["listen_host"], CONF["listen_port"], reuse_port=False)
    await site.start()  # fails loudly if the port is taken
    ctl.start()
    log("start", listen=f"{CONF['listen_host']}:{CONF['listen_port']}", tv=CONF["host"],
        huawei=bool(huawei))
    await asyncio.Event().wait()


def call(action: str, args_json: str = "{}") -> None:
    key = load_key(CONF["key_file"])
    body = json.dumps({"v": 1, "nonce": secrets.token_urlsafe(18), "action": action,
                       "args": json.loads(args_json), "deadline": time.time() + 10.0}).encode()
    ts = str(int(time.time()))
    req = urllib.request.Request(
        f"http://127.0.0.1:{CONF['listen_port']}/tv/cmd", data=body, method="POST",
        headers={"Content-Type": "application/json", "X-TV-Timestamp": ts, "X-TV-Signature": sign(key, ts, body)},
    )
    try:
        with urllib.request.urlopen(req, timeout=12) as r:
            raw = r.read().decode()
            if action == "screenshot":
                d = json.loads(raw)
                b64 = d.pop("image_b64", "")
                d["image_b64_len"] = len(b64)
                print(r.status, json.dumps(d))
            else:
                print(r.status, raw)
    except urllib.error.HTTPError as e:
        print(e.code, e.read().decode())


if __name__ == "__main__":
    if sys.argv[1:2] == ["serve"]:
        asyncio.run(serve())
    elif sys.argv[1:2] == ["call"] and len(sys.argv) in (3, 4):
        call(*sys.argv[2:])
    else:
        raise SystemExit(__doc__)
