package campaign

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
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
