//go:build darwin || linux

package checkpoint

import (
	"errors"
	"os"
	"syscall"
)

func openLocked(path string) (*os.File, error) {
	// Nonblocking open lets us reject special files without waiting on a FIFO.
	const flags = syscall.O_RDWR | syscall.O_CREAT | syscall.O_APPEND | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	fd, err := syscall.Open(path, flags, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open checkpoint", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("checkpoint must be a regular file")
	}
	if err == nil {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			err = ErrLocked
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, &os.PathError{Op: "lock checkpoint", Path: path, Err: err}
	}
	return file, nil
}
