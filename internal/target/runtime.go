package target

import (
	_ "embed"
	"path/filepath"
	"strings"
)

//go:embed probe_go.txt
var probeSource string

type Replacement struct {
	Path      string
	Line      int
	Data      []byte
	Synthetic bool
}

func Runtime(pkg Package) []Replacement {
	source := strings.NewReplacer("PROBE_PACKAGE", pkg.Name, "PROBE", pkg.Prefix).Replace(probeSource)
	return []Replacement{{Path: filepath.Join(pkg.Dir, strings.ToLower(pkg.Prefix)+"_probe.go"), Data: []byte(source), Synthetic: true}}
}
