"""Request signing for tvd, shared by the daemon, its tests and the manual client.

A request is POST /tv/cmd with headers X-TV-Timestamp (unix seconds) and X-TV-Signature
(hex HMAC-SHA256). The signed message starts with a fixed scope line, so a signature made for
the bridge's /rtc routes (same algorithm, different key and no scope) can never pass here.
The body carries a nonce; a nonce is accepted once, also across restarts (the store is a file).
"""
import hashlib
import hmac
import os
import re
import time

SCOPE = b"tv-v1\nPOST\n/tv/cmd\n"
WINDOW_S = 30          # how old a request may be
FUTURE_SKEW_S = 5      # how far ahead of our clock a request may be dated
MAX_BODY = 64 * 1024
MAX_NONCES = 10000
# Always used with fullmatch: "$" would also accept a trailing newline, which the nonce file
# then loses on reload (Codex re-review 2026-10-18).
NONCE_RE = re.compile(r"[A-Za-z0-9_-]{16,64}")
SIG_RE = re.compile(r"[0-9a-f]{64}")
TS_RE = re.compile(r"[0-9]{1,12}")  # ASCII digits only: str.isdigit() also accepts "²"


def sign(key: bytes, ts: str, body: bytes) -> str:
    return hmac.new(key, SCOPE + ts.encode() + b"." + body, hashlib.sha256).hexdigest()


def headers_ok(ts, sig) -> bool:
    """Shape check before any body is read: cheap, and refuses most junk without work."""
    return (isinstance(ts, str) and isinstance(sig, str)
            and TS_RE.fullmatch(ts) is not None and SIG_RE.fullmatch(sig.strip().lower()) is not None)


class Verifier:
    """Checks timestamp, signature and nonce. Every method returns an error code or None."""

    def __init__(self, key: bytes, clock=time.time, window: int = WINDOW_S, store_path: str | None = None):
        if len(key) < 32:
            raise ValueError("signing key must be at least 32 bytes")
        self._key = key
        self._clock = clock
        self._window = window
        # Requests signed before this process started are refused as a second guard; the nonce
        # file is what really keeps a used request from coming back after a restart.
        self._started = int(clock())
        self._store_path = store_path
        self._nonces: dict[str, float] = {}
        if store_path:
            self._load()

    # -- persistence: one "expiry nonce" line per accepted request, compacted on load/prune --

    def _load(self) -> None:
        assert self._store_path
        now = self._clock()
        try:
            with open(self._store_path) as f:
                for line in f:
                    parts = line.split()
                    if len(parts) == 2 and NONCE_RE.fullmatch(parts[1]):
                        try:
                            exp = float(parts[0])
                        except ValueError:
                            continue
                        if exp > now:
                            self._nonces[parts[1]] = exp
        except FileNotFoundError:
            pass
        self._rewrite()

    def _rewrite(self) -> None:
        if not self._store_path:
            return
        tmp = self._store_path + ".tmp"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            for n, exp in self._nonces.items():
                f.write(f"{exp:.0f} {n}\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self._store_path)

    def _append(self, nonce: str, exp: float) -> None:
        if not self._store_path:
            return
        fd = os.open(self._store_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        with os.fdopen(fd, "a") as f:
            f.write(f"{exp:.0f} {nonce}\n")
            f.flush()
            os.fsync(f.fileno())

    # -- checks --

    def verify(self, ts: str | None, sig: str | None, body: bytes) -> str | None:
        if len(body) > MAX_BODY:
            return "too_large"
        if not headers_ok(ts, sig) or ts is None or sig is None:
            return "unauthenticated"
        now = self._clock()
        t = int(ts)
        if t < now - self._window or t > now + FUTURE_SKEW_S or t < self._started:
            return "stale"
        if not hmac.compare_digest(sign(self._key, ts, body), sig.strip().lower()):
            return "forbidden"
        return None

    def claim_nonce(self, nonce) -> str | None:
        """Call only after verify() passed, so unsigned traffic cannot fill the store."""
        if not isinstance(nonce, str) or not NONCE_RE.fullmatch(nonce):
            return "bad_request"
        now = self._clock()
        if len(self._nonces) >= MAX_NONCES:
            self._nonces = {n: exp for n, exp in self._nonces.items() if exp > now}
            self._rewrite()
        if nonce in self._nonces and self._nonces[nonce] > now:
            return "replay"
        if len(self._nonces) >= MAX_NONCES:
            return "busy"
        # Kept until no timestamp that could carry it is acceptable any more.
        exp = now + self._window + FUTURE_SKEW_S + 1
        self._append(nonce, exp)  # on disk before the command runs
        self._nonces[nonce] = exp
        return None
