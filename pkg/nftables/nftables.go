package nftables

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"golang.org/x/sys/unix"
)

const (
	NatTable  = "kube_vip_%s"
	SNatChain = "kube_vip_snat_%s"
)

const (
	DNATChain  = "kube_vip_prerouting_%s"
	InputChain = "kube_vip_input_%s"
)

func ApplySNAT(podIP, vipIP, service, destinationPorts string, ignoreCIDR []string, IPv6 bool) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	var tableName string
	if IPv6 {
		tableName = fmt.Sprintf(NatTable, "v6")
	} else {
		tableName = fmt.Sprintf(NatTable, "v4")
	}
	// Look up the table
	if t, err := FilterTable(conn, tableName, IPv6); err != nil {
		if t == nil {
			// If it doesn't exist then create it
			slog.Debug("[egress]", "Creating Table", tableName)
			conn.AddTable(GetTable(IPv6))
		}
	}
	slog.Debug("[egress]", "Creating Chain for service", service, utils.IPv6Family, IPv6)
	conn.AddChain(GetSNatChain(IPv6, service))
	err = conn.Flush()
	if err != nil {
		return err
	}
	// Create our nftables rule
	rule, err := CreateRule(podIP, vipIP, service, destinationPorts, ignoreCIDR, conn, IPv6)
	if err != nil {
		return err
	}
	slog.Debug("[egress]", "table", rule.Table.Name, "chain", rule.Chain.Name, "expr", rule.Exprs)
	conn.AddRule(rule) // Add the rule

	err = conn.Flush() // Commit the rule to nftables
	if err != nil {
		return err
	}

	return conn.CloseLasting() // Close out any remaining netlink communication
}

func DeleteSNAT(IPv6 bool, service string) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	var chainName = fmt.Sprintf(SNatChain, service)
	slog.Info("[egress]", "Looking for", chainName)

	chain, err := conn.ListChain(GetTable(IPv6), chainName)
	if err != nil {
		return err
	}
	if chain != nil {
		slog.Info("[egress]", "Deleting chain", chainName)
		conn.DelChain(chain)
		return conn.Flush()

	}

	return fmt.Errorf("unable to find chain [%s]", chainName)
}

func GetTable(IPv6 bool) *nftables.Table {
	var tableName string
	if IPv6 {
		tableName = fmt.Sprintf(NatTable, "v6")
	} else {
		tableName = fmt.Sprintf(NatTable, "v4")
	}
	// Default to IPv4
	table := &nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   tableName,
	}

	// Move to IPv6 if needed
	if IPv6 {
		table.Family = nftables.TableFamilyIPv6
	}
	return table
}

func GetSNatChain(IPv6 bool, service string) *nftables.Chain {
	var chainName = fmt.Sprintf(SNatChain, service)
	policy := nftables.ChainPolicyAccept
	return &nftables.Chain{
		Name:     chainName,
		Table:    GetTable(IPv6),
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   &policy,
	}
}

func FilterTable(conn *nftables.Conn, tableName string, IPv6 bool) (*nftables.Table, error) {
	if IPv6 {
		return conn.ListTableOfFamily(tableName, nftables.TableFamilyIPv6)
	}
	return conn.ListTableOfFamily(tableName, nftables.TableFamilyIPv4)
}

// ClearTable will remove the original tables and create new empty ones
func ClearTable(conn *nftables.Conn) error {
	tableName := fmt.Sprintf(NatTable, "v6")
	if t, err := FilterTable(conn, tableName, false); err != nil {
		return err
	} else if t != nil {
		conn.DelTable(t)
	}

	// These don't return errors, so not 100% sure how to guarantee things were created
	conn.AddTable(GetTable(true))
	tableName = fmt.Sprintf(NatTable, "v4")
	if t, err := FilterTable(conn, tableName, true); err != nil {
		return err
	} else if t != nil {
		conn.DelTable(t)
	}

	// These don't return errors, so not 100% sure how to guarantee things were created
	conn.AddTable(GetTable(false))
	return nil
}

