//go:build !darwin && !linux

package cli

import "testing"

func assertOrphanReaped(_ *testing.T, _ int) {}
