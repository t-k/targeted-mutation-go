package campaign

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProcessEventPICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/events.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		t.Run(strings.ReplaceAll(row, "\t", "/"), func(t *testing.T) {
			f := strings.Fields(row)
			const pkg, name = "example.org/p", "TestReal"
			parser := commandOutput{direct: f[0] == "binary"}
			var wire string
			if parser.direct {
				if f[2] == "lookalike" {
					wire = "--- FAIL: TestFabricated (0.00s)\n=== RUN   TestFabricated"
				}
				wire += fmt.Sprintf("\x16=== RUN   %s\n\x16--- %s: %s (0.00s)\n", name, strings.ToUpper(f[1]), name)
			} else {
				if f[2] == "lookalike" {
					wire = "{\"Action\":\"output\",\"Test\":\"TestReal\",\"Output\":\"--- FAIL: TestFabricated\"}\n"
				}
				wire += fmt.Sprintf("{\"Action\":\"run\",\"Package\":%q,\"Test\":%q}\n", pkg, name)
				wire += fmt.Sprintf("{\"Action\":%q,\"Package\":%q,\"Test\":%q}\n", f[1], pkg, name)
			}
			if f[3] == "bytes" {
				for _, b := range []byte(wire) {
					_, _ = parser.Write([]byte{b})
				}
			} else {
				_, _ = parser.Write([]byte(wire))
			}
			events := parser.events
			if !parser.direct {
				events = parser.inventory[pkg]
			}
			if want := []string{"run:" + name, f[1] + ":" + name}; !slices.Equal(events, want) {
				t.Fatalf("events %q, want %q", events, want)
			}
			if (f[1] == "fail") != slices.Contains(parser.failed, name) || slices.Contains(parser.failed, "TestFabricated") {
				t.Fatalf("failures %q", parser.failed)
			}
		})
	}
}

func TestCommandOutputBufferAndLineBoundaries(t *testing.T) {
	const limit = 64 * 1024
	// 1. Exact 65535 bytes keeps tail of 65535; adding 1 byte hits excess == 0 (keeps all 65536); adding 1 more trims 1 byte (excess == 1).
	var out commandOutput
	_, _ = out.Write([]byte("A" + strings.Repeat("b", limit-2)))
	if out.tail.Len() != limit-1 || out.tail.String()[0] != 'A' {
		t.Fatalf("expected 65535-byte tail starting with A, got len=%d", out.tail.Len())
	}
	_, _ = out.Write([]byte("Z"))
	if out.tail.Len() != limit || out.tail.String()[0] != 'A' || out.tail.String()[limit-1] != 'Z' {
		t.Fatalf("excess==0 should retain full 65536 bytes, got len=%d first=%c", out.tail.Len(), out.tail.String()[0])
	}
	_, _ = out.Write([]byte("!"))
	if out.tail.Len() != limit || out.tail.String()[0] != 'b' || out.tail.String()[limit-1] != '!' {
		t.Fatalf("excess==1 should evict exactly 1 byte, got len=%d first=%c", out.tail.Len(), out.tail.String()[0])
	}

	// 2. Single chunk of exact limit (65536) resets tail cleanly; line of exact 65536 is accepted without moreDiagnostics.
	var exactLine commandOutput
	exactJSON := `{"Action":"pass","Package":"p","Elapsed":0.25}`
	padded := exactJSON + strings.Repeat(" ", limit-len(exactJSON))
	_, _ = exactLine.Write([]byte(padded))
	if exactLine.moreDiagnostics || exactLine.tail.Len() != limit {
		t.Fatalf("65536-byte line without newline should fit lineLimit: moreDiagnostics=%v len=%d", exactLine.moreDiagnostics, exactLine.tail.Len())
	}
	_, _ = exactLine.Write([]byte("\n"))
	if exactLine.packages["p"] != "pass" || exactLine.maxTestElapsed != 250*time.Millisecond {
		t.Fatalf("exact 65536-byte JSON line failed to parse: %+v", exactLine)
	}

	// 3. Line of 65537 bytes triggers discardLine and moreDiagnostics.
	var overLine commandOutput
	_, _ = overLine.Write([]byte(strings.Repeat("x", limit+1) + "\n"))
	if !overLine.moreDiagnostics {
		t.Fatalf("65537-byte line must set moreDiagnostics")
	}
}

func TestWorkerEnvBudgetsGOMAXPROCS(t *testing.T) {
	t.Setenv("TMT_GO_TRACE", "/tmp/old-trace")
	t.Setenv("TMT_GO_MUTATION", "1:1:0:false")
	t.Setenv(sourceWitnessEnvironment, "/tmp/old-witness")

	budgeted := (&session{workerProcs: 2}).workerEnv()
	if !slices.Contains(budgeted, "GOMAXPROCS=2") {
		t.Fatalf("expected GOMAXPROCS=2 in workerEnv, got %v", budgeted)
	}
	for _, entry := range budgeted {
		if strings.HasPrefix(entry, "TMT_GO_TRACE=") || strings.HasPrefix(entry, "TMT_GO_MUTATION=") || strings.HasPrefix(entry, sourceWitnessEnvironment+"=") {
			t.Fatalf("unexpected leaked TMT env var %q", entry)
		}
	}

	unbudgeted := (&session{workerProcs: 0}).workerEnv()
	if slices.Contains(unbudgeted, "GOMAXPROCS=0") {
		t.Fatalf("workerProcs=0 must not inject GOMAXPROCS=0")
	}

	binEnv := binaryEnv(goEnvironment{GOROOT: "/go"}, "/tmp/t", "0:1:0:false")
	if !slices.Contains(binEnv, "TMT_GO_TRACE=/tmp/t") || !slices.Contains(binEnv, "TMT_GO_MUTATION=0:1:0:false") {
		t.Fatalf("unexpected binaryEnv: %v", binEnv)
	}
}
