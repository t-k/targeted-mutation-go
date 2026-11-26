# Go mutation testing benchmark

The fixture contains 24 individually tested integer comparisons. At each site, tests execute both sides and leave a gap at the exact boundary. The harness checks each expected survivor and rejects results that do not match the fixture before computing medians.

```sh
benchmarks/install.sh docs.local/bench/bin
python3 benchmarks/compare.py --binaries docs.local/bench/bin --outputs docs.local/bench/results
```

The harness defaults to Go 1.26.10 because go-mutants 0.1.2 cannot decode Go 1.27 export data. It gives each tool four workers, uses a shared warmed Go build cache, turns off mutation outcome caching where supported, and rotates tool order across five measured serial rounds after a warm-up. Tool versions are pinned in `install.sh`.

`tmt-observed` derives the result from runtime observations. `tmt-replayed` requires a separate mutation replay for every compared boundary. Gremlins, gomutants, and go-mutants are configured to run that same set. gomu measures its default catalogue and is reported independently. The harness keeps raw reports, stdout, stderr, and a machine-readable timing summary in the output directory.
