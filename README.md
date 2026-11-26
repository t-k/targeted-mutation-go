# Targeted Mutation Testing for Go

`targeted-mutation-go` finds inputs and assertions missing from a Go test suite. It observes values at integer comparisons, derives viable boundary changes, and reuses test binaries to check which changes the tests catch.

For example, tests that exercise `price >= 5000` at `4800` and `5000` catch the change to `price > 5000`. A threshold of `4999` still passes. The report identifies `4999` as a distinguishing input and can compile and run a test for the original and the mutant.

## Install and run

Go 1.26 or newer, on Linux or macOS. The target must run natively with the active Go toolchain.

```sh
go install ./cmd/targeted-mutation-go

cd /path/to/go/module
targeted-mutation-go analyze --verify-witnesses ./...
```

For a package containing `func free(price int) bool { return price >= 5000 }` and the two tests above, the report includes the following finding.

```text
INPUT GAP limits.go:3:42
  expression: price >= 5000
  mutation: price >= 4999 [observed]
  observed-equivalent thresholds: 4801..5000
  add a check for free(4999) == false (mutant: true) [verified]
```

For PRs, pass the Git base before the package patterns.

```sh
base=$(git merge-base HEAD origin/main)
targeted-mutation-go analyze --since "$base" --format=json ./...
targeted-mutation-go analyze --since "$base" --undo-changes --full ./...
```

`--since` scopes mutation candidates to changed lines, including untracked Go sources. `--undo-changes` restores each edit hunk of an existing file in a separate build overlay. It reports hunks whose restoration still passes the tests. Use `--tags` to select build tags and `--jobs` to control concurrent workers. Run `targeted-mutation-go analyze --help` for options.

## Source mutation catalogue

`--full` combines the comparison probes with source mutations. `--source-only` runs the source catalogue with a separate overlay for each edit. Use `--include-generated` to add generated Go files.

| Family | Supported edits |
| --- | --- |
| Comparisons | Boundary and negation changes plus other viable relational and equality replacements |
| Arithmetic and bits | Arithmetic, bitwise and shift replacements, compound assignments, removal of the compound operator, sign changes, increment/decrement swaps |
| Conditions and branches | Boolean operand replacement, logical swaps and removal of negation, forced and negated conditions, break/continue swaps, early range breaks, empty blocks and cases |
| Values and returns | Integer and float literal steps, string and boolean literals, type-appropriate zero values, forced boolean returns, nil returns |
| Errors and statements | Nil error checks, `%w` to `%v` error wrapping, statement deletion, discarded assignment results |

The catalogue was built against Gremlins 0.6.0, gomutants 0.7.3, gomu 0.3.0 and go-mutants 0.1.2. The peer fixture checks representative outcomes across those versions. Actual candidates depend on the selected files, build tags, scope, and whether the edited program compiles. [Peer verification and performance](benchmarks/README.md)

## Findings

| Finding | Evidence |
| --- | --- |
| `INPUT GAP` | The recorded comparisons agree under the original and the mutant. A distinguishing operand is shown when the tool can derive one. `--verify-survivors` replays the mutation to check this prediction. |
| `ASSERTION GAP` | The mutant changed a comparison result in the listed test packages and the same tests still passed. |
| `SOURCE SURVIVOR` | The source edit passed the selected suite, or a comparison probe established that the edit survived or agreed with the recorded comparisons. |
| `UNTESTED CHANGE` | The selected suite passed after a Git hunk was restored. |
| `UNREACHED` | The selected test binaries left this comparison unobserved. Use `--show-unreached` to list it. |
| `NON VIABLE` | A Go language rule or compiler diagnostics establish that the edit cannot compile. |
| `UNSTABLE`, `TIMEOUT`, `BUILD ERROR`, `ERROR` | The campaign could not establish a verdict. |

A witness marked `verified` passed on the original and failed with the mutant applied. A suggested operand can be useful even when the tool cannot derive an end-to-end test call. Automatic witness compilation currently supports direct comparisons of a single integer parameter in a standalone function.

JSON reports use `schema: 1`. Integers in thresholds and witnesses are decimal strings to preserve 64-bit values. Exit codes are `0` for a complete run with no gaps, `1` for usage, baseline, or infrastructure errors, `2` for a complete run with gaps, and `3` for a run with incomplete findings.

## How it works

1. Resolve the selected Go packages and type-check their source using the active Go build context.
2. Run the ordinary suite. In an isolated build overlay, add typed comparison probes and compile a test binary per consumer package. Confirm that the binary runs the same tests with the same terminal dispositions.
3. Observe comparison results and the nearest values on either side. Use the observations to derive test-equivalent thresholds and skip binary replays whose observed comparison results stay the same.
4. Give each remaining replay a fresh process and a private trace. Require evidence that the mutation was reached and changed a result before assigning an assertion-gap or killed verdict.

The probes support integer ordering and equality, including named integer types and architecture-sized integers. `--full` also probes eligible string comparisons, including named string types. Tests in other selected packages can cover a site. The analysis conservatively leaves contexts that can change evaluation order or compile-time semantics out of instrumentation, including some expressions containing calls, constant-length range expressions, and generated or cgo-transformed source. Package-level tracing accommodates subtests and parallel tests. The guarantees about pruning apply to deterministic tests and the selected Go build configuration.

Use explicit module package patterns at a workspace root that has no Go module, for example `./app/... ./lib/...`. Only compatible build flags are accepted in `GOFLAGS`. Pass test timeouts and build tags through the CLI.

`--full` reuses the comparison binaries for eligible operator replacements, adjacent integer literal changes, and boolean replacements of comparison expressions. The JSON `evidence` field records `observed` for agreement on the recorded comparisons, `replayed` for a binary replay, `source-replay` for a separate source overlay, and `static` for an edit that empties a function body with required results. An unconfirmed comparison replay falls back to its source overlay. `--source-only` builds every candidate independently.

Source replay shares original snapshots and creates each edited file inside a worker. It can retry once when the edit leaves an unused import or local variable. The retry preserves import initialization and the local's initializer. The ordinary baseline runs Go vet. Source discovery can include original Go files in cgo packages and uses the compiler to validate edits that require cgo.

The command runs target build tools and tests with your permissions. Run it on code you trust. The tool's own source edits and test binaries live in temporary overlays.

## Development

```sh
go test ./...
go vet ./...
go test -race ./...
python3 scripts/check-pict.py
python3 scripts/check-mutants.py
lean +leanprover/lean4:v4.36.0 verification/lean/Threshold.lean
tla2tools -workers 2 verification/tla/Campaign.tla
tla2tools -workers 2 verification/tla/SourceReplay.tla
```

Lean checks integer ordering, summaries, and distinguishing inputs. TLC checks a finite model of baseline, worker, activation, and cleanup transitions. The Go tests exercise generated PICT combinations and real test binaries. The measured workloads and reproducible commands are in the [benchmark guide](benchmarks/README.md).

Related implementations include [Rust](https://github.com/t-k/targeted-mutation) and [JavaScript/TypeScript](https://github.com/t-k/targeted-mutation-ts).

Licensed under [Apache 2.0](LICENSE).
