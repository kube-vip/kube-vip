//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
)

const (
	kindNetworkName        = "kind"
	manifestDirectory      = "/etc/kubernetes/manifests"
	manifestStashDirectory = "/tmp"
	nodeReadyTimeout       = 60 * time.Second
	nodeReadyPollInterval  = time.Second
)

// PartitionNode models a node-level network partition by disconnecting the
// node container from the Kind network. Processes in the node continue to run.
func PartitionNode(clusterName, node string) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return runDockerAllow(clusterName, []string{"is not connected"}, "network", "disconnect", kindNetworkName, node)
}

// HealNode models recovery from a node-level network partition by reconnecting
// the node to the Kind network. It restarts the node if it does not become
// Ready within the recovery timeout.
func HealNode(clusterName, node string) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	if err := runDockerAllow(clusterName, []string{"already exists"}, "network", "connect", kindNetworkName, node); err != nil {
		return err
	}

	readinessErr := waitForNodeReady(clusterName, node)
	if readinessErr == nil {
		return nil
	}

	if err := runDocker(clusterName, "restart", node); err != nil {
		return fmt.Errorf("node %q in cluster %q did not become Ready and restart failed: %w (readiness error: %v)", node, clusterName, err, readinessErr)
	}

	if err := waitForNodeReady(clusterName, node); err != nil {
		return fmt.Errorf("node %q in cluster %q did not become Ready after restart: %w", node, clusterName, err)
	}

	return nil
}

// BlackholeAPIServer models loss of a node's local Kubernetes API connection
// by dropping its outbound TCP connections to the apiserver port. The node
// itself remains running and can continue to report as Ready for a while.
func BlackholeAPIServer(clusterName, node string) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return runDocker(clusterName, "exec", node, "sh", "-c",
		"iptables -C OUTPUT -p tcp --dport 6443 -j DROP || iptables -I OUTPUT -p tcp --dport 6443 -j DROP",
	)
}

// RestoreAPIServer removes the apiserver blackhole installed by
// BlackholeAPIServer, restoring the node's outbound TCP connections to port
// 6443.
func RestoreAPIServer(clusterName, node string) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return runDocker(clusterName, "exec", node, "sh", "-c",
		"while iptables -C OUTPUT -p tcp --dport 6443 -j DROP 2>/dev/null; do iptables -D OUTPUT -p tcp --dport 6443 -j DROP; done",
	)
}

// KillKubeVip models kube-vip process failure inside a node. A graceful fault
// sends SIGTERM so kube-vip can perform shutdown handling; an abrupt fault
// sends SIGKILL.
func KillKubeVip(clusterName, node string, graceful bool) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	signal := "-KILL"
	if graceful {
		signal = "-TERM"
	}

	return runDocker(clusterName, "exec", node, "pkill", signal, "kube-vip")
}

// KillAndSuppressKubeVip stops kubelet before sending SIGKILL to the single
// kube-vip process. Kind bind-mounts the static-pod manifest from the host, so
// moving that file inside the node fails with EBUSY. Stopping kubelet prevents
// a restart without modifying the test-owned host mount source.
func KillAndSuppressKubeVip(clusterName, node string) (string, error) {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return "", err
	}

	return killAndSuppressKubeVip(clusterName, node, runDocker, runDockerOutput)
}

func killAndSuppressKubeVip(clusterName, node string, run dockerRun, output dockerOutput) (pid string, err error) {
	restore := true
	defer func() {
		if !restore {
			return
		}
		if restoreErr := restoreKubelet(clusterName, node, run); restoreErr != nil {
			err = fmt.Errorf("%w; additionally failed to restore kubelet: %v", err, restoreErr)
		}
	}()
	if err := run(clusterName, "exec", node, "systemctl", "stop", "kubelet"); err != nil {
		return "", err
	}

	if err := run(clusterName, "exec", node, "sh", "-c", "! systemctl is-active --quiet kubelet"); err != nil {
		return "", fmt.Errorf("verify kubelet stopped on node %q: %w", node, err)
	}
	pids, err := output(clusterName, "exec", node, "pgrep", "-x", "kube-vip")
	if err != nil {
		return "", err
	}
	pid, err = singlePID(pids)
	if err != nil {
		return "", err
	}
	if err := run(clusterName, "exec", node, "kill", "-KILL", pid); err != nil {
		return "", err
	}

	restore = false
	return pid, nil
}

