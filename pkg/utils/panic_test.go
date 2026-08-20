package utils

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsPanicError(t *testing.T) {
	panicErr := NewPanicError("endpoint watch stopped")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "direct", err: panicErr, want: true},
		{name: "wrapped", err: fmt.Errorf("watch failed: %w", panicErr), want: true},
		{name: "ordinary", err: errors.New("watch failed"), want: false},
		{name: "nil", err: nil, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsPanicError(test.err); got != test.want {
				t.Fatalf("IsPanicError() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWrapPanicErrorPreservesCause(t *testing.T) {
	cause := errors.New("endpointslices is forbidden")
	err := WrapPanicError(cause, "endpoint watch failed")

	if !IsPanicError(err) {
		t.Fatal("expected wrapped error to be classified as PanicError")
	}
	if !errors.Is(err, cause) {
		t.Fatal("expected wrapped PanicError to preserve its cause")
	}
}
