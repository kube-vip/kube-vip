package sysctl

import (
	"fmt"
	"io"
	"os"
	"strconv"
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

func CheckProcSys(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false, fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		if cErr := f.Close(); cErr != nil && err == nil {
			err = fmt.Errorf("failed to close file: %w", cErr)
		}
	}()

	buffer := make([]byte, 1)

	if n, err := f.Read(buffer); err != nil || n < len(buffer) {
		return false, fmt.Errorf("failed to read file: %w", err)
	}

	var isEnabled bool
	isEnabled, err = strconv.ParseBool(string(buffer))
	if err != nil {
		return false, fmt.Errorf("failed to parse value: %w", err)
	}

	return isEnabled, nil
}

func EnableProcSys(path string) (bool, error) {
	isEnabled, err := CheckProcSys(path)
	if err != nil {
		return false, fmt.Errorf("failed to check '%s' status: %w", path, err)
	}
	if !isEnabled {
		if err := WriteProcSys(path, "1"); err != nil {
			if os.IsPermission(err) {
				return false, fmt.Errorf("no permission to write to the file '%s' - please ensure that kube-vip is running with proper capabilities/privileged mode to write to sysfs: %w", path, err)
			}
			return false, fmt.Errorf("failed to enable '%s': %w", path, err)
		}

		return true, nil
	}
	return false, nil
}
