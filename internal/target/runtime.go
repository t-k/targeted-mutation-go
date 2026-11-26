package target

import (
	_ "embed"
	"path/filepath"
	"slices"
	"strings"
)

//go:embed probe_go.txt
var probeSource string

//go:embed probe_string.go.txt
var stringProbeSource string

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
	source = strings.NewReplacer("PROBE_PACKAGE", pkg.Name, "PROBE", pkg.Prefix).Replace(source)
	return []Replacement{{Path: filepath.Join(pkg.Dir, strings.ToLower(pkg.Prefix)+"_probe.go"), Data: []byte(source), Synthetic: true}}
}
