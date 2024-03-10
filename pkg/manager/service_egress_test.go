package manager

import "testing"

func Test_getSameFamilyCidr(t *testing.T) {
	type args struct {
		source string
		ip     string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "This is wrong",
			args: args{
				source: "10.96.0.0",
				ip:     "172.16.0.1",
			},
			want: "10.96.0.0",
		},
		{
			name: "This returns the correct cidr",
			args: args{
				source: "10.96.0.0/16",
				ip:     "10.96.0.1",
			},
			want: "10.96.0.0/16",
		},
		{
			name: "This returns the correct cidr from multiple",
			args: args{
				source: "192.168.0.0/16,10.96.0.0/16",
				ip:     "10.96.0.1",
			},
			want: "10.96.0.0/16",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getSameFamilyCidr(tt.args.source, tt.args.ip); got != tt.want {
				t.Errorf("getSameFamilyCidr() = %v, want %v", got, tt.want)
			}
		})
	}
}
