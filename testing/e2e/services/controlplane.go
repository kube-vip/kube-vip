package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"html/template"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

func getKindNetworkSubnetCIDRs() ([]string, error) {
	cmd := exec.Command(
		"docker", "inspect", "kind",
		"--format", `{{ range $i, $a := .IPAM.Config }}{{ println .Subnet }}{{ end }}`,
	)
	cmdOut := new(bytes.Buffer)
	cmd.Stdout = cmdOut
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(cmdOut)

	cidrs := []string{}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("error finding subnet CIDRs in the Docker \"kind\" network, %s", err)
		}

		cidrs = append(cidrs, strings.TrimSpace(line))
		if readErr == io.EOF {
			break
		}
	}

	return cidrs, nil
}

func generateIPv4VIP() (string, error) {
	cidrs, err := getKindNetworkSubnetCIDRs()
	if err != nil {
		return "", err
	}
	for _, cidr := range cidrs {
		ip, ipNet, parseErr := net.ParseCIDR(cidr)
		if parseErr != nil {
			return "", parseErr
		}
		if ip.To4() != nil {
			mask := binary.BigEndian.Uint32(ipNet.Mask)
			start := binary.BigEndian.Uint32(ipNet.IP)
			end := (start & mask) | (^mask)

			chosenVIP := make([]byte, 4)
			binary.BigEndian.PutUint32(chosenVIP, end-5)
			return net.IP(chosenVIP).String(), nil
		}
	}
	return "", fmt.Errorf("could not find any IPv4 CIDRs in the Docker \"kind\" network")
}

func generateIPv6VIP() (string, error) {
	cidrs, err := getKindNetworkSubnetCIDRs()
	if err != nil {
		return "", err
	}
	for _, cidr := range cidrs {
		ip, ipNet, parseErr := net.ParseCIDR(cidr)
		if parseErr != nil {
			return "", parseErr
		}
		if ip.To4() == nil {
			lowerMask := binary.BigEndian.Uint64(ipNet.Mask[8:])
			lowerStart := binary.BigEndian.Uint64(ipNet.IP[8:])
			lowerEnd := (lowerStart & lowerMask) | (^lowerMask)

			chosenVIP := make([]byte, 16)
			// Copy upper half into chosenVIP
			copy(chosenVIP, ipNet.IP[0:8])
			// Copy lower half into chosenVIP
			binary.BigEndian.PutUint64(chosenVIP[8:], lowerEnd-5)
			return net.IP(chosenVIP).String(), nil
		}
	}
	return "", fmt.Errorf("could not find any IPv6 CIDRs in the Docker \"kind\" network")

}

func (config *testConfig) manifestGen() error {
	curDir, err := os.Getwd()
	if err != nil {
		return err
	}
	templatePath := filepath.Join(curDir, "testing/e2e/kube-vip.yaml.tmpl")

	kubeVIPManifestTemplate, err := template.New("kube-vip.yaml.tmpl").ParseFiles(templatePath)
	if err != nil {
		return err
	}
	tempDirPath, err := os.MkdirTemp("", "kube-vip-test")
	if err != nil {
		return err
	}

	var manifestFile *os.File

	if config.IPv6 {
		config.Name = fmt.Sprintf("%s-ipv6", filepath.Base(tempDirPath))
		config.ManifestPath = filepath.Join(tempDirPath, "kube-vip-ipv6.yaml")
		manifestFile, err = os.Create(config.ManifestPath)
		if err != nil {
			return err
		}
		defer manifestFile.Close()

		config.ControlPlaneAddress, err = generateIPv6VIP()
		if err != nil {
			return err
		}
	} else {
		config.Name = fmt.Sprintf("%s-ipv4", filepath.Base(tempDirPath))
		config.ManifestPath = filepath.Join(tempDirPath, "kube-vip-ipv4.yaml")
		manifestFile, err = os.Create(config.ManifestPath)
		if err != nil {
			return err
		}
		defer manifestFile.Close()

		config.ControlPlaneAddress, err = generateIPv4VIP()
		if err != nil {
			return err
		}
	}
	log.Infof("🗃️ Manifest path %s", config.ManifestPath)
	err = kubeVIPManifestTemplate.Execute(manifestFile, kubevipManifestValues{
		ControlPlaneVIP: config.ControlPlaneAddress,
		ImagePath:       config.ImagePath,
	})
	return err
}

// func (config *testConfig) startTest(ctx context.Context, clientset *kubernetes.Clientset) error {
// 	if config.ControlPlaneAddress == "" {
// 		log.Fatal("no control plane address exists")
// 	}

// 	return nil
// }
