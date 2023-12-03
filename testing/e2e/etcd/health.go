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
	nodeEtcdEndpoint := etcdEndpointForNode(node)
	Eventually(func(g Gomega) error {
		health, err := getEtcdHealth(httpClient, node)
		g.Expect(err).NotTo(HaveOccurred())
		if !health.Healthy() {
			c.Logger.Printf("Member %s is not healthy with reason: %s", node.String(), health.Reason)
		}
		g.Expect(health.Healthy()).To(BeTrue(), "member is not healthy with reason: %s", health.Reason)
		statusCtx, statusCancel := context.WithTimeout(ctx, 2*time.Second)
		defer statusCancel()
		status, err := client.Status(statusCtx, nodeEtcdEndpoint)
		g.Expect(err).NotTo(HaveOccurred())

		g.Expect(status.Errors).To(BeEmpty(), "member should not have any errors in status")
		g.Expect(status.IsLearner).To(BeFalse(), "member should not be a learner")

		alarmsCtx, alarmsCancel := context.WithTimeout(ctx, 2*time.Second)
		defer alarmsCancel()
		alarms, err := client.AlarmList(alarmsCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(alarms.Alarms).To(BeEmpty(), "cluster should not have any alarms")

		return nil
	}, timeout).Should(Succeed(), "node %s should eventually be healthy", node.String())
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

func getEtcdHealth(c *http.Client, node nodes.Node) (*etcdHealthCheckResponse, error) {
	req, err := http.NewRequest("GET", etcdHealthEndpoint(node), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, errors.Wrapf(err, "etcd member not ready, returned http status %d", resp.StatusCode)
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
