package target

import (
	_ "embed"
	"path/filepath"
	"slices"
	"strings"
)

const BytesPerSite = 40

//go:embed probe_go.txt
var probeSource string

//go:embed probe_string.go.txt
var stringProbeSource string

//go:embed probe_float.go.txt
var floatProbeSource string

type Replacement struct {
	Path        string
	Line        int
	EndLine     int
	Column      int
	Description string
	Data        []byte
	Synthetic   bool
}

func Runtime(pkg Package) []Replacement {
	source := probeSource
	if slices.ContainsFunc(pkg.Files, func(file *File) bool {
		return slices.ContainsFunc(file.Sites, func(site *Site) bool { return site.String })
	}) {
		source += stringProbeSource
	}
	if slices.ContainsFunc(pkg.Files, func(file *File) bool {
		return slices.ContainsFunc(file.Sites, func(site *Site) bool { return site.Float })
	}) {
		source += floatProbeSource
	}
	source = strings.NewReplacer("PROBE_PACKAGE", pkg.Name, "PROBE", pkg.Prefix).Replace(source)
	name, _ := syntheticProbeFileName(pkg.Prefix, pkg.Paths)
	return []Replacement{{Path: filepath.Join(pkg.Dir, name), Data: []byte(source), Synthetic: true}}
}

func syntheticProbeFileName(prefix string, paths []string) (string, bool) {
	suffix := strings.ToLower(prefix) + "_probe.go"
	for _, sep := range []string{"-", "!"} {
		for count := 1; count <= 32; count++ {
			name := "0" + strings.Repeat(sep, count) + suffix
			conflict := false
			for _, p := range paths {
				base := filepath.Base(p)
				if base == "" {
					continue
				}
				if base <= name {
					conflict = true
					break
				}
			}
			if !conflict {
				return name, true
			}
		}
	}
	return "0-" + suffix, false
}
