package services

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

func Test_detectAPIServerEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "annotation unset", value: "", want: false},
		{name: "true", value: "true", want: true},
		{name: "True", value: "True", want: true},
		{name: "false", value: "false", want: false},
		{name: "False", value: "False", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectAPIServerEnabled(tt.value); got != tt.want {
				t.Errorf("detectAPIServerEnabled(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