// RestoreKubeVip restarts kubelet after KillAndSuppressKubeVip and verifies
// that static-pod reconciliation is active again.
func RestoreKubeVip(clusterName, node string) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return restoreKubelet(clusterName, node, runDocker)
}

// KubeVipPID returns the PID of the single kube-vip process running in a Kind
// node. Static-pod restarts get a new PID, which lets fault tests distinguish a
// process restart from a node-container restart.
func KubeVipPID(clusterName, node string) (string, error) {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return "", err
	}

	output, err := runDockerOutput(clusterName, "exec", node, "pgrep", "-x", "kube-vip")
	if err != nil {
		return "", err
	}

	return singlePID(output)
}

// NodeRunning reports whether the Kind node container is still running.
func NodeRunning(clusterName, node string) (bool, error) {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return false, err
	}

	output, err := runDockerOutput(clusterName, "inspect", "--format={{.State.Running}}", node)
	if err != nil {
		return false, err
	}

	switch strings.TrimSpace(output) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("node %q in cluster %q returned invalid running state %q", node, clusterName, strings.TrimSpace(output))
	}
}

// RestartNode models a complete node failure and recovery by restarting its
// Docker container. The caller can use HealNode when it also needs a Ready
// check after a network fault.
func RestartNode(clusterName, node string) error {
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return runDocker(clusterName, "restart", node)
}

// DeleteLease models loss of a Kubernetes coordination lease by deleting it
// through the coordination API.
func DeleteLease(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	if client == nil {
		return fmt.Errorf("delete lease %s/%s: client is nil", namespace, name)
	}
	if err := client.CoordinationV1().Leases(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete lease %s/%s: %w", namespace, name, err)
	}

	return nil
}

// StealLease models a competing leader taking ownership of a Kubernetes
// coordination lease by overwriting its holder identity and renewal time.
func StealLease(ctx context.Context, client kubernetes.Interface, namespace, name, newHolder string) error {
	if client == nil {
		return fmt.Errorf("steal lease %s/%s: client is nil", namespace, name)
	}
	if newHolder == "" {
		return fmt.Errorf("steal lease %s/%s: new holder is empty", namespace, name)
	}

	leases := client.CoordinationV1().Leases(namespace)
	lease, err := leases.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get lease %s/%s before stealing: %w", namespace, name, err)
	}

	holder := newHolder
	lease.Spec.HolderIdentity = &holder
	renewTime := metav1.NowMicro()
	lease.Spec.RenewTime = &renewTime
	if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update stolen lease %s/%s: %w", namespace, name, err)
	}

	return nil
}

// StashPodManifest models a static-pod failure by moving a manifest out of
// the kubelet manifest directory into /tmp on the selected node.
func StashPodManifest(clusterName, node, manifestName string) error {
	src, dst, err := podManifestPaths(manifestName)
	if err != nil {
		return fmt.Errorf("stash manifest on node %q in cluster %q: %w", node, clusterName, err)
	}
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return runDocker(clusterName, "exec", node, "sh", "-c",
		fmt.Sprintf("if [ -e %s ]; then mv %s %s; elif [ ! -e %s ]; then exit 1; fi", src, src, dst, dst))
}

// RestorePodManifest models static-pod recovery by moving a stashed manifest
// back into the kubelet manifest directory on the selected node.
func RestorePodManifest(clusterName, node, manifestName string) error {
	src, dst, err := podManifestPaths(manifestName)
	if err != nil {
		return fmt.Errorf("restore manifest on node %q in cluster %q: %w", node, clusterName, err)
	}
	if err := validateFaultTarget(clusterName, node); err != nil {
		return err
	}

	return runDocker(clusterName, "exec", node, "sh", "-c",
		fmt.Sprintf("if [ -e %s ]; then mv %s %s; elif [ ! -e %s ]; then exit 1; fi", dst, dst, src, src))
}

