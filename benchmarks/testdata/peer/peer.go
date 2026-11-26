package peer

import "fmt"

func Bump(x int) int {
	x++
	return x
}

func Preserve(err error) error {
	return err
}

func Wrapped(err error) error {
	return fmt.Errorf("load: %w", err)
}

func Label(s string) string {
	if s == "" {
		return "<empty>"
	}
	return s
}