// Create our nftables rule
func CreateRule(podIP, vipIP, service, destinationPorts string, ignoreCIDR []string, conn *nftables.Conn, IPv6 bool) (*nftables.Rule, error) {

	// Validate pod IP
	if net.ParseIP(podIP) == nil {
		return nil, errors.New("ip is invalid")
	}

	// Validate vip IP
	if net.ParseIP(vipIP) == nil {
		return nil, errors.New("output_ip is not a valid ip")
	}

	// Get the kube-vip table
	table := GetTable(IPv6)

	// Create our rule
	rule := &nftables.Rule{
		Table: table,
		Exprs: []expr.Any{},
	}
	// Set the correct chain
	rule.Chain = GetSNatChain(IPv6, service)

	// Create a set for our original/source address
	set := &nftables.Set{
		Table:     table,
		Anonymous: true,
		Constant:  true,
		KeyType:   nftables.TypeIPAddr,
		Interval:  false,
	}
	if IPv6 {
		set.KeyType = nftables.TypeIP6Addr
	} else {
		set.KeyType = nftables.TypeIPAddr
	}

	// Create an element using our pod IP
	elements := []nftables.SetElement{}
	if IPv6 {
		elements = append(elements, nftables.SetElement{Key: net.ParseIP(podIP).To16()})
	} else {
		elements = append(elements, nftables.SetElement{Key: net.ParseIP(podIP).To4()})
	}

	// Add the elements to the set
	err := conn.AddSet(set, elements)
	if err != nil {
		return nil, err
	}

	// Create the expression using the set
	expression := []expr.Any{}

	payload := &expr.Payload{
		OperationType:  expr.PayloadLoad,
		Base:           expr.PayloadBaseNetworkHeader,
		DestRegister:   1,
		SourceRegister: 0,
	}

	// Set the length of the data based upon the type of IP version being used
	if IPv6 {
		payload.Offset = 8
		payload.Len = 16
	} else {
		payload.Offset = 12
		payload.Len = 4
	}
	lookup := &expr.Lookup{
		SourceRegister: 1,
		DestRegister:   0,
		SetID:          set.ID,
	}

	// Add expressions
	expression = append(expression, payload)
	expression = append(expression, lookup)

	// Add expression to the rule
	rule.Exprs = append(rule.Exprs, expression...)

	// If we filter on ports protocols then parse them
	if destinationPorts != "" {
		fixedPorts := strings.Split(destinationPorts, ",")

		// Create an element using our pod IP
		tcpElements := []nftables.SetElement{}
		udpElements := []nftables.SetElement{}
		sctpElements := []nftables.SetElement{}

		tcpSet := &nftables.Set{
			Anonymous: true,
			Constant:  true,
			Table:     table,
			KeyType:   nftables.TypeInetService,
		}
		udpSet := &nftables.Set{
			Anonymous: true,
			Constant:  true,
			Table:     table,
			KeyType:   nftables.TypeInetService,
		}
		sctpSet := &nftables.Set{
			Anonymous: true,
			Constant:  true,
			Table:     table,
			KeyType:   nftables.TypeInetService,
		}
		for _, fixedPort := range fixedPorts {
			data := strings.Split(fixedPort, ":")
			if len(data) == 0 {
				continue
			} else if len(data) == 2 { // Ensure we have two elements { proto:port }
				// parse the port to a number
				port, err := strconv.Atoi(data[1])
				if err != nil {
					slog.Error("[egress]", "unable to process port", data[1])
					continue
				}
				// Ensure the port is within the valid range for uint16
				if port < 0 || port > 65535 {
					slog.Error("[egress]", "port out of range for uint16", data[1])
					continue
				}

				switch data[0] {
				case "tcp":
					//nolint:gosec
					tcpElements = append(tcpElements, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(uint16(port))})
				case "udp":
					//nolint:gosec
					udpElements = append(udpElements, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(uint16(port))})
				case "sctp":
					//nolint:gosec
					sctpElements = append(sctpElements, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(uint16(port))})
				default:
					slog.Error("[egress]", "unknown protocol", data[0])
				}
			}
		}

		// Add TCP Ports
		if len(tcpElements) != 0 {
			err = conn.AddSet(tcpSet, tcpElements)
			if err != nil {
				return nil, err
			}
			expression := []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				// [ cmp eq reg 1 0x00000006 ]
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte{unix.IPPROTO_TCP},
				},

				// [ payload load 2b @ transport header + 2 => reg 1 ]
				&expr.Payload{
					DestRegister: 1,
					Base:         expr.PayloadBaseTransportHeader,
					Offset:       2,
					Len:          2,
				},
				// [ lookup reg 1 set __set%d ]
				&expr.Lookup{
					SourceRegister: 1,
					SetName:        tcpSet.Name,
					SetID:          tcpSet.ID,
				},
			}
			rule.Exprs = append(rule.Exprs, expression...)
		}

		// Add UDP ports
		if len(udpElements) != 0 {
			err = conn.AddSet(udpSet, udpElements)
			if err != nil {
				return nil, err
			}
			expression := []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				// [ cmp eq reg 1 0x00000006 ]
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte{unix.IPPROTO_UDP},
				},

				// [ payload load 2b @ transport header + 2 => reg 1 ]
				&expr.Payload{
					DestRegister: 1,
					Base:         expr.PayloadBaseTransportHeader,
					Offset:       2,
					Len:          2,
				},
				// [ lookup reg 1 set __set%d ]
				&expr.Lookup{
					SourceRegister: 1,
					SetName:        udpSet.Name,
					SetID:          udpSet.ID,
				},
			}
			rule.Exprs = append(rule.Exprs, expression...)
		}

		// Add SCTP Ports
		if len(sctpElements) != 0 {
			err = conn.AddSet(sctpSet, sctpElements)
			if err != nil {
				return nil, err
			}
			expression := []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				// [ cmp eq reg 1 0x00000006 ]
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte{unix.IPPROTO_SCTP},
				},

				// [ payload load 2b @ transport header + 2 => reg 1 ]
				&expr.Payload{
					DestRegister: 1,
					Base:         expr.PayloadBaseTransportHeader,
					Offset:       2,
					Len:          2,
				},
				// [ lookup reg 1 set __set%d ]
				&expr.Lookup{
					SourceRegister: 1,
					SetName:        sctpSet.Name,
					SetID:          sctpSet.ID,
				},
			}
			rule.Exprs = append(rule.Exprs, expression...)
		}
	}

	// Parse which CIDRs we will not SNAT for
	for _, cidr := range ignoreCIDR {
		start, end, err := nftables.NetFirstAndLastIP(cidr)
		if err != nil {
			return nil, err
		}
		expression = []expr.Any{}

		payload := &expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
		}
		notEqualRange := &expr.Range{
			Op:       expr.CmpOpNeq,
			Register: 1,
		}

		if IPv6 {
			payload.Len = 16
			payload.Offset = 24
			notEqualRange.FromData = start.To16()
			notEqualRange.ToData = end.To16()
		} else {
			payload.Offset = 16
			payload.Len = 4
			notEqualRange.FromData = start.To4()
			notEqualRange.ToData = end.To4()
		}
		// Add expressions
		expression = append(expression, payload)
		expression = append(expression, notEqualRange)

		// // Add expression to the rule
		rule.Exprs = append(rule.Exprs, expression...)
	}

	// Final expression to the rule is the SNAT to the VIP address
	expression = []expr.Any{}

	immediate := &expr.Immediate{
		Register: 1,
	}

	nat := &expr.NAT{
		Type:        expr.NATTypeSourceNAT,
		RegAddrMin:  1,
		RegAddrMax:  1,
		RegProtoMin: 0,
		RegProtoMax: 0,
		Random:      false,
		FullyRandom: false,
		Persistent:  false,
		Prefix:      false,
	}

	if IPv6 {
		immediate.Data = net.ParseIP(vipIP).To16()
		nat.Family = unix.NFPROTO_IPV6
	} else {
		immediate.Data = net.ParseIP(vipIP).To4()
		nat.Family = unix.NFPROTO_IPV4
	}
	// https://github.com/google/nftables/blob/main/nftables_test.go#L5375
	// Add expressions
	expression = append(expression, immediate)
	expression = append(expression, nat)
	rule.Exprs = append(rule.Exprs, expression...)

	return rule, nil
}

