package kubevip

import "testing"

func TestParseEnvironment(t *testing.T) {

	tests := []struct {
		name    string
		c       *Config
		wantErr bool
	}{
		{"", nil, false},
		{"", &Config{Interface: "eth0", ServicesInterface: "eth1"}, false},
	}
	for _, tt := range tests {
		t.Logf("%v", tt.c)
		t.Run(tt.name, func(t *testing.T) {
			if err := ParseEnvironment(tt.c); (err != nil) != tt.wantErr {
				t.Errorf("ParseEnvironment() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
