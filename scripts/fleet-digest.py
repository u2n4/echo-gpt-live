#!/usr/bin/env python3
"""Digest a review-fleet run into the claims that survived cross-examination.

The bundled consolidator clusters round-1 findings by a heading shape this project's lens packs do
not produce, so it reported zero topics while the round-2 verdicts parsed fine. The verdicts are the
part that matters anyway: round 2 is where every claim was checked against FACTS by a model that did
not write it.

Output is grouped by verdict and deduplicated, with the number of DISTINCT MODELS that reached the
same verdict on a similar claim. Agreement is a prioritisation signal, not evidence.
"""
import collections
import glob
import os
import re
import sys

VERDICTS = ("SUPPORTED", "CONTRADICTED", "NEEDS_MEASUREMENT", "UNSUPPORTED")


def normalise(claim):
    """Fold a claim to a key so near-duplicates from different models land together."""
    s = claim.lower()
    s = re.sub(r"[^a-z0-9 ]+", " ", s)
    s = re.sub(r"\b(the|a|an|is|are|would|could|that|this|it|to|of|and|in|on|for|with|be|by|as)\b", " ", s)
    words = sorted(set(w for w in s.split() if len(w) > 3))
    return " ".join(words[:9])


def main(root):
    rows = []
    for path in sorted(glob.glob(os.path.join(root, "round2", "*.md"))):
        name = os.path.basename(path)
        m = re.match(r"r2-\d+-(.+?)-(.+)\.md$", name)
        lens = m.group(1) if m else "?"
        model = m.group(2) if m else "?"
        for line in open(path, encoding="utf-8", errors="replace"):
            parts = [p.strip() for p in line.split("|")]
            if len(parts) >= 2 and parts[0] in VERDICTS:
                rows.append({
                    "verdict": parts[0],
                    "claim": parts[1],
                    "evidence": parts[2] if len(parts) > 2 else "",
                    "lens": lens,
                    "model": model,
                })

    if not rows:
        print("no verdict lines found under", root)
        return

    by_verdict = collections.Counter(r["verdict"] for r in rows)
    models = {r["model"] for r in rows}
    print(f"{root}")
    print(f"  {len(rows)} verdicts from {len(models)} distinct models across "
          f"{len({r['lens'] for r in rows})} lenses")
    print("  " + "  ".join(f"{v}={by_verdict[v]}" for v in VERDICTS))
    print()

    for verdict in ("SUPPORTED", "CONTRADICTED", "NEEDS_MEASUREMENT"):
        group = [r for r in rows if r["verdict"] == verdict]
        if not group:
            continue
        clusters = collections.defaultdict(list)
        for r in group:
            clusters[normalise(r["claim"])].append(r)
        ranked = sorted(clusters.values(), key=lambda c: -len({r["model"] for r in c}))
        print(f"##### {verdict} — {len(group)} claims in {len(clusters)} clusters")
        for cluster in ranked[:40]:
            n = len({r["model"] for r in cluster})
            lenses = sorted({r["lens"] for r in cluster})
            longest = max(cluster, key=lambda r: len(r["claim"]))
            print(f"  [{n} model{'s' if n != 1 else ''}] ({','.join(lenses[:3])}) {longest['claim'][:300]}")
        print()


if __name__ == "__main__":
    for d in sys.argv[1:]:
        main(d)
