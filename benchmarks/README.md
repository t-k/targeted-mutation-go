# Go mutation testing benchmark

The fixture contains 24 individually tested integer comparisons. At each site, tests execute both sides and leave a gap at the exact boundary. The harness checks each expected survivor and rejects results that do not match the fixture before computing medians.

```sh
benchmarks/install.sh docs.local/bench/bin
python3 benchmarks/compare.py --binaries docs.local/bench/bin --outputs docs.local/bench/results
```

The harness defaults to Go 1.26.10 because go-mutants 0.1.2 cannot decode Go 1.27 export data. It gives each tool four workers, uses a shared warmed Go build cache, turns off mutation outcome caching where supported, and rotates tool order across five measured serial rounds after a warm-up. Tool versions are pinned in `install.sh`.

`tmt-observed` derives the result from runtime observations. `tmt-replayed` requires a separate mutation replay for every compared boundary. Gremlins, gomutants, and go-mutants are configured to run that same set. gomu measures its default catalogue and is reported independently. The harness keeps raw reports, stdout, stderr, and a machine-readable timing summary in the output directory.

## Recorded result

macOS arm64, Go 1.26.10, four workers, five measured runs. Every tool below produced the expected verdict on all 24 shared boundary mutations. [Timing samples](results/macos-arm64-go1.26.10.json)

| Tool | Median wall time |
| --- | ---: |
| `targeted-mutation-go`, replay every boundary | 1.89 s |
| `targeted-mutation-go`, derive from observations | 2.49 s |
| go-mutants 0.1.2 | 6.53 s |
| Gremlins 0.6.0 | 6.63 s |
| gomutants 0.7.3 | 10.26 s |

The observation-based run avoided 24 mutation replays per sample. The total includes package discovery, the ordinary baseline, and the instrumented build. The two TMT timings reflect that full workflow.

gomu 0.3.0 reported the same boundary survivors as part of its 192-mutation default catalogue. Its median for that catalogue was 38.99 s.

## Source operator compatibility

The source fixture exercises increment reversal, error swallowing, error unwrapping, string literals, and relational changes. Each tool runs in a separate temporary target copy. The harness normalizes and compares the verdict for every distinct executable mutation the peers generate on this fixture. It saves the full reports.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 benchmarks/peer_parity.py --binaries docs.local/bench/bin --outputs docs.local/bench/source-parity
```

All matching cases below had the same verdict with Go 1.26.10. The applied profile for each tool appears in the harness.

| Peer | Distinct executable mutations matched |
| --- | ---: |
| Gremlins 0.6.0 | 2 |
| gomutants 0.7.3 | 4 |
| go-mutants 0.1.2 | 3 |
| gomu 0.3.0 | 14 |

## Source queue allocations

```sh
GOMAXPROCS=4 go test ./internal/campaign -run '^$' -bench '^BenchmarkSourceQueue$' -benchtime=5x -count=3
```

On macOS arm64, queuing 1,024 mutations against a 64 KiB source file allocated about 64 MiB with eager file copies and 1.03 MiB when sharing the original. Each worker now creates the edited file at replay time. This measurement covers preparation of the queue. Separate Go build and test processes determine most of the full source campaign's external work.
