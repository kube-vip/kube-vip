package kubevip

import "testing"

func TestGeneratePodSpecLoseLeadershipTimeoutUsesDedicatedEnvironmentVariable(t *testing.T) {
	pod, err := generatePodSpec(&Config{
		LoseLeadership:               true,
		LoseLeadershipTimeoutSeconds: 45,
	}, "ghcr.io/kube-vip/kube-vip", "v0.0.0", true)
	if err != nil {
		t.Fatalf("generatePodSpec() error: %v", err)
	}

	values := make(map[string][]string)
	for _, env := range pod.Spec.Containers[0].Env {
		values[env.Name] = append(values[env.Name], env.Value)
	}

	if got := values[vipLoseLeadership]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("%s environment = %v, want [true]", vipLoseLeadership, got)
	}
	if got := values[vipLoseLeadershipTimeoutSeconds]; len(got) != 1 || got[0] != "45" {
		t.Fatalf("%s environment = %v, want [45]", vipLoseLeadershipTimeoutSeconds, got)
	}
}
