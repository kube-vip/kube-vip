package kubevip

import "testing"

func TestCheckSubnetExists(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "vip only without subnet returns error",
			config: Config{
				VIP: "172.18.0.20",
			},
			wantErr: true,
		},
		{
			name: "vip with subnet does not return error",
			config: Config{
				VIP:       "172.18.0.20",
				VIPSubnet: "32",
			},
			wantErr: false,
		},
		{
			name: "address only without subnet does not return error",
			config: Config{
				Address: "172.18.0.20",
			},
			wantErr: false,
		},
		{
			name: "address overrides vip without subnet",
			config: Config{
				VIP:     "172.18.0.20",
				Address: "172.18.0.30",
			},
			wantErr: false,
		},
		{
			name:    "empty config does not return error",
			config:  Config{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.CheckSubnetExists()
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckSubnetExists() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
