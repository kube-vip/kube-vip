package utils

import (
	"errors"
	"fmt"
)

type PanicError struct {
	cause error
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("%s - unrecoverable error", e.cause)
}

func (e *PanicError) Unwrap() error {
	return e.cause
}

func NewPanicError(format string, args ...any) error {
	return &PanicError{cause: fmt.Errorf(format, args...)}
}

func WrapPanicError(err error, format string, args ...any) error {
	return &PanicError{cause: fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)}
}

func IsPanicError(err error) bool {
	var panicErr *PanicError
	return errors.As(err, &panicErr)
}
