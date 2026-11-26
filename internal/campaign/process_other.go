//go:build !darwin && !linux

package campaign

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func cleanupProcess(_ *exec.Cmd) {}
