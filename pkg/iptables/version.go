package iptables

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type Version struct {
	Major       int
	Minor       int
	Patch       int
	BackendMode string
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		return v.Major - other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor - other.Minor
	}
	return v.Patch - other.Patch
}

func ParseVersion(versionString string) (Version, error) {
	re := regexp.MustCompile(`v([0-9]+)\.([0-9]+)\.([0-9]+)`)
	match := re.FindStringSubmatch(versionString)
	if len(match) != 4 {
		return Version{}, fmt.Errorf("invalid version string: %s", versionString)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}

func GetVersion() (Version, error) {
	ver := Version{}
	cmd := exec.Command("iptables", "--version")
	out, err := cmd.Output()
	if err != nil {
		return ver, fmt.Errorf("run cmd 'iptables --version' wtith error: %v", err)
	}

	ver, err = ParseVersion(string(out))
	if err != nil {
		return ver, err
	}

	nft4 := getOutput("iptables-nft-save")
	legacy4 := getOutput("iptables-legacy-save")

	nft6 := getOutput("ip6tables-nft-save")
	legacy6 := getOutput("ip6tables-legacy-save")

	if strings.Contains(nft4, "KUBE-IPTABLES") ||
		strings.Contains(nft6, "KUBE-IPTABLES") ||
		strings.Contains(nft4, "KUBE-KUBELET") ||
		strings.Contains(nft6, "KUBE-KUBELET") {
		ver.BackendMode = "nft"
	} else if strings.Contains(legacy4, "KUBE-IPTABLES") ||
		strings.Contains(legacy6, "KUBE-IPTABLES") ||
		strings.Contains(legacy4, "KUBE-KUBELET") ||
		strings.Contains(legacy6, "KUBE-KUBELET") {
		ver.BackendMode = "legacy"
	} else {
		nftCount := strings.Count(nft4, "\n") + strings.Count(nft6, "\n")
		legacyCount := strings.Count(legacy4, "\n") + strings.Count(legacy6, "\n")

		if nftCount >= legacyCount {
			ver.BackendMode = "nft"
		} else {
			ver.BackendMode = "legacy"
		}
	}

	return ver, nil
}

func getOutput(name string) string {
	cmd := exec.Command(name)
	out, _ := cmd.Output()
	return string(out)
}
