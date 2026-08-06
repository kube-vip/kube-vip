package kubevip

import (
	"testing"
)

func TestParseEnvironmentSkipDAD(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset keeps default false", value: "", want: false},
		{name: "true enables", value: "true", want: true},
		{name: "false disables", value: "false", want: false},
		{name: "garbage errors", value: "not-a-bool", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv(vipSkipDAD, tc.value)
			}
			c := &Config{}
			err := ParseEnvironment(c)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.SkipDAD != tc.want {
				t.Fatalf("SkipDAD = %v, want %v", c.SkipDAD, tc.want)
			}
		})
	}
}
