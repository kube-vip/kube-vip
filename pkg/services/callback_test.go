package services

import "testing"

func TestCallbackRunWithoutFunction(t *testing.T) {
	for _, callback := range []*Callback{nil, {}} {
		if err := callback.Run(nil, nil, nil); err != nil {
			t.Fatalf("Run error = %v", err)
		}
	}
}
