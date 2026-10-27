package e2e

import (
	"net/netip"
	"regexp"
)

// nftExcludeRE matches "ip daddr != A.B.C.D/N" and "ip daddr != A.B.C.D-E.F.G.H".
// kube-vip programs CIDR exclusions as range expressions, which nft lists as
// address ranges rather than prefixes.
var nftExcludeRE = regexp.MustCompile(`daddr != ([0-9.]+)(?:/([0-9]+)|-([0-9.]+))`)

// NftExcludesCIDR reports whether chain holds a destination exclusion that
// covers the whole of cidr.
func NftExcludesCIDR(chain, cidr string) bool {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	prefix = prefix.Masked()
	first, last := prefix.Addr(), lastAddr(prefix)
	for _, match := range nftExcludeRE.FindAllStringSubmatch(chain, -1) {
		low, err := netip.ParseAddr(match[1])
		if err != nil {
			continue
		}
		var high netip.Addr
		switch {
		case match[2] != "":
			excluded, err := netip.ParsePrefix(match[1] + "/" + match[2])
			if err != nil {
				continue
			}
			low, high = excluded.Masked().Addr(), lastAddr(excluded.Masked())
		default:
			if high, err = netip.ParseAddr(match[3]); err != nil {
				continue
			}
		}
		if low.Compare(first) <= 0 && high.Compare(last) >= 0 {
			return true
		}
	}
	return false
}

func lastAddr(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Addr().AsSlice()
	for bit := prefix.Bits(); bit < len(bytes)*8; bit++ {
		bytes[bit/8] |= 1 << (7 - bit%8)
	}
	addr, _ := netip.AddrFromSlice(bytes)
	return addr
}
