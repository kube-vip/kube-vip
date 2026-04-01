package cluster_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestInitCluster_HealthCheckClientNoCA(t *testing.T) {
	t.Parallel()
	cfg := &kubevip.Config{
		EnableBGP: true,
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address:        "http://localhost:6443/livez",
			TimeoutSeconds: 5,
		},
	}

	_, err := cluster.InitCluster(cfg, true, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitCluster_HealthCheckClientValidCA(t *testing.T) {
	t.Parallel()
	caPEM := generateTestCACert(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &kubevip.Config{
		EnableBGP: true,
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address:        "https://localhost:6443/livez",
			TimeoutSeconds: 3,
			CAPath:         caFile,
		},
	}

	_, err := cluster.InitCluster(cfg, true, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitCluster_HealthCheckClientInvalidCAPath(t *testing.T) {
	t.Parallel()
	cfg := &kubevip.Config{
		EnableBGP: true,
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address: "https://localhost:6443/livez",
			CAPath:  "/nonexistent/ca.crt",
		},
	}

	_, err := cluster.InitCluster(cfg, true, nil, nil)
	if err == nil {
		t.Fatal("expected error for invalid CA path")
	}
	if !strings.Contains(err.Error(), "reading health check CA cert") {
		t.Errorf("expected error about reading CA cert, got: %v", err)
	}
}

func TestInitCluster_HealthCheckClientInvalidCAContent(t *testing.T) {
	t.Parallel()
	caFile := filepath.Join(t.TempDir(), "bad-ca.crt")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &kubevip.Config{
		EnableBGP: true,
		ControlPlaneHealthCheck: kubevip.HealthCheck{
			Address: "https://localhost:6443/livez",
			CAPath:  caFile,
		},
	}

	_, err := cluster.InitCluster(cfg, true, nil, nil)
	if err == nil {
		t.Fatal("expected error for invalid CA content")
	}
	if !strings.Contains(err.Error(), "contains no valid certificates") {
		t.Errorf("expected error about invalid certificates, got: %v", err)
	}
}

// generateTestCACert creates a self-signed CA certificate in PEM format for testing.
func generateTestCACert(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}
