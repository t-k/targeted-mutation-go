package campaign

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
	"golang.org/x/tools/go/packages"
)

func (s *session) testPatterns() []string {
	if len(s.cfg.TestPatterns) != 0 {
		return s.cfg.TestPatterns
	}
	return s.cfg.Patterns
}

func resolveTestPackages(ctx context.Context, cfg Config) ([]target.Package, error) {
	options := &packages.Config{Context: ctx, Dir: cfg.Dir, Mode: packages.NeedName | packages.NeedFiles}
	if cfg.Tags != "" {
		options.BuildFlags = []string{"-tags=" + cfg.Tags}
	}
	loaded, err := packages.Load(options, cfg.TestPatterns...)
	if err != nil {
		return nil, fmt.Errorf("load test packages: %w", err)
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("no test packages match %s", strings.Join(cfg.TestPatterns, " "))
	}
	result := make([]target.Package, 0, len(loaded))
	for _, pkg := range loaded {
		if len(pkg.Errors) != 0 {
			return nil, fmt.Errorf("load test package %s: %s", pkg.PkgPath, pkg.Errors[0])
		}
		result = append(result, target.Package{Path: pkg.PkgPath, Dir: pkg.Dir, Name: pkg.Name})
	}
	slices.SortFunc(result, func(a, b target.Package) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

func explicitScope(dir string, selectors []string) (target.Scope, error) {
	scope := make(target.Scope)
	for _, selector := range selectors {
		file := selector
		span := target.Span{Start: 1, End: math.MaxInt}
		if !strings.HasSuffix(selector, ".go") {
			separator := strings.LastIndexByte(selector, ':')
			if separator < 0 {
				return nil, fmt.Errorf("invalid --lines %q: expected FILE, FILE:LINE, or FILE:FROM-TO", selector)
			}
			file = selector[:separator]
			first, last, ranged := strings.Cut(selector[separator+1:], "-")
			if !ranged {
				last = first
			}
			start, leftErr := strconv.Atoi(first)
			end, rightErr := strconv.Atoi(last)
			if leftErr != nil || rightErr != nil || start < 1 || end < start {
				return nil, fmt.Errorf("invalid line range in --lines %q", selector)
			}
			span = target.Span{Start: start, End: end}
		}
		if !isGoSource(file) {
			return nil, fmt.Errorf("--lines requires a production Go file: %q", file)
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(dir, file)
		}
		path, err := filepath.EvalSymlinks(file)
		if err != nil {
			return nil, fmt.Errorf("--lines %q: %w", selector, err)
		}
		scope[path] = append(scope[path], span)
	}
	return scope, nil
}

func intersectScope(a, b target.Scope) target.Scope {
	if a == nil {
		return b
	}
	result := make(target.Scope)
	for path, spans := range b {
		for _, left := range a[path] {
			for _, right := range spans {
				from, to := max(left.Start, right.Start), min(left.End, right.End)
				if from <= to {
					result[path] = append(result[path], target.Span{Start: from, End: to})
				}
			}
		}
	}
	return result
}