// Returns a list of all chains IPv4/IPv6 in nftables
func ListChains() ([]string, error) {
	chains := []string{}
	conn, err := nftables.New()
	if err != nil {
		return nil, err
	}
	ipv4, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, err
	}
	ipv6, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv6)
	if err != nil {
		return nil, err
	}
	for x := range ipv4 {
		chains = append(chains, fmt.Sprintf("Table=%s, Chain=%s", ipv4[x].Table.Name, ipv4[x].Name))
	}
	for x := range ipv6 {
		chains = append(chains, fmt.Sprintf("Table=%s, Chain=%s", ipv6[x].Table.Name, ipv6[x].Name))
	}

	_ = conn.CloseLasting() // TODO: Should we ignore this error, we're not actually doing any actions with nftables
	return chains, nil
}

func GetDNATChain(IPv6 bool, service string) *nftables.Chain {
	name := fmt.Sprintf(DNATChain, service)
	return &nftables.Chain{
		Name:     name,
		Table:    GetTable(IPv6),
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	}
}

func GetInputChain(IPv6 bool, service string) *nftables.Chain {
	name := fmt.Sprintf(InputChain, service)
	policy := nftables.ChainPolicyAccept
	return &nftables.Chain{
		Name:     name,
		Table:    GetTable(IPv6),
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policy,
	}
}

