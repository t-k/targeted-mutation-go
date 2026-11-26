//go:build darwin || linux || windows

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecoveryPICT(t *testing.T) {
	table, err := os.ReadFile("../../verification/pict/checkpoint.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(table)), "\n")[1:] {
		factors := strings.Fields(row)
		if len(factors) != 3 {
			t.Fatalf("invalid PICT row %q", row)
		}
		t.Run(strings.Join(factors, "/"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "campaign.jsonl")
			journal, err := Open[int](path, "campaign")
			if err != nil {
				t.Fatal(err)
			}
			count := 1
			if factors[2] == "multiple" {
				count = 3
			}
			for i := range count {
				if err := journal.Put(fmt.Sprint(i), i); err != nil {
					t.Fatal(err)
				}
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			complete := readFile(t, path)
			switch factors[1] {
			case "torn":
				// The newline is the record commit marker even for otherwise valid JSON.
				appendFile(t, path, `{"key":"tail","result":42}`)
			case "invalid":
				appendFile(t, path, "{\"key\":\"tail\"}\n")
			}
			before := readFile(t, path)
			fingerprint := "campaign"
			if factors[0] == "different" {
				fingerprint = "other"
			}
			resumed, err := Open[int](path, fingerprint)
			if factors[0] == "different" || factors[1] == "invalid" {
				want := ErrCorrupt
				if factors[0] == "different" {
					want = ErrFingerprint
				}
				if !errors.Is(err, want) || resumed != nil {
					t.Fatalf("Open = %v, %v, want %v", resumed, err, want)
				}
				if readFile(t, path) != before {
					t.Fatal("failed recovery changed the journal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resumed.Close() })
			if readFile(t, path) != complete {
				t.Fatal("recovery did not truncate exactly the torn tail")
			}
			for i := range count {
				if got, ok := resumed.Get(fmt.Sprint(i)); !ok || got != i {
					t.Fatalf("Get(%d) = %d, %t", i, got, ok)
				}
			}
			if _, ok := resumed.Get("tail"); ok {
				t.Fatal("torn result appeared after recovery")
			}
			if err := resumed.Put("tail", 42); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPutSnapshotsAndRejectsConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.jsonl")
	journal, err := Open[map[string][]int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	original := map[string][]int{"tests": {1}}
	if err := journal.Put("mutation", original); err != nil {
		t.Fatal(err)
	}
	serialized := readFile(t, path)
	if err := journal.Put("mutation", original); err != nil {
		t.Fatal(err)
	}
	original["tests"][0] = 2
	if err := journal.Put("mutation", original); err == nil {
		t.Fatal("conflicting value accepted")
	}
	got, ok := journal.Get("mutation")
	if !ok || got["tests"][0] != 1 {
		t.Fatalf("saved result changed: %v, %t", got, ok)
	}
	got["tests"][0] = 3
	if value, ok := journal.Get("mutation"); !ok || value["tests"][0] != 1 {
		t.Fatalf("mutating returned value changed snapshot: %v, %t", value, ok)
	}
	if readFile(t, path) != serialized {
		t.Fatal("duplicate or conflicting Put appended a record")
	}
}

func TestConcurrentPutAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.jsonl")
	journal, err := Open[int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	const results = 16
	var workers sync.WaitGroup
	for i := range results {
		workers.Go(func() {
			key := fmt.Sprint(i)
			if err := journal.Put(key, i); err != nil {
				t.Errorf("Put: %v", err)
				return
			}
			if got, ok := journal.Get(key); !ok || got != i {
				t.Errorf("Get(%d) = %d, %t", i, got, ok)
			}
		})
	}
	workers.Wait()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Put("after-close", 1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Put after Close returned %v", err)
	}
	resumed, err := Open[int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	for i := range results {
		if got, ok := resumed.Get(fmt.Sprint(i)); !ok || got != i {
			t.Fatalf("reopened Get(%d) = %d, %t", i, got, ok)
		}
	}
}

func TestLockAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.jsonl")
	journal, err := Open[int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.Put("known", 5); err != nil {
		t.Fatal(err)
	}
	run := func(expect string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockChild$")
		cmd.Env = append(os.Environ(), "TMT_JOURNAL_TEST_FILE="+path, "TMT_JOURNAL_TEST_EXPECT="+expect)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, output)
		}
	}
	run("locked")
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	run("resumed")
}

func TestCloseRacingWithWritersPreservesEveryAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.jsonl")
	journal, err := Open[int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Put("before", 42); err != nil {
		t.Fatal(err)
	}
	const count = 16
	begin := make(chan struct{})
	acknowledged := make(chan int, count)
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			<-begin
			key := fmt.Sprint(i)
			if err := journal.Put(key, i); err == nil {
				acknowledged <- i
			} else if !errors.Is(err, os.ErrClosed) {
				t.Errorf("Put(%s): %v", key, err)
			}
			journal.Get(key)
		})
	}
	close(begin)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	close(acknowledged)
	resumed, err := Open[int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	if got, ok := resumed.Get("before"); !ok || got != 42 {
		t.Fatalf("initial acknowledgement lost: %v, %t", got, ok)
	}
	for i := range acknowledged {
		if got, ok := resumed.Get(fmt.Sprint(i)); !ok || got != i {
			t.Errorf("lost Put(%d) after Close: %d, %t", i, got, ok)
		}
	}
}

func TestLockChild(t *testing.T) {
	path := os.Getenv("TMT_JOURNAL_TEST_FILE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	journal, err := Open[int](path, "campaign")
	if os.Getenv("TMT_JOURNAL_TEST_EXPECT") == "locked" {
		if !errors.Is(err, ErrLocked) || journal != nil {
			t.Fatalf("Open = %v, %v, want locked", journal, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if got, ok := journal.Get("known"); !ok || got != 5 {
		t.Fatalf("Get = %d, %t", got, ok)
	}
}

func TestRejectsInvalidCompleteHistory(t *testing.T) {
	for _, test := range []struct{ name, contents string }{
		{"incomplete header", `{"version":1,"fingerprint":"campaign"}`},
		{"unsupported version", "{\"version\":2,\"fingerprint\":\"campaign\"}\n"},
		{"invalid JSON", "{\"version\":1,\"fingerprint\":\"campaign\"}\n{broken\n"},
		{"invalid result type", "{\"version\":1,\"fingerprint\":\"campaign\"}\n{\"key\":\"a\",\"result\":\"x\"}\n"},
		{"conflicting results", "{\"version\":1,\"fingerprint\":\"campaign\"}\n{\"key\":\"a\",\"result\":1}\n{\"key\":\"a\",\"result\":2}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "campaign.jsonl")
			if err := os.WriteFile(path, []byte(test.contents), 0600); err != nil {
				t.Fatal(err)
			}
			if journal, err := Open[int](path, "campaign"); !errors.Is(err, ErrCorrupt) || journal != nil {
				t.Fatalf("Open = %v, %v, want corrupt", journal, err)
			}
			if readFile(t, path) != test.contents {
				t.Fatal("invalid checkpoint modified")
			}
		})
	}
}

func TestFileSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "campaign.jsonl")
	journal, err := Open[int](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		t.Fatalf("unsafe file permissions: %v, %v", info, err)
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{dir, link} {
		if journal, err := Open[int](entry, "campaign"); err == nil || journal != nil {
			t.Errorf("unsafe path %q accepted: %v, %v", entry, journal, err)
		}
	}
}

func TestFailedAppendDoesNotPublishResult(t *testing.T) {
	journal, err := Open[int](filepath.Join(t.TempDir(), "campaign.jsonl"), "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.Put("before", 42); err != nil {
		t.Fatal(err)
	}
	if err := journal.file.Close(); err != nil {
		t.Fatal(err)
	}
	failure := journal.Put("failed", 1)
	if failure == nil {
		t.Fatal("write to closed descriptor succeeded")
	}
	if err := journal.Put("subsequent", 2); err != failure {
		t.Fatalf("subsequent Put = %v, want original persistence error %v", err, failure)
	}
	if _, ok := journal.Get("failed"); ok {
		t.Fatal("failed append became visible")
	}
	if got, ok := journal.Get("before"); !ok || got != 42 {
		t.Fatalf("previous result was lost: %v, %t", got, ok)
	}
}

func TestRejectsSizeLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.jsonl")
	journal, err := Open[string](path, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	if err := journal.Put("large", strings.Repeat("x", 2<<20)); err == nil {
		t.Fatal("oversized record accepted")
	}
	if readFile(t, path) != before {
		t.Fatal("rejecting oversized record modified checkpoint")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 256<<20); err != nil {
		t.Fatal(err)
	}
	if journal, err := Open[string](path, "campaign"); err == nil || journal != nil {
		t.Fatalf("oversized checkpoint accepted: %v, %v", journal, err)
	}

	// Exact maxLineSize (1<<20) boundary tests for header and Put records.
	const exactMaxLine = 1 << 20
	// Header of exact maxLineSize vs maxLineSize+1 (note validText caps fingerprint at 4096, so pad JSON whitespace).
	validHdrPrefix := `{"version":1,"fingerprint":"campaign"`
	validHdrSuffix := "}\n"
	exactHdr := validHdrPrefix + strings.Repeat(" ", exactMaxLine-len(validHdrPrefix)-len(validHdrSuffix)) + validHdrSuffix
	exactHdrPath := filepath.Join(t.TempDir(), "exact-hdr.jsonl")
	if err := os.WriteFile(exactHdrPath, []byte(exactHdr), 0600); err != nil {
		t.Fatal(err)
	}
	if j, err := Open[string](exactHdrPath, "campaign"); err != nil {
		t.Fatalf("exact maxLineSize header rejected: %v", err)
	} else {
		_ = j.Close()
	}
	overHdr := validHdrPrefix + strings.Repeat(" ", exactMaxLine+1-len(validHdrPrefix)-len(validHdrSuffix)) + validHdrSuffix
	overHdrPath := filepath.Join(t.TempDir(), "over-hdr.jsonl")
	if err := os.WriteFile(overHdrPath, []byte(overHdr), 0600); err != nil {
		t.Fatal(err)
	}
	if j, err := Open[string](overHdrPath, "campaign"); !errors.Is(err, ErrCorrupt) || j != nil {
		t.Fatalf("maxLineSize+1 header accepted: %v, %v", j, err)
	}

	// Put record of exact maxLineSize vs maxLineSize+1, and data of exact maxLineSize.
	linePath := filepath.Join(t.TempDir(), "exact-line.jsonl")
	lj, err := Open[string](linePath, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lj.Close() })
	// json.Marshal(string) adds 2 quotes; record wrapper `{"key":"k","result":"..."}` + `\n` adds 23 bytes total around raw string.
	const wrapperOverhead = len(`{"key":"k","result":""}` + "\n")
	if err := lj.Put("k", strings.Repeat("a", exactMaxLine-wrapperOverhead)); err != nil {
		t.Fatalf("Put of exact maxLineSize line rejected: %v", err)
	}
	if err := lj.Put("k2", strings.Repeat("a", exactMaxLine-wrapperOverhead)); err == nil {
		// "k2" adds 1 byte -> exactMaxLine + 1!
		t.Fatalf("Put of maxLineSize+1 line accepted")
	}
	// len(data) == exactMaxLine (raw string of exactMaxLine - 2 quotes) must be rejected at len(data) >= maxLineSize.
	if err := lj.Put("k3", strings.Repeat("a", exactMaxLine-2)); err == nil {
		t.Fatalf("Put with len(data) == maxLineSize accepted")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func appendFile(t *testing.T, path, contents string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(contents); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
