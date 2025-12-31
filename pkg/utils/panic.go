package utils

import "fmt"

type PanicError struct {
	cause string
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("%s - unrecoverable error", e.cause)
}

func NewPanicError(cause string) error {
	return &PanicError{cause: cause}
}
