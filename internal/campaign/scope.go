package campaign

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

var hunkHeader = regexp.MustCompile(`(?m)^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func gitScope(ctx context.Context, dir, revision string, undo bool) (target.Scope, map[string][]target.Replacement, error) {
	if revision == "" {
		return nil, nil, nil
	}
	rootText, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, err
	}
	root := strings.TrimSuffix(string(rootText), "\n")
	commitText, err := git(ctx, root, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return nil, nil, err
	}
	commit := strings.TrimSpace(string(commitText))
	tracked, err := git(ctx, root, "diff", "--no-renames", "--name-only", "--diff-filter=AMT", "-z", commit, "--")
	if err != nil {
		return nil, nil, err
	}
	untracked, err := git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, nil, err
	}
	scope := make(target.Scope)
	reverts := make(map[string][]target.Replacement)
	for name := range strings.SplitSeq(string(untracked), "\x00") {
		if isGoSource(name) {
			scope[filepath.Join(root, name)] = []target.Span{{Start: 1, End: math.MaxInt}}
		}
	}
	for name := range strings.SplitSeq(string(tracked), "\x00") {
		if !isGoSource(name) {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		patch, err := git(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", commit, "--", ":(literal)"+name)
		if err != nil {
			return nil, nil, err
		}
		matches := hunkHeader.FindAllSubmatch(patch, -1)
		var current, previous [][]byte
		if undo && len(matches) > 0 && !bytes.Contains(patch, []byte("\n--- /dev/null\n")) {
			before, err := git(ctx, root, "show", commit+":"+name)
			if err != nil {
				return nil, nil, err
			}
			after, err := os.ReadFile(path)
			if err != nil {
				return nil, nil, err
			}
			previous, current = sourceLines(before), sourceLines(after)
		}
		for _, hunk := range matches {
			oldStart, oldCount, newStart, newCount := number(hunk[1], 0), number(hunk[2], 1), number(hunk[3], 0), number(hunk[4], 1)
			line := max(1, newStart)
			scope[path] = append(scope[path], target.Span{Start: line, End: max(line, newStart+newCount-1)})
			if previous == nil {
				continue
			}
			oldOffset, newOffset := hunkOffset(oldStart, oldCount), hunkOffset(newStart, newCount)
			if oldOffset < 0 || newOffset < 0 || oldOffset+oldCount > len(previous) || newOffset+newCount > len(current) {
				return nil, nil, fmt.Errorf("working file changed while reading Git diff: %s", name)
			}
			data := append([]byte{}, bytes.Join(current[:newOffset], nil)...)
			data = append(data, bytes.Join(previous[oldOffset:oldOffset+oldCount], nil)...)
			data = append(data, bytes.Join(current[newOffset+newCount:], nil)...)
			dir := filepath.Dir(path)
			reverts[dir] = append(reverts[dir], target.Replacement{Path: path, Data: data, Line: line})
		}
	}
	return scope, reverts, nil
}

func number(text []byte, fallback int) int {
	if len(text) == 0 {
		return fallback
	}
	n, _ := strconv.Atoi(string(text))
	return n
}

func hunkOffset(start, count int) int {
	if count == 0 {
		return start
	}
	return start - 1
}

func sourceLines(data []byte) [][]byte {
	lines := bytes.SplitAfter(data, []byte{'\n'})
	if len(lines[len(lines)-1]) == 0 {
		return lines[:len(lines)-1]
	}
	return lines
}

func isGoSource(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	configureProcess(cmd)
	out, err := cmd.Output()
	if err != nil {
		if failure, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(failure.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}
