//go:build e2e
// +build e2e

package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/kube-vip/kube-vip/testing/e2e"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
)

func (c *Cluster) expectEtcdNodeHealthy(ctx context.Context, node nodes.Node, timeout time.Duration) {
	httpClient := c.newEtcdHTTPClient()
	client := c.newEtcdClient(e2e.NodeIPv4(node))
	defer client.Close()
	nodeEtcdEndpoint := etcdEndpointForNode(node)
	err := waitForEtcdHealth(ctx, timeout, time.Second, func(probeCtx context.Context) error {
		health, err := getEtcdHealth(probeCtx, httpClient, node)
		if err != nil {
			return fmt.Errorf("checking member health: %w", err)
		}
		if !health.Healthy() {
			c.Logger.Printf("Member %s is not healthy with reason: %s", node.String(), health.Reason)
			return fmt.Errorf("member is not healthy: %s", health.Reason)
		}

		status, err := client.Status(probeCtx, nodeEtcdEndpoint)
		if err != nil {
			return fmt.Errorf("checking member status: %w", err)
		}
		if len(status.Errors) != 0 {
			return fmt.Errorf("member status contains errors: %v", status.Errors)
		}
		if status.IsLearner {
			return errors.New("member is still a learner")
		}

		alarms, err := client.AlarmList(probeCtx)
		if err != nil {
			return fmt.Errorf("listing cluster alarms: %w", err)
		}
		if len(alarms.Alarms) != 0 {
			return fmt.Errorf("cluster has alarms: %v", alarms.Alarms)
		}

		return nil
	})
	Expect(err).NotTo(HaveOccurred(), "node %s should eventually be healthy", node.String())
}

func waitForEtcdHealth(ctx context.Context, timeout, interval time.Duration, check func(context.Context) error) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		if err := deadlineCtx.Err(); err != nil {
			return etcdHealthWaitError(ctx, err, lastErr)
		}

		probeCtx, probeCancel := context.WithTimeout(deadlineCtx, 2*time.Second)
		lastErr = check(probeCtx)
		probeCancel()
		if lastErr == nil {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-deadlineCtx.Done():
			timer.Stop()
			return etcdHealthWaitError(ctx, deadlineCtx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func etcdHealthWaitError(ctx context.Context, deadlineErr, lastErr error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("waiting for etcd health: %w", ctx.Err())
	}
	if lastErr != nil {
		return fmt.Errorf("waiting for etcd health: %w (last probe: %v)", deadlineErr, lastErr)
	}
	return fmt.Errorf("waiting for etcd health: %w", deadlineErr)
}

func (c *Cluster) newEtcdHTTPClient() *http.Client {
	tlsInfo := transport.TLSInfo{
		TrustedCAFile: filepath.Join(c.EtcdCertsFolder, "ca.crt"),
		CertFile:      filepath.Join(c.EtcdCertsFolder, "etcdctl-etcd-client.crt"),
		KeyFile:       filepath.Join(c.EtcdCertsFolder, "etcdctl-etcd-client.key"),
	}

	clientTLS, err := tlsInfo.ClientConfig()
	Expect(err).NotTo(HaveOccurred())

	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: clientTLS,
		},
	}
}

type etcdHealthCheckResponse struct {
	Health string `json:"health"`
	Reason string `json:"reason"`
}

func (h *etcdHealthCheckResponse) Healthy() bool {
	return h.Health == "true"
}

func getEtcdHealth(ctx context.Context, c *http.Client, node nodes.Node) (*etcdHealthCheckResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, etcdHealthEndpoint(node), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("etcd member not ready, returned HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	health, err := parseEtcdHealthResponse(body)
	if err != nil {
		return nil, err
	}

	return health, nil
}

func etcdEndpointForNode(node nodes.Node) string {
	return e2e.NodeIPv4(node) + ":2379"
}

func etcdHealthEndpoint(node nodes.Node) string {
	return fmt.Sprintf("https://%s:2379/health", e2e.NodeIPv4(node))
}

func parseEtcdHealthResponse(data []byte) (*etcdHealthCheckResponse, error) {
	obj := &etcdHealthCheckResponse{}
	if err := json.Unmarshal(data, obj); err != nil {
		return nil, err
	}
	return obj, nil
}
