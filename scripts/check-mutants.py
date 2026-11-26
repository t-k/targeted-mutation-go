#!/usr/bin/env python3
"""The planner tests must reject each selected mutation in a separate Go overlay."""

import json
from pathlib import Path
import subprocess
import tempfile


root = Path(__file__).resolve().parents[1]
source = root / "internal/analysis/analysis.go"
original = source.read_text()
mutants = {
    "operator mask": ("c.Op.Mask()^original.Mask()", "c.Op.Mask()&original.Mask()"),
    "signed rank": ("uint64(value) ^ (1 << 63)", "uint64(value)"),
    "inclusive threshold": ("low = seen.Below + 1", "low = seen.Below"),
    "negation catalog": (
        'candidates = append(candidates, Candidate{Kind: "negation", Op: op.Negate(), Pivot: pivot})',
        "",
    ),
}

for name, (before, after) in mutants.items():
    if original.count(before) != 1:
        raise SystemExit(f"{name}: expected exactly one mutation site")
    with tempfile.TemporaryDirectory(prefix="tmt-go-self-") as temporary:
        overlay = Path(temporary) / "overlay.json"
        changed = Path(temporary) / "analysis.go"
        changed.write_text(original.replace(before, after, 1))
        overlay.write_text(json.dumps({"Replace": {str(source): str(changed)}}))
        result = subprocess.run(
            ["go", "test", "-count=1", f"-overlay={overlay}", "./internal/analysis"],
            cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=90,
        )
        if result.returncode != 1 or "--- FAIL: Test" not in result.stdout or "[build failed]" in result.stdout:
            raise SystemExit(f"{name}: mutant was not killed by a test\n{result.stdout}")
        print(f"{name}: killed")
