package sysctl

import (
	"fmt"
	"io"
	"os"
)

func WriteProcSys(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		if cErr := f.Close(); cErr != nil && err == nil {
			err = fmt.Errorf("failed to close file: %w", cErr)
		}
	}()

	n, err := f.Write([]byte(value))
	if err != nil {
		return fmt.Errorf("failed to write value: %w", err)
	}
	if n < len(value) {
		return io.ErrShortWrite
	}

	return nil
}
