#!/usr/bin/env python3
"""Compare tools on an identical set of integer boundary mutations."""

import argparse
import json
import os
from pathlib import Path
import platform
import signal
import statistics
import subprocess
import tempfile
import time


def make_fixture(directory, sites):
    directory.mkdir()
    directory.joinpath("go.mod").write_text("module example.org/thresholds\n\ngo 1.26.0\n")
    source = ["package thresholds", ""]
    tests = ["package thresholds", "", 'import "testing"', ""]
    for i in range(sites):
        source.append(f"func limit{i}(v int) bool {{ return v >= {100+i} }}")
        tests.append(
            f"func TestLimit{i}(t *testing.T) {{ "
            f'if limit{i}({i}) || !limit{i}({102+i}) {{ t.Fatal("wrong boundary") }} }}'
        )
    directory.joinpath("limits.go").write_text("\n".join(source) + "\n")
    directory.joinpath("limits_test.go").write_text("\n".join(tests) + "\n")


def run(command, cwd, env):
    start = time.perf_counter()
    with subprocess.Popen(
        command, cwd=cwd, env=env, text=True, start_new_session=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    ) as process:
        try:
            stdout, stderr = process.communicate(timeout=180)
        except BaseException:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()
            raise
    return time.perf_counter() - start, process.returncode, stdout, stderr


def compare(binaries, sites, runs, toolchain, outputs, workers):
    def binary(name):
        return str(binaries / name)

    tmt = [binary("targeted-mutation-go"), "analyze", f"--jobs={workers}", "--format=json"]
    commands = {
        "tmt-observed": (tmt + ["--comparison=boundary", "."], {2}, None),
        "tmt-replayed": (tmt + ["--comparison=boundary", "--conventional", "."], {2}, None),
        "gremlins": ([
            binary("gremlins"), "unleash", f"--workers={workers}", "--timeout-coefficient=3000",
            "--arithmetic-base=false", "--increment-decrement=false",
            "--invert-negatives=false", "--conditionals-negation=false",
            "--output=gremlins.json", ".",
        ], {0}, "gremlins.json"),
        "gomutants": ([
            binary("gomutants"), f"--workers={workers}", "--cache=off",
            "--only=CONDITIONALS_BOUNDARY", "--timeout-min=10s", "--quiet", "--output=gomutants.json", ".",
        ], {0}, "gomutants.json"),
        "go-mutants": ([
            binary("go-mutants"), "run", "--operator=comparison", f"--jobs={workers}",
            "--cache=off", "--report=none", "--no-tui", "--json", "--timeout=30s",
        ], {0}, None),
        "gomu-default": ([
            binary("gomu"), "run", f"--workers={workers}", "--incremental=false",
            "--fail-on-gate=false", "--output=json", ".",
        ], {0}, "mutation-report.json"),
    }
    samples = {name: [] for name in commands}
    env = dict(os.environ, GOTOOLCHAIN=toolchain, GOMAXPROCS=str(workers), GOFLAGS="")
    names = list(commands)
    outputs.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="tmt-go-bench-") as scratch:
        directory = Path(scratch) / "thresholds"
        make_fixture(directory, sites)
        _, status, version, error = run(["go", "version"], directory, env)
        if status:
            raise RuntimeError(error)
        for round_index in range(runs + 1):
            order = names[round_index % len(names):] + names[:round_index % len(names)]
            for name in order:
                command, expected_codes, report_file = commands[name]
                path = directory / report_file if report_file else None
                if path:
                    path.unlink(missing_ok=True)
                elapsed, code, stdout, stderr = run(command, directory, env)
                evidence = outputs / f"{round_index:02d}-{name}"
                evidence.with_suffix(".stderr").write_text(stderr)
                evidence.with_suffix(".stdout").write_text(stdout)
                if code not in expected_codes:
                    raise RuntimeError(f"{name} exited {code}. See {evidence}.stderr")
                payload = json.loads(path.read_text() if path else stdout)
                evidence.with_suffix(".json").write_text(json.dumps(payload, indent=2) + "\n")
                verify(name, payload, sites)
                if round_index:
                    samples[name].append(elapsed)
                print(f"{round_index:2} {name:16} {elapsed:.3f}s", flush=True)
    result = {
        "host": platform.platform(), "go": version.strip(), "workers": workers,
        "sites": sites, "runs": runs, "seconds": samples,
        "median_seconds": {name: statistics.median(values) for name, values in samples.items()},
    }
    outputs.joinpath("summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result["median_seconds"], indent=2))


def verify(name, payload, sites):
    if name.startswith("tmt-"):
        boundaries = [f for f in payload["findings"] if f["rule"] == "boundary"]
        counts = [f["line"] for f in boundaries if f["status"] == "input-gap"]
        expected_replays = sites if name == "tmt-replayed" else 0
        if payload["mutation_replays"] != expected_replays or payload["incomplete"] != 0:
            raise RuntimeError(f"{name} did not run the expected number of mutation processes")
    elif name in {"gremlins", "gomutants"}:
        counts = [
            m["line"] for f in payload["files"] for m in f["mutations"]
            if m["type"] == "CONDITIONALS_BOUNDARY" and m["status"] == "LIVED"
        ]
    elif name == "go-mutants":
        counts = [
            m["line"] for m in payload["mutants"]
            if m["outcome"] == "survived" and m["rule"] == "ge-to-gt"
        ]
    else:
        # gomu's default catalogue is timed as a separate workload.
        counts = [
            m["mutant"]["line"] for m in payload["results"]
            if m["mutant"]["original"] == ">=" and m["mutant"]["mutated"] == ">"
            and m["status"] == "SURVIVED"
        ]
    if sorted(counts) != list(range(3, sites + 3)):
        raise RuntimeError(f"{name} did not produce the expected boundary survivor at every site")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binaries", type=Path, required=True)
    parser.add_argument("--outputs", type=Path, required=True)
    parser.add_argument("--go", default="go1.26.10")
    parser.add_argument("--sites", type=int, default=24)
    parser.add_argument("--runs", type=int, default=5)
    parser.add_argument("--workers", type=int, default=4)
    args = parser.parse_args()
    if min(args.sites, args.runs, args.workers) < 1:
        parser.error("sites, runs, and workers must be positive")
    compare(args.binaries.resolve(), args.sites, args.runs, args.go, args.outputs.resolve(), args.workers)