func ApplyAPIServerDNAT(
	wgIf string,
	vipIP string,
	targetIP string,
	sourcePort uint16,
	targetPort uint16,
	service string,
	IPv6 bool,
) error {

	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := GetTable(IPv6)

	if _, err := FilterTable(conn, table.Name, IPv6); err != nil {
		conn.AddTable(table)
	}

	dnatChain := GetDNATChain(IPv6, service)
	inputChain := GetInputChain(IPv6, service)

	// Create POSTROUTING chain for SNAT/masquerade
	postroutingChain := &nftables.Chain{
		Name:     fmt.Sprintf("kube_vip_postrouting_%s", service),
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	}

	conn.AddChain(dnatChain)
	conn.AddChain(inputChain)
	conn.AddChain(postroutingChain)

	if err := conn.Flush(); err != nil {
		return err
	}

	vip := net.ParseIP(vipIP)
	target := net.ParseIP(targetIP)
	if vip == nil || target == nil {
		return fmt.Errorf("invalid vip or target ip")
	}

	/* ---------------- DNAT RULE ---------------- */

	dnatRule := &nftables.Rule{
		Table: table,
		Chain: dnatChain,
		Exprs: []expr.Any{

			// iifname == wgIf
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     append([]byte(wgIf), 0),
			},

			// tcp
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte{unix.IPPROTO_TCP},
			},

			// dport == sourcePort (incoming port, e.g., 6443)
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       2,
				Len:          2,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     binaryutil.BigEndian.PutUint16(sourcePort),
			},

			// dnat to targetIP:targetPort (e.g., 10.43.0.1:443)
			&expr.Immediate{
				Register: 1,
				Data:     ipToBytes(target, IPv6),
			},
			&expr.Immediate{
				Register: 2,
				Data:     binaryutil.BigEndian.PutUint16(targetPort),
			},
			&expr.NAT{
				Type:        expr.NATTypeDestNAT,
				Family:      ipFamily(IPv6),
				RegAddrMin:  1,
				RegProtoMin: 2,
			},
		},
	}

	conn.AddRule(dnatRule)

	/* ---------------- INPUT ACCEPT RULE ---------------- */

	inputRule := &nftables.Rule{
		Table: table,
		Chain: inputChain,
		Exprs: []expr.Any{

			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     append([]byte(wgIf), 0),
			},

			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte{unix.IPPROTO_TCP},
			},

			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       2,
				Len:          2,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     binaryutil.BigEndian.PutUint16(sourcePort),
			},

			&expr.Verdict{Kind: expr.VerdictAccept},
		},
	}

	conn.AddRule(inputRule)

	/* ---------------- SNAT RULE FOR REPLIES ON WG0 ---------------- */

	// SNAT replies going back out wg0 to appear from the VIP
	snatRule := &nftables.Rule{
		Table: table,
		Chain: postroutingChain,
		Exprs: []expr.Any{
			// oifname == wgIf
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     append([]byte(wgIf), 0),
			},

			// tcp sport == target port
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte{unix.IPPROTO_TCP},
			},

			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       0, // source port
				Len:          2,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     binaryutil.BigEndian.PutUint16(targetPort),
			},

			// snat to VIP
			&expr.Immediate{
				Register: 1,
				Data:     ipToBytes(vip, IPv6),
			},
			&expr.NAT{
				Type:       expr.NATTypeSourceNAT,
				Family:     ipFamily(IPv6),
				RegAddrMin: 1,
			},
		},
	}

	conn.AddRule(snatRule)

	/* ---------------- MASQUERADE RULE FOR TRAFFIC TO K8S SERVICE ---------------- */

	// Masquerade traffic going to the Kubernetes API service so replies come back to us
	masqueradeRule := &nftables.Rule{
		Table: table,
		Chain: postroutingChain,
		Exprs: []expr.Any{
			// oifname != wgIf (going out eth0 or other interface)
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 1,
				Data:     append([]byte(wgIf), 0),
			},

			// ip daddr == targetIP
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       16, // destination IP in IPv4 header
				Len:          4,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ipToBytes(target, IPv6),
			},

			// tcp dport == target port
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte{unix.IPPROTO_TCP},
			},

			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       2, // destination port
				Len:          2,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     binaryutil.BigEndian.PutUint16(targetPort),
			},

			// masquerade
			&expr.Masq{},
		},
	}

	conn.AddRule(masqueradeRule)

	if err := conn.Flush(); err != nil {
		return err
	}

	return conn.CloseLasting()
}

func DeleteIngressChains(IPv6 bool, service string) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := GetTable(IPv6)

	chains := []string{
		fmt.Sprintf(DNATChain, service),
		fmt.Sprintf(InputChain, service),
		fmt.Sprintf("kube_vip_postrouting_%s", service),
	}

	for _, name := range chains {
		ch, err := conn.ListChain(table, name)
		if err == nil && ch != nil {
			slog.Info("[ingress]", "Deleting chain", name)
			conn.DelChain(ch)
		}
	}

	return conn.Flush()
}

func ipToBytes(ip net.IP, IPv6 bool) []byte {
	if IPv6 {
		return ip.To16()
	}
	return ip.To4()
}

func ipFamily(IPv6 bool) uint32 {
	if IPv6 {
		return unix.NFPROTO_IPV6
	}
	return unix.NFPROTO_IPV4
}
