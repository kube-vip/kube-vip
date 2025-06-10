//go:build e2e
// +build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/vip"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
)

const (
	IPv4Family = "IPv4"
	IPv6Family = "IPv6"
)

func EnsureKindNetwork() {
	By("checking if the Docker \"kind\" network exists")
	cmd := exec.Command("docker", "inspect", "kind")
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session).Should(gexec.Exit())
	if session.ExitCode() == 0 {
		return
	}

	By("Docker \"kind\" network was not found. Creating dummy Kind cluster to ensure creation")
	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking: kindconfigv1alpha4.Networking{
			IPFamily: kindconfigv1alpha4.IPv6Family,
		},
	}

	provider := cluster.NewProvider(
		cluster.ProviderWithDocker(),
	)
	dummyClusterName := fmt.Sprintf("dummy-cluster-%d", time.Now().Unix())
	Expect(provider.Create(
		dummyClusterName,
		cluster.CreateWithV1Alpha4Config(&clusterConfig),
	)).To(Succeed())

	By("deleting dummy Kind cluster")
	Expect(provider.Delete(dummyClusterName, ""))

	By("checking if the Docker \"kind\" network was successfully created")
	cmd = exec.Command("docker", "inspect", "kind")
	session, err = gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session).Should(gexec.Exit(0))
}

func GenerateVIP(family string, offset uint) string {
	cidrs := getKindNetworkSubnetCIDRs()

	for _, cidr := range cidrs {
		ip, ipNet, parseErr := net.ParseCIDR(cidr)
		Expect(parseErr).NotTo(HaveOccurred())

		if ip.To4() == nil && family == IPv6Family {
			lowerMask := binary.BigEndian.Uint64(ipNet.Mask[8:])
			lowerStart := binary.BigEndian.Uint64(ipNet.IP[8:])
			lowerEnd := (lowerStart & lowerMask) | (^lowerMask)

			chosenVIP := make([]byte, 16)
			// Copy upper half into chosenVIP
			copy(chosenVIP, ipNet.IP[0:8])
			// Copy lower half into chosenVIP
			binary.BigEndian.PutUint64(chosenVIP[8:], lowerEnd-uint64(offset))
			return net.IP(chosenVIP).String()
		} else if ip.To4() != nil && family == IPv4Family {
			mask := binary.BigEndian.Uint32(ipNet.Mask)
			start := binary.BigEndian.Uint32(ipNet.IP)
			end := (start & mask) | (^mask)

			chosenVIP := make([]byte, 4)
			binary.BigEndian.PutUint32(chosenVIP, end-uint32(offset))
			return net.IP(chosenVIP).String()
		}
	}

	Fail("Could not find any " + family + " CIDRs in the Docker \"kind\" network")
	return ""
}

func GenerateDualStackVIP(offset uint) string {
	return GenerateVIP(IPv4Family, offset) + "," + GenerateVIP(IPv6Family, offset)
}

func getKindNetworkSubnetCIDRs() []string {
	cmd := exec.Command(
		"docker", "inspect", "kind",
		"--format", `{{ range $i, $a := .IPAM.Config }}{{ println .Subnet }}{{ end }}`,
	)
	cmdOut := new(bytes.Buffer)
	cmd.Stdout = cmdOut
	Expect(cmd.Run()).To(Succeed(), "The Docker \"kind\" network was not found.")
	reader := bufio.NewReader(cmdOut)

	cidrs := []string{}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			Expect(readErr).NotTo(HaveOccurred(), "Error finding subnet CIDRs in the Docker \"kind\" network")
		}

		cidrs = append(cidrs, strings.TrimSpace(line))
		if readErr == io.EOF {
			break
		}
	}

	return cidrs
}

func CheckIPAddressPresence(ip string, container string, expected bool) bool {
	family := "-4"
	if vip.IsIPv6(ip) {
		family = "-6"
	}

	cmd := exec.Command(
		"docker", "exec", container, "ip", family, "addr", "show", "dev", "eth0",
	)

	cmdOut := new(bytes.Buffer)
	cmdErr := new(bytes.Buffer)
	cmd.Stdout = cmdOut
	cmd.Stderr = cmdErr
	if err := cmd.Run(); err != nil {
		return false
	}

	return strings.Contains(cmdOut.String(), ip) == expected
}

func CheckIPAddressPresenceByLease(name, namespace, ip string, client kubernetes.Interface, expected bool) bool {
	container := GetLeaseHolder(name, namespace, client)
	if container == "" {
		return false
	}
	return CheckIPAddressPresence(ip, container, expected)
}

func CheckRoutePresence(ip string, container string, expected bool) bool {
	family := "-4"
	if vip.IsIPv6(ip) {
		family = "-6"
	}

	result := false
	Eventually(func() bool {
		cmdOut := new(bytes.Buffer)
		cmdErr := new(bytes.Buffer)

		cmd := exec.Command(
			"docker", "exec", container, "ip", family, "route", "show", "table", "198",
		)

		cmd.Stdout = cmdOut
		cmd.Stderr = cmdErr
		cmd.Run()

		result = strings.Contains(cmdOut.String(), ip) == expected

		return result
	}, "60s", "1s").Should(BeTrue())

	return result
}

func GetLeaseHolder(name, namespace string, client kubernetes.Interface) string {
	var lease *coordinationv1.Lease
	Eventually(func() error {
		var err error
		lease, err = client.CoordinationV1().Leases(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		return err
	}, "300s").ShouldNot(HaveOccurred())

	Expect(lease).ToNot(BeNil())
	return *lease.Spec.HolderIdentity
}

type SecureOffset struct {
	value int
	mu    sync.Mutex
}

func NewOffset(value int) *SecureOffset {
	return &SecureOffset{
		value: value,
	}
}

func (so *SecureOffset) Get() uint {
	so.mu.Lock()
	defer so.mu.Unlock()
	so.value++
	return uint(so.value - 1)
}
