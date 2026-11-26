#!/usr/bin/env python3
"""Measure full campaigns and check candidate identity and verdict parity."""

import argparse
import json
import os
from pathlib import Path
import shutil
import statistics
import tempfile

from compare import make_fixture, run
from peer_parity import compare_verdicts


def findings(report):
    result = {}
    for row in report["findings"]:
        key = (row["file"], row["line"], row["column"], row["rule"], row.get("expression", ""), row.get("mutation", ""))
        if key in result:
            raise RuntimeError(f"duplicate finding {key}")
        result[key] = row["status"]
    return result


def measure(args):
    env = dict(os.environ, GOFLAGS="", GOTOOLCHAIN=args.toolchain, GOMAXPROCS=str(args.workers))
    args.output.mkdir(parents=True, exist_ok=True)
    options = [f"--jobs={args.workers}", "--show-killed", "--format=json", "."]
    peer = Path(__file__).resolve().parent / "testdata" / "peer"
    summary = {}
    with tempfile.TemporaryDirectory(prefix="tmt-full-comparison-") as scratch:
        for fixture in ("peer", "integer"):
            commands = {name: [str(binary.resolve()), "analyze", "--full", *options]
                        for name, binary in (("before", args.before), ("after", args.after))}
            if args.gomu and fixture == "peer":
                commands["gomu"] = [str(args.gomu.resolve()), "run", f"--workers={args.workers}",
                                    "--incremental=false", "--fail-on-gate=false", "--output=json", "."]
            shared = Path(scratch) / fixture
            if fixture == "peer":
                shutil.copytree(peer, shared)
            else:
                make_fixture(shared, args.sites)
            paths = {name: shared for name in commands}
            if "gomu" in commands:
                paths["gomu"] = Path(scratch) / "peer-gomu"
                shutil.copytree(peer, paths["gomu"])
            samples = {name: [] for name in commands}

            oracle_cmd = [str(args.after.resolve()), "analyze", "--source-only", *options]
            _, code, stdout, stderr = run(oracle_cmd, paths["after"], env)
            if code not in {0, 2}:
                raise RuntimeError(f"source-only failed for {fixture}: {stderr}\n{stdout[:2000]}")
            oracle = findings(json.loads(stdout))

            for i in range(args.runs + 1):
                reports = {}
                names = list(commands)
                names = names[i % len(names):] + names[:i % len(names)]
                for name in names:
                    path = paths[name]
                    gomu_report = path / "mutation-report.json"
                    gomu_report.unlink(missing_ok=True)
                    elapsed, code, stdout, stderr = run(commands[name], path, env)
                    if code not in ({0} if name == "gomu" else {0, 2}):
                        raise RuntimeError(f"{name} failed for {fixture}: {stderr}\n{stdout[:2000]}")
                    report = json.loads(gomu_report.read_text() if name == "gomu" else stdout)
                    reports[name] = report
                    record = {"seconds": elapsed}
                    if name == "gomu":
                        record["findings"] = len(report["results"])
                    else:
                        if report["incomplete"]:
                            raise RuntimeError(f"{name} produced an incomplete finding for {fixture}")
                        record.update(findings=len(report["findings"]), source_runs=report["source_runs"],
                                      probe_replays=report["mutation_replays"], killed=report["killed"],
                                      non_viable=report.get("non_viable", 0))
                    (args.output / f"{fixture}-{i}-{name}.json").write_text(json.dumps(report, indent=2) + "\n")
                    print(fixture, i, name, record, flush=True)
                    if i:
                        samples[name].append(record)

                before, after = (findings(reports[name]) for name in ("before", "after"))
                if before != after:
                    raise RuntimeError(f"full findings differ for {fixture}: {before.items() ^ after.items()}")
                mismatches = {key: (verdict, oracle.get(key)) for key, verdict in after.items()
                              if key[3] not in {"boundary", "negation", "threshold"} and verdict != oracle.get(key)}
                if mismatches:
                    raise RuntimeError(f"source-only verdicts disagree for {fixture}: {mismatches}")
                missing_steps = {key for key in oracle if key[3] == "integer-step" and key not in after}
                if missing_steps:
                    raise RuntimeError(f"full catalogue omitted integer steps for {fixture}: {missing_steps}")
                if "gomu" in reports:
                    compare_verdicts({"tmt": reports["after"], "gomu": reports["gomu"]})

            summary[fixture] = {"samples": samples, "median_seconds": {
                name: statistics.median(row["seconds"] for row in rows) for name, rows in samples.items()}}
    (args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print({fixture: result["median_seconds"] for fixture, result in summary.items()}, flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before", type=Path, required=True)
    parser.add_argument("--after", type=Path, required=True)
    parser.add_argument("--gomu", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--toolchain", default="go1.26.10")
    parser.add_argument("--sites", type=int, default=8)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--runs", type=int, default=5)
    options = parser.parse_args()
    if min(options.sites, options.workers, options.runs) <= 0:
        parser.error("--sites, --workers, and --runs must be positive")
    measure(options)
