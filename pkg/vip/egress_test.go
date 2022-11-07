package vip

import (
	"fmt"
	"reflect"
	"testing"
)

func Test_findRules(t *testing.T) {
	type args struct {
		rules []string
	}
	tests := []struct {
		name string
		args args
		want [][]string
	}{

		{
			"test",
			args{[]string{
				"-A PREROUTING -m comment --comment \"cali:6gwbT8clXdHdC1b1\" -j cali-PREROUTING",
				fmt.Sprintf("-A KUBE-VIP-EGRESS -s 172.17.88.190/32 -m comment --comment \"%s\" -j MARK --set-xmark 0x40/0x40", Comment),
				fmt.Sprintf("-A POSTROUTING -m comment --comment \"%s\" -j RETURN", Comment),
			}},
			[][]string{{"-A", "KUBE-VIP-EGRESS", "-s", "172.17.88.190/32", "-m", "comment", "--comment", Comment, "-j", "MARK", "--set-xmark", "0x40/0x40"}, {"-A", "POSTROUTING", "-m", "comment", "--comment", Comment, "-j", "RETURN"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findRules(tt.args.rules); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("findRules() = \n%v, want \n%v", got, tt.want)
			}
		})
	}
}
