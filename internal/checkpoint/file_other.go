//go:build !darwin && !linux && !windows

package checkpoint

import (
	"errors"
	"os"
)

func openLocked(path string) (*os.File, error) {
	return nil, &os.PathError{Op: "open checkpoint", Path: path, Err: errors.New("checkpoints require Linux, macOS, or Windows")}
}
