"""Add --tv-url to BRIDGE_FLAGS in bridge.env, once (used by tvd/deploy-remote.sh)."""
import io
import sys

FLAG = "--tv-url http://127.0.0.1:8831"
path = sys.argv[1]
lines = io.open(path, encoding="utf-8").read().split("\n")
changed = False
for i, line in enumerate(lines):
    if line.startswith("BRIDGE_FLAGS=") and "--tv-url" not in line:
        value = line[len("BRIDGE_FLAGS="):]
        if len(value) >= 2 and value[0] in "\"'" and value[-1] == value[0]:
            lines[i] = "BRIDGE_FLAGS=" + value[:-1] + " " + FLAG + value[-1]
        else:
            lines[i] = line + " " + FLAG
        changed = True
if changed:
    io.open(path, "w", encoding="utf-8").write("\n".join(lines))
print("added --tv-url to BRIDGE_FLAGS" if changed else "BRIDGE_FLAGS already has --tv-url (or no BRIDGE_FLAGS line)")
