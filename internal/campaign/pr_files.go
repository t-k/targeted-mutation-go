package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

// previousTestFiles overlays the base revision's tests on the current project.
// Nil data marks a test file that did not exist in that revision.
func previousTestFiles(ctx context.Context, dir, revision string) ([]target.Replacement, error) {
	if revision == "" {
		return nil, nil
	}
	root, commit, err := gitBase(ctx, dir, revision)
	if err != nil {
		return nil, err
	}
	const tests = ":(glob)**/*_test.go"
	tracked, err := git(ctx, root, "diff", "--raw", "--no-ext-diff", "--no-renames", "--diff-filter=AMDT", "-z", commit, "--", tests)
	if err != nil {
		return nil, err
	}
	untracked, err := git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z", "--", tests)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]target.Replacement)
	if len(tracked) != 0 {
		pairs := strings.Split(strings.TrimSuffix(string(tracked), "\x00"), "\x00")
		if len(pairs)%2 != 0 {
			return nil, fmt.Errorf("malformed Git test diff")
		}
		for i := 0; i < len(pairs); i += 2 {
			meta, name := strings.Fields(pairs[i]), pairs[i+1]
			if len(meta) != 5 || !strings.HasPrefix(meta[0], ":") {
				return nil, fmt.Errorf("malformed Git test diff for %q", name)
			}
			oldMode, newMode := strings.TrimPrefix(meta[0], ":"), meta[1]
			if oldMode != "000000" && !regularGitFile(oldMode) || newMode != "000000" && !regularGitFile(newMode) {
				continue
			}
			path, err := prTestPath(root, name)
			if err != nil {
				return nil, err
			}
			if path == "" {
				continue
			}
			replacement := target.Replacement{Path: path}
			if oldMode != "000000" {
				data, err := git(ctx, root, "show", commit+":"+name)
				if err != nil {
					return nil, err
				}
				replacement.Data = append([]byte{}, data...)
			}
			selected[path] = replacement
		}
	}
	for name := range strings.SplitSeq(string(untracked), "\x00") {
		path, err := prTestPath(root, name)
		if err != nil {
			return nil, err
		}
		if _, exists := selected[path]; path != "" && !exists {
			selected[path] = target.Replacement{Path: path}
		}
	}
	result := make([]target.Replacement, 0, len(selected))
	for _, replacement := range selected {
		result = append(result, replacement)
	}
	slices.SortFunc(result, func(a, b target.Replacement) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

func regularGitFile(mode string) bool { return mode == "100644" || mode == "100755" }

// Neither an overlay key nor one of its parents may follow a symlink. Missing
// paths are expected when the working tree deleted a file or its directory.
func prTestPath(root, name string) (string, error) {
	name = filepath.FromSlash(name)
	if !filepath.IsLocal(name) || name != filepath.Clean(name) || !strings.HasSuffix(name, "_test.go") {
		return "", nil
	}
	path := root
	parts := strings.Split(name, string(filepath.Separator))
	for i, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return filepath.Join(root, name), nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect test path %q: %w", name, err)
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return "", nil
			}
		} else if !info.IsDir() {
			return "", nil
		}
	}
	return path, nil
}
