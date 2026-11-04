package mailstore

import (
	"context"
	"errors"
	"testing"
)

func TestBackgroundMark(t *testing.T) {
	t.Parallel()
	type key struct{}
	plain := context.Background()
	marked := WithBackground(plain)
	child, cancel := context.WithCancel(context.WithValue(marked, key{}, "x"))
	defer cancel()
	tests := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"plain", plain, false},
		{"marked", marked, true},
		{"child of a marked context", child, true},
		{"marked twice", WithBackground(marked), true},
	}
	for _, tt := range tests {
		if got := IsBackground(tt.ctx); got != tt.want {
			t.Errorf("IsBackground(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
	if IsBackground(plain) {
		t.Error("WithBackground changed its parent")
	}
}

func TestErrBusyIsItsOwnError(t *testing.T) {
	t.Parallel()
	for _, other := range []error{ErrUnavailable, ErrConnLimit, ErrNotFound, ErrStaleRef, ErrUnsupported} {
		if errors.Is(ErrBusy, other) || errors.Is(other, ErrBusy) {
			t.Errorf("ErrBusy and %v match each other", other)
		}
	}
	if ErrBusy.Error() != "no idle connection for background work" {
		t.Errorf("ErrBusy = %q", ErrBusy.Error())
	}
}
