package vip

import (
	"reflect"
	"testing"
)

func Test_parsePortProtocols(t *testing.T) {
	tests := []struct {
		name    string
		ports   string
		want    map[uint16]uint8
		wantErr bool
	}{
		{
			name:  "empty input",
			ports: "",
			want:  map[uint16]uint8{},
		},
		{
			name:  "single tcp port",
			ports: "tcp:80",
			want:  map[uint16]uint8{80: ProtocolTCP},
		},
		{
			name:  "multiple protocols",
			ports: "tcp:80,udp:53,sctp:9",
			want:  map[uint16]uint8{80: ProtocolTCP, 53: ProtocolUDP, 9: ProtocolSCTP},
		},
		{
			// A trailing comma produces an empty entry. The previous code ran
			// data[1] on it and panicked with an index out of range.
			name:  "trailing comma",
			ports: "tcp:80,",
			want:  map[uint16]uint8{80: ProtocolTCP},
		},
		{
			// A bare port with no protocol has only one field. The previous code
			// indexed data[1] and panicked.
			name:  "bare port without protocol",
			ports: "80",
			want:  map[uint16]uint8{},
		},
		{
			name:    "non-numeric port",
			ports:   "tcp:http",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePortProtocols(tt.ports)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePortProtocols(%q) expected error, got nil", tt.ports)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortProtocols(%q) unexpected error: %v", tt.ports, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePortProtocols(%q) = %v, want %v", tt.ports, got, tt.want)
			}
		})
	}
}
