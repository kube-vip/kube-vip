package utils

import (
	"os"
)

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
