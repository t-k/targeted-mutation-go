#!/usr/bin/env python3
"""Check mandatory PICT rows and pair coverage against the exhaustive model."""

from itertools import combinations
from pathlib import Path
import subprocess


def rows(text):
    return [tuple(line.split("\t")) for line in text.splitlines() if line]


def pairs(table):
    return {pair for row in table for pair in combinations(enumerate(row), 2)}


root = Path(__file__).resolve().parents[1] / "verification/pict"
for model in sorted(root.glob("*.pict")):
    header, *test_rows = rows(model.with_suffix(".tsv").read_text())
    seed_header, *seeds = rows(model.with_suffix(".seed.tsv").read_text())
    full_header, *combinations_found = rows(
        subprocess.check_output(["pict", str(model), "/o:max"], text=True)
    )
    if header != seed_header or header != full_header:
        raise SystemExit(f"{model.name}: inconsistent column order")
    if not set(seeds) <= set(test_rows):
        raise SystemExit(f"{model.name}: required rows missing")
    if not set(test_rows) <= set(combinations_found):
        raise SystemExit(f"{model.name}: impossible rows in test table")
    expected = pairs(combinations_found)
    if pairs(test_rows) != expected:
        raise SystemExit(f"{model.name}: missing valid pairs")
    print(f"{model.name}: {len(test_rows)} rows cover {len(expected)} valid pairs and {len(seeds)} required rows")
