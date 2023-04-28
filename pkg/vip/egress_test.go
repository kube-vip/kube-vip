package vip

import (
	"fmt"
	"reflect"
	"testing"
)

func Test_findRules(t *testing.T) {
	e := Egress{comment: Comment + "-" + "default"}
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
				fmt.Sprintf("-A KUBE-VIP-EGRESS -s 172.17.88.190/32 -m comment --comment \"%s\" -j MARK --set-xmark 0x40/0x40", e.comment),
				fmt.Sprintf("-A POSTROUTING -m comment --comment \"%s\" -j RETURN", e.comment),
			}},
			[][]string{{"-A", "KUBE-VIP-EGRESS", "-s", "172.17.88.190/32", "-m", "comment", "--comment", e.comment, "-j", "MARK", "--set-xmark", "0x40/0x40"}, {"-A", "POSTROUTING", "-m", "comment", "--comment", e.comment, "-j", "RETURN"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.findRules(tt.args.rules); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("findRules() = \n%v, want \n%v", got, tt.want)
			}
		})
	}
}
