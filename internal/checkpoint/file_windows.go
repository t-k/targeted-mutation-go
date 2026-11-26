//go:build windows

package checkpoint

import (
	"errors"
	"os"
	"syscall"
)

const (
	fileFlagOpenReparsePoint = 0x00200000
	errSharingViolation      = syscall.Errno(32)
	errLockViolation         = syscall.Errno(33)
)

func openLocked(path string) (*os.File, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open checkpoint", Path: path, Err: err}
	}
	// shareMode=0 enforces an OS-level exclusive lock until the handle is closed.
	// FILE_FLAG_OPEN_REPARSE_POINT prevents following symlinks/junctions.
	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL|fileFlagOpenReparsePoint,
		0,
	)
	if err != nil {
		if errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation) {
			return nil, &os.PathError{Op: "lock checkpoint", Path: path, Err: ErrLocked}
		}
		return nil, &os.PathError{Op: "open checkpoint", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("checkpoint must be a regular file")
	}
	if err != nil {
		_ = file.Close()
		return nil, &os.PathError{Op: "lock checkpoint", Path: path, Err: err}
	}
	return file, nil
}
