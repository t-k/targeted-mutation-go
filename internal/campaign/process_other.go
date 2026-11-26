//go:build !darwin && !linux && !windows

package campaign

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func cleanupProcess(_ *exec.Cmd) {}
