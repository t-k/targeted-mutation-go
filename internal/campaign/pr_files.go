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
	return previousMatchingFiles(ctx, dir, revision, ":(glob)**/*_test.go", prTestPath)
}

// previousTestdataFiles returns Git-tracked and untracked testdata/ changes
// between revision and the working tree.
func previousTestdataFiles(ctx context.Context, dir, revision string) ([]target.Replacement, error) {
	return previousMatchingFiles(ctx, dir, revision, ":(glob)**/testdata/**", prTestdataPath)
}

func previousMatchingFiles(ctx context.Context, dir, revision, pathspec string, validate func(root, name string) (string, error)) ([]target.Replacement, error) {
	if revision == "" {
		return nil, nil
	}
	root, commit, err := gitBase(ctx, dir, revision)
	if err != nil {
		return nil, err
	}
	tracked, err := git(ctx, root, "diff", "--raw", "--no-ext-diff", "--no-renames", "--diff-filter=AMDT", "-z", commit, "--", pathspec)
	if err != nil {
		return nil, err
	}
	untracked, err := git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z", "--", pathspec)
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
			path, err := validate(root, name)
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
		path, err := validate(root, name)
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
	return prSafePath(root, name)
}

func prTestdataPath(root, name string) (string, error) {
	name = filepath.FromSlash(name)
	if !filepath.IsLocal(name) || name != filepath.Clean(name) {
		return "", nil
	}
	if _, ok := testdataOwnerDir(filepath.Join(root, name)); !ok {
		return "", nil
	}
	return prSafePath(root, name)
}

func prSafePath(root, name string) (string, error) {
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

// testdataOwnerDir returns the package directory immediately containing a
// "testdata" path component, or ("", false) when path is not under testdata/.
func testdataOwnerDir(path string) (string, bool) {
	clean := filepath.Clean(path)
	sep := string(filepath.Separator)
	parts := strings.Split(clean, sep)
	for i, part := range parts {
		if part == "testdata" && i > 0 && i < len(parts)-1 {
			owner := strings.Join(parts[:i], sep)
			if owner == "" && strings.HasPrefix(clean, sep) {
				owner = sep
			}
			return owner, true
		}
	}
	return "", false
}

// restorePreviousTestdata materializes all Git-tracked regular files under
// pkgDir/testdata from revision into destDir/testdata.
func restorePreviousTestdata(ctx context.Context, repoDir, revision, pkgDir, destDir string) (bool, error) {
	root, commit, err := gitBase(ctx, repoDir, revision)
	if err != nil {
		return false, err
	}
	relPkg, err := filepath.Rel(root, pkgDir)
	if err != nil || !filepath.IsLocal(relPkg) && relPkg != "." {
		return false, nil
	}
	relTestdata := "testdata"
	if relPkg != "." {
		relTestdata = filepath.ToSlash(filepath.Join(relPkg, "testdata"))
	}
	tree, err := git(ctx, root, "ls-tree", "-r", "-z", commit, "--", relTestdata)
	if err != nil {
		return false, err
	}
	if len(tree) == 0 {
		return false, nil
	}
	prefix := relTestdata + "/"
	restored := false
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(tree), "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 || !regularGitFile(fields[0]) {
			continue
		}
		sub, ok := strings.CutPrefix(name, prefix)
		if !ok || sub == "" {
			continue
		}
		subPath := filepath.FromSlash(sub)
		if !filepath.IsLocal(subPath) || subPath != filepath.Clean(subPath) {
			continue
		}
		data, err := git(ctx, root, "show", commit+":"+name)
		if err != nil {
			return false, err
		}
		targetPath := filepath.Join(destDir, "testdata", subPath)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
			return false, err
		}
		mode := os.FileMode(0600)
		if fields[0] == "100755" {
			mode = 0700
		}
		if err := os.WriteFile(targetPath, data, mode); err != nil {
			return false, err
		}
		restored = true
	}
	return restored, nil
}
