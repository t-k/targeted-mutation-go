#!/usr/bin/env python3
"""Check killed variants from the pinned peers against the Go source profile."""

import argparse
import json
import os
from pathlib import Path
import shutil
import tempfile

from compare import run


def matching(rows, condition, tool, description):
    if not any(condition(row) for row in rows):
        raise RuntimeError(f"{tool} did not kill {description}")
    print(f"{tool}: killed {description}", flush=True)


def verify(tool, report):
    if tool == "tmt":
        rows = [row for row in report["findings"] if row["status"] == "killed"]
        for rule, before, after in (
            ("source-operator", "++", "--"),
            ("return-nil", "err", "nil"),
            ("errorf-unwrap", "%w", "%v"),
            ("return-zero", '"<empty>"', '""'),
            ("string-literal", '"load: %w"', '""'),
        ):
            matching(rows, lambda r: (r["rule"], r.get("expression"), r.get("mutation")) == (rule, before, after),
                     tool, f"{before} to {after}")
        if report["incomplete"]:
            raise RuntimeError("TMT reported an incomplete source replay")
    elif tool == "go-mutants":
        rows = [row for row in report["mutants"] if row["outcome"] == "killed"]
        for rule in ("incr-to-decr", "return-err-to-nil"):
            matching(rows, lambda r: r["rule"] == rule, tool, rule)
    elif tool == "gomu":
        rows = [row["mutant"] for row in report["results"] if row["status"] == "KILLED"]
        matching(rows, lambda r: r["original"] == '"<empty>"' and r["mutated"] == '""', tool, "literal removal")
    else:
        rows = [
            row for file in report["files"] for row in file["mutations"] if row["status"] == "KILLED"
        ]
        rules = ["INCREMENT_DECREMENT"]
        if tool == "gomutants":
            rules += ["RETURN_ERROR_NIL", "ERRORF_WRAP"]
        for rule in rules:
            matching(rows, lambda r: r["type"] == rule, tool, rule)


def verdicts(tool, report):
    """Normalize each executable mutation on the fixture, including survivors."""
    if tool == "tmt":
        rows = ((r["line"], r["rule"], r.get("expression", ""), r.get("mutation", ""), r["status"])
                for r in report["findings"])
    elif tool == "go-mutants":
        rows = ((r["line"], r["rule"], r["original"], r["replacement"], r["outcome"]) for r in report["mutants"])
    elif tool == "gomu":
        rows = ((r["mutant"]["line"], r["mutant"]["type"], r["mutant"]["original"],
                 r["mutant"]["mutated"], r["status"]) for r in report["results"])
    elif tool == "gremlins":
        edits = {"CONDITIONALS_NEGATION": ("==", "!="), "INCREMENT_DECREMENT": ("++", "--")}
        rows = ((r["line"], r["type"], *edits[r["type"]], r["status"])
                for f in report["files"] for r in f["mutations"])
    else:
        rows = ((r["line"], r["type"], r["original"], r["replacement"], r["status"])
                for f in report["files"] for r in f["mutations"])

    known = {"killed": "killed", "lived": "survived", "survived": "survived", "source-survivor": "survived"}
    normalized = {}
    for line, rule, before, after, status in rows:
        status = status.lower().replace("_", "-")
        if status in {"non-viable", "not-viable", "rejected"}:
            continue
        if status not in known:
            raise RuntimeError(f"{tool} has an unexpected verdict {status!r} on line {line}")
        if rule in {"empty-block", "empty_block"}:
            before, after = "{...}", "{}"
        if after == "<removed>":
            after = ""
        key = line, before, after
        old = normalized.setdefault(key, known[status])
        if old != known[status]:
            raise RuntimeError(f"{tool} has inconsistent results for {key}")
    return normalized


def compare_verdicts(reports):
    tmt = verdicts("tmt", reports["tmt"])
    for tool, report in reports.items():
        if tool == "tmt":
            continue
        wanted = verdicts(tool, report)
        differences = {edit: (expected, tmt.get(edit)) for edit, expected in wanted.items() if tmt.get(edit) != expected}
        if differences:
            raise RuntimeError(f"{tool} differs from TMT: {differences}")
        print(f"{tool}: all {len(wanted)} distinct executable fixture mutations matched TMT", flush=True)


def main(binaries, outputs):
    commands = {
        "tmt": ("targeted-mutation-go", ["analyze", "--source-only", "--jobs=4", "--show-killed", "--format=json", "."], None, {0, 2}),
        "gremlins": ("gremlins", ["unleash", "--timeout-coefficient=3000", "--workers=4", "--output=tool.json", "."], "tool.json", {0}),
        "gomutants": ("gomutants", ["--workers=4", "--cache=off", "--only=INCREMENT_DECREMENT,RETURN_ERROR_NIL,ERRORF_WRAP",
                                   "--timeout-min=10s", "--output=tool.json", "."], "tool.json", {0}),
        "go-mutants": ("go-mutants", ["run", "--jobs=4", "--operator=error-swallowing", "--operator=arithmetic-assignment",
                                    "--timeout=30s", "--cache=off", "--report=none", "--no-tui", "--json"], None, {0}),
        "gomu": ("gomu", ["run", "--workers=4", "--incremental=false", "--fail-on-gate=false", "--output=json", "."], "mutation-report.json", {0}),
    }
    fixture = Path(__file__).resolve().parent / "testdata" / "peer"
    outputs.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, GOFLAGS="", GOTOOLCHAIN="go1.26.10", GOMAXPROCS="4")
    reports = {}
    with tempfile.TemporaryDirectory(prefix="tmt-peer-parity-") as root:
        for tool, (binary, args, report_file, allowed) in commands.items():
            path = Path(root) / tool / "peer"
            shutil.copytree(fixture, path)
            elapsed, code, stdout, stderr = run([str(binaries / binary), *args], path, env)
            (outputs / f"{tool}.stdout").write_text(stdout)
            (outputs / f"{tool}.stderr").write_text(stderr)
            if code not in allowed:
                raise RuntimeError(f"{tool} exited {code}. Logs are in {outputs}")
            report = json.loads((path / report_file).read_text() if report_file else stdout)
            (outputs / f"{tool}.json").write_text(json.dumps(report, indent=2) + "\n")
            reports[tool] = report
            verify(tool, report)
            print(f"{tool}: completed in {elapsed:.2f}s", flush=True)
    compare_verdicts(reports)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binaries", type=Path, required=True)
    parser.add_argument("--outputs", type=Path, required=True)
    args = parser.parse_args()
    main(args.binaries.resolve(), args.outputs.resolve())
