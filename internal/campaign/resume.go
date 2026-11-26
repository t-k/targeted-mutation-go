package campaign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/checkpoint"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func (s *session) openCheckpoint(ctx context.Context) error {
	path, err := filepath.Abs(s.cfg.Checkpoint)
	if err != nil {
		return err
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(parent, filepath.Base(path))
	}
	fingerprint, err := s.campaignFingerprint(ctx, path)
	if err != nil {
		return fmt.Errorf("compute checkpoint fingerprint: %w", err)
	}
	s.journal, err = checkpoint.Open[Finding](path, fingerprint)
	return err
}

func (s *session) campaignFingerprint(ctx context.Context, journalPath string) (string, error) {
	state := sha256.New()
	cfg := s.cfg
	cfg.Checkpoint = ""
	env := cleanEnv()
	slices.Sort(env)
	writeHash(state, stableHash("tmt-go-checkpoint-v1", cfg, s.env, env))
	if cfg.Since != "" {
		revision, err := git(ctx, cfg.Dir, "rev-parse", "--verify", "--end-of-options", cfg.Since+"^{commit}")
		if err != nil {
			return "", err
		}
		writeHash(state, string(revision))
	}
	seen := make(map[string]bool)
	addFile := func(path string) error {
		if seen[path] || path == journalPath {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		seen[path] = true
		return hashFile(state, path)
	}
	tool, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := addFile(tool); err != nil {
		return "", err
	}
	workspace := cfg.Dir
	if filepath.IsAbs(s.env.GOWORK) {
		workspace = filepath.Dir(s.env.GOWORK)
	} else if filepath.IsAbs(s.env.GOMOD) && s.env.GOMOD != os.DevNull {
		workspace = filepath.Dir(s.env.GOMOD)
	}
	rootSeen := make(map[string]bool)
	addTree := func(root string) error {
		if rootSeen[root] {
			return nil
		}
		rootSeen[root] = true
		return hashTree(root, addFile)
	}
	if err := addTree(workspace); err != nil {
		return "", err
	}
	if err := s.fingerprintDependencies(ctx, addFile, addTree); err != nil {
		return "", err
	}
	return hex.EncodeToString(state.Sum(nil)), nil
}

func hashTree(root string, addFile func(string) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == ".git" || entry.IsDir() && entry.Name() == ".worktree" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		return addFile(path)
	})
}

func (s *session) fingerprintDependencies(ctx context.Context, addFile, addTree func(string) error) error {
	args := append([]string{"list", "-deps", "-test", "-json"}, s.buildFlags()...)
	args = append(args, append(slices.Clone(s.cfg.Patterns), s.testPatterns()...)...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env, cmd.Dir = cleanEnv(), s.cfg.Dir
	configureProcess(cmd)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list dependency files: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	var files, localRoots []string
	for {
		var pkg struct {
			Dir                                                                  string
			GoFiles, CgoFiles, TestGoFiles, XTestGoFiles                         []string
			CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles, SwigFiles          []string
			SwigCXXFiles, SysoFiles, EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
			Module                                                               *struct {
				GoMod, Dir string
				Main       bool
				Replace    *struct{ Dir, Version string }
			}
		}
		if err := decoder.Decode(&pkg); err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
		if module := pkg.Module; module != nil {
			if module.GoMod != "" {
				files = append(files, module.GoMod)
			}
			if module.Main && module.Dir != "" {
				localRoots = append(localRoots, module.Dir)
			} else if module.Replace != nil && module.Replace.Dir != "" && module.Replace.Version == "" {
				localRoots = append(localRoots, module.Replace.Dir)
			}
		}
		for _, file := range slices.Concat(pkg.GoFiles, pkg.CgoFiles, pkg.TestGoFiles, pkg.XTestGoFiles,
			pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles,
			pkg.SwigCXXFiles, pkg.SysoFiles, pkg.EmbedFiles, pkg.TestEmbedFiles, pkg.XTestEmbedFiles) {
			if !filepath.IsAbs(file) {
				file = filepath.Join(pkg.Dir, file)
			}
			files = append(files, filepath.Clean(file))
		}
	}
	slices.Sort(localRoots)
	for _, root := range slices.Compact(localRoots) {
		if err := addTree(root); err != nil {
			return err
		}
	}
	slices.Sort(files)
	for _, path := range slices.Compact(files) {
		if err := addFile(path); err != nil {
			return err
		}
	}
	return nil
}

func hashFile(state hash.Hash, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("checkpoint input is not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	contents := sha256.New()
	if _, err := io.Copy(contents, file); err != nil {
		return err
	}
	writeHash(state, path)
	writeHash(state, info.Mode().String())
	writeHash(state, string(contents.Sum(nil)))
	return nil
}

func writeHash(state hash.Hash, field string) { fmt.Fprintf(state, "%d:%s", len(field), field) }

func stableHash(values ...any) string {
	data, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *session) cachedEdit(f Finding, site *target.Site, reached []*consumer, run func() sourceResult) sourceResult {
	if s.journal == nil {
		return run()
	}
	type observation struct {
		Package string
		Summary analysis.Summary
	}
	var trace []observation
	if site != nil {
		for _, bin := range reached {
			trace = append(trace, observation{Package: bin.pkg, Summary: bin.seen[site.ID]})
		}
	}
	key := stableHash(f.ID, s.baselineKey, trace)
	if saved, ok := s.journal.Get(key); ok && saved.ID == f.ID && checkpointVerdict(saved.Status) {
		return sourceResult{jobResult: jobResult{finding: saved}, resumed: true}
	}
	result := run()
	if result.err == nil && checkpointVerdict(result.finding.Status) {
		result.err = s.journal.Put(key, result.finding)
	}
	return result
}

func checkpointVerdict(status string) bool {
	switch status {
	case "killed", "non-viable", "input-gap", "assertion-gap", "source-survivor", "untested-change":
		return true
	}
	return false
}
