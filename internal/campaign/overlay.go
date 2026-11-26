package campaign

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

const bytesPerSite = 24

func makeOverlay(dir string, files []target.Replacement) (string, error) {
	mapping := make(map[string]string, len(files))
	for i, file := range files {
		if _, exists := mapping[file.Path]; exists {
			return "", fmt.Errorf("duplicate overlay entry for %s", file.Path)
		}
		if file.Data == nil {
			mapping[file.Path] = ""
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("source-%d.go", i))
		if err := os.WriteFile(path, file.Data, 0600); err != nil {
			return "", err
		}
		mapping[file.Path] = path
	}
	data, err := json.Marshal(struct{ Replace map[string]string }{mapping})
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", err
	}
	return path, nil
}

func newTrace(dir string, sites int) (string, error) {
	file, err := os.CreateTemp(dir, "trace-")
	if err != nil {
		return "", err
	}
	truncateErr := file.Truncate(int64(sites) * bytesPerSite)
	closeErr := file.Close()
	if truncateErr != nil {
		return "", truncateErr
	}
	return file.Name(), closeErr
}

func readTrace(path string, sites int) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != sites*bytesPerSite {
		return nil, fmt.Errorf("invalid trace size: got %d, want %d", len(data), sites*bytesPerSite)
	}
	return data, nil
}

func summary(data []byte, site int) analysis.Summary {
	start := site * bytesPerSite
	return analysis.Summary{
		Flags: binary.NativeEndian.Uint64(data[start:]),
		Below: binary.NativeEndian.Uint64(data[start+8:]),
		Above: ^binary.NativeEndian.Uint64(data[start+16:]),
	}
}

func readFlags(path string, site int) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var word [8]byte
	_, err = file.ReadAt(word[:], int64(site)*bytesPerSite)
	if err != nil {
		return 0, err
	}
	return binary.NativeEndian.Uint64(word[:]), nil
}
