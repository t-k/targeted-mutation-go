package peer

import (
	"errors"
	"testing"
)

func TestBump(t *testing.T) {
	if Bump(4) != 5 {
		t.Fatal("wrong increment")
	}
}

func TestErrors(t *testing.T) {
	cause := errors.New("permission denied")
	if Preserve(cause) != cause || !errors.Is(Wrapped(cause), cause) {
		t.Fatal("lost the original error")
	}
}

func TestLabels(t *testing.T) {
	if Label("roof") != "roof" || Label("") != "<empty>" {
		t.Fatal("wrong label")
	}
}