func runDocker(clusterName string, args ...string) error {
	return runDockerAllow(clusterName, nil, args...)
}

type dockerRun func(clusterName string, args ...string) error
type dockerOutput func(clusterName string, args ...string) (string, error)

func restoreKubelet(clusterName, node string, run dockerRun) error {
	if err := run(clusterName, "exec", node, "systemctl", "start", "kubelet"); err != nil {
		if dockerContainerMissing(err) {
			return nil
		}
		return err
	}
	if err := run(clusterName, "exec", node, "systemctl", "is-active", "--quiet", "kubelet"); err != nil && !dockerContainerMissing(err) {
		return err
	}
	return nil
}

func dockerContainerMissing(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container")
}

func runDockerOutput(clusterName string, args ...string) (string, error) {
	var output bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		details := strings.TrimSpace(output.String())
		if details != "" {
			return "", fmt.Errorf("cluster %q: docker %s failed: %w: %s", clusterName, strings.Join(args, " "), err, details)
		}
		return "", fmt.Errorf("cluster %q: docker %s failed: %w", clusterName, strings.Join(args, " "), err)
	}

	return strings.TrimSpace(output.String()), nil
}

func runDockerAllow(clusterName string, allowedErrors []string, args ...string) error {
	var output bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		details := strings.TrimSpace(output.String())
		for _, allowed := range allowedErrors {
			if strings.Contains(strings.ToLower(details), strings.ToLower(allowed)) {
				return nil
			}
		}
		if details != "" {
			return fmt.Errorf("cluster %q: docker %s failed: %w: %s", clusterName, strings.Join(args, " "), err, details)
		}
		return fmt.Errorf("cluster %q: docker %s failed: %w", clusterName, strings.Join(args, " "), err)
	}

	return nil
}

func validateFaultTarget(clusterName, node string) error {
	if strings.TrimSpace(clusterName) == "" {
		return fmt.Errorf("fault target cluster name is empty")
	}
	if strings.TrimSpace(node) == "" {
		return fmt.Errorf("fault target node in cluster %q is empty", clusterName)
	}

	return nil
}

func singlePID(output string) (string, error) {
	pids := strings.Fields(output)
	if len(pids) != 1 {
		return "", fmt.Errorf("expected one kube-vip process, found %d in %q", len(pids), strings.TrimSpace(output))
	}
	return pids[0], nil
}

func podManifestPaths(manifestName string) (string, string, error) {
	if manifestName == "" || path.Base(manifestName) != manifestName || manifestName == "." || manifestName == ".." {
		return "", "", fmt.Errorf("manifest name %q must be a file name", manifestName)
	}

	manifestPath := path.Join(manifestDirectory, manifestName)
	stashPath := path.Join(manifestStashDirectory, manifestName)
	return manifestPath, stashPath, nil
}

func waitForNodeReady(clusterName, node string) error {
	client, err := clientForKindCluster(clusterName)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for cluster %q: %w", clusterName, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), nodeReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(nodeReadyPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		current, err := client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			lastErr = err
		} else if current == nil {
			lastErr = fmt.Errorf("Kubernetes returned an empty node object")
		} else if !nodeIsReady(current) {
			lastErr = fmt.Errorf("Ready condition is not True")
		} else {
			return nil
		}

		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return fmt.Errorf("node %q in cluster %q did not become Ready within %s: %w", node, clusterName, nodeReadyTimeout, lastErr)
		case <-ticker.C:
		}
	}
}

func clientForKindCluster(clusterName string) (kubernetes.Interface, error) {
	provider := kindcluster.NewProvider(kindcluster.ProviderWithDocker())
	kubeconfig, err := provider.KubeConfig(clusterName, false)
	if err != nil {
		return nil, err
	}

	config, err := ClientConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

func nodeIsReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
