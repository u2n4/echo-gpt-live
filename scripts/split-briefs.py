#!/usr/bin/env python3
"""Split docs/REVIEW-PLAN.md into one brief file per reviewer.

Each brief is pasted as the ENTIRE prompt to a model with no other context, so each file gets a
short preamble naming the repository and the fact that the brief is self-contained.
"""
import os
import re
import sys

PREAMBLE = """You are one of five independent reviewers working on the same problem. This brief is
your entire task. Answer it on its own; you are not talking to the other reviewers.

Repository under review: {root}
Read `docs/EVIDENCE-audio-quality.md` in it first. Everything measured is there.

"""


def main(plan, outdir, root):
    with open(plan, encoding="utf-8") as f:
        src = f.read()
    os.makedirs(outdir, exist_ok=True)

    # split on the top-level BRIEF headings
    parts, current, fence = [], [], None
    for line in src.splitlines(keepends=True):
        marker = re.match(r"^ {0,3}(`{3,}|~{3,})", line)
        if marker:
            token = marker.group(1)
            if fence is None:
                fence = token
            elif token[0] == fence[0] and len(token) >= len(fence):
                fence = None
        if fence is None and line.startswith("## BRIEF "):
            parts.append("".join(current))
            current = []
        current.append(line)
    parts.append("".join(current))
    written = []
    for part in parts:
        m = re.match(r"## BRIEF ([A-Z]\d+)\b", part)
        if not m:
            continue
        tag = m.group(1)
        # stop the section before the planner's own prior, which reviewers must not see
        part = re.split(r"\n## What I expect the answer to be", part)[0]
        kind = "grok" if tag.startswith("G") else "codex"
        path = os.path.join(outdir, f"{kind}-{tag.lower()}.md")
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(PREAMBLE.format(root=root))
            f.write(part.rstrip() + "\n")
        written.append((path, len(part.split("\n"))))

    for p, n in written:
        print(f"{n:4d} lines  {p}")
    if not written:
        sys.exit("no BRIEF sections found")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], sys.argv[3])
