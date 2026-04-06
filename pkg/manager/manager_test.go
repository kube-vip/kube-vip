package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeNodeName(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		expected string
	}{
		{
			name:     "All lowercase hostname remains unchanged",
			hostname: "worker-node-1",
			expected: "worker-node-1",
		},
		{
			name:     "Mixed case hostname is lowercased",
			hostname: "Worker-Node-1",
			expected: "worker-node-1",
		},
		{
			name:     "All uppercase hostname is lowercased",
			hostname: "MASTER-NODE",
			expected: "master-node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeNodeName(tt.hostname)
			assert.Equal(t, tt.expected, result, "The normalized node name did not match the expected RFC1123 compliant name")
		})
	}
}
