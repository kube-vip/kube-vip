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
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
)

const (
	NatTable  = "kube_vip_%s"
	SNatChain = "kube_vip_snat_%s"
)

const (
	// Legacy per-service chain names (kept for compatibility during migration)
	DNATChain   = "kube_vip_prerouting_%s"
	InputChain  = "kube_vip_input_%s"
	MangleChain = "kube_vip_mangle_%s"
)

// Per-tunnel chain name templates (use with fmt.Sprintf and tunnel interface name)
const (
	TunnelDNATChain   = "kvip_dnat_%s"
	TunnelInputChain  = "kvip_input_%s"
	TunnelPostrouting = "kvip_post_%s"
	TunnelManglePre   = "kvip_mpre_%s"
	TunnelMangleOut   = "kvip_mout_%s"
)

// Per-tunnel set name templates
const (
	AcceptPortsSet = "kvip_accept_%s"
	SNATPortsSet   = "kvip_snat_%s"
	MasqIPsSet     = "kvip_masq_ip_%s"
	MasqPortsSet   = "kvip_masq_pt_%s"
)

// Note: We use the WireGuard listen port as the routing table number.
// For the fwmark, we add an offset (0x10000) to avoid collision with WireGuard's own fwmark.
// WireGuard sets fwmark=listenPort on its own UDP packets to the peer, so we must use
// a different mark value for our connmark-based policy routing.
const connmarkOffset = 0x10000 // 65536 - added to listenPort to get our connmark value

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
func ClearTables() error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	tableName := fmt.Sprintf(NatTable, "v6")
	if t, err := FilterTable(conn, tableName, false); err != nil {
		slog.Debug("[egress]", "Cleaning IPv6 finding tables error", err)
	} else if t != nil {
		conn.DelTable(t)
	}

	// These don't return errors, so not 100% sure how to guarantee things were created
	conn.AddTable(GetTable(true))
	tableName = fmt.Sprintf(NatTable, "v4")
	if t, err := FilterTable(conn, tableName, true); err != nil {
		slog.Debug("[egress]", "Cleaning IPv4 finding tables error", err)
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
	// Use priority -105 to run BEFORE kube-proxy's iptables-nft rules (which are at -100).
	// This ensures our DNAT processes WireGuard traffic first, preventing kube-proxy
	// from applying masquerade which would break the response path.
	dnatPriority := nftables.ChainPriority(-105)
	return &nftables.Chain{
		Name:     name,
		Table:    GetTable(IPv6),
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: &dnatPriority,
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

// DNATTarget represents a single target endpoint for DNAT load balancing
type DNATTarget struct {
	IP   string
	Port uint16
}

// tunnelInfraCreated tracks whether infrastructure has been created per tunnel
var tunnelInfraCreated = make(map[string]bool)

// getTunnelChainNames returns the chain names for a specific tunnel
func getTunnelChainNames(wgIf string) (dnat, input, post, mangle, mangleOut string) {
	return fmt.Sprintf(TunnelDNATChain, wgIf),
		fmt.Sprintf(TunnelInputChain, wgIf),
		fmt.Sprintf(TunnelPostrouting, wgIf),
		fmt.Sprintf(TunnelManglePre, wgIf),
		fmt.Sprintf(TunnelMangleOut, wgIf)
}

// getTunnelSetNames returns the set names for a specific tunnel
func getTunnelSetNames(wgIf string) (accept, snat, masqIP, masqPort string) {
	return fmt.Sprintf(AcceptPortsSet, wgIf),
		fmt.Sprintf(SNATPortsSet, wgIf),
		fmt.Sprintf(MasqIPsSet, wgIf),
		fmt.Sprintf(MasqPortsSet, wgIf)
}

// EnsureTunnelInfrastructure creates the chains and sets for a specific tunnel.
// Each tunnel has its own chains and sets, keyed by the interface name.
func EnsureTunnelInfrastructure(wgIf string, vipIP string, IPv6 bool, tunnelListenPort int) error {
	table := GetTable(IPv6)
	key := fmt.Sprintf("%s-%s-%v", table.Name, wgIf, IPv6)

	if tunnelInfraCreated[key] {
		return nil // Already created
	}

	conn, err := nftables.New()
	if err != nil {
		return err
	}

	// Ensure table exists
	if _, err := FilterTable(conn, table.Name, IPv6); err != nil {
		conn.AddTable(table)
	}

	dnatChainName, inputChainName, postChainName, manglePreName, mangleOutName := getTunnelChainNames(wgIf)
	acceptSetName, snatSetName, masqIPSetName, masqPortSetName := getTunnelSetNames(wgIf)

	// Check if chains already exist
	existingChain, _ := conn.ListChain(table, dnatChainName)
	if existingChain != nil {
		tunnelInfraCreated[key] = true
		return nil // Already exists
	}

	// Create chains for this tunnel
	dnatPriority := nftables.ChainPriority(-105)
	manglePriority := nftables.ChainPriority(-150)

	dnatChain := &nftables.Chain{
		Name:     dnatChainName,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: &dnatPriority,
	}

	inputChain := &nftables.Chain{
		Name:     inputChainName,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
	}

	postroutingChain := &nftables.Chain{
		Name:     postChainName,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	}

	manglePreChain := &nftables.Chain{
		Name:     manglePreName,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: &manglePriority,
	}

	mangleOutChain := &nftables.Chain{
		Name:     mangleOutName,
		Table:    table,
		Type:     nftables.ChainTypeRoute,
		Hooknum:  nftables.ChainHookOutput,
		Priority: &manglePriority,
	}

	conn.AddChain(dnatChain)
	conn.AddChain(inputChain)
	conn.AddChain(postroutingChain)
	conn.AddChain(manglePreChain)
	conn.AddChain(mangleOutChain)

	// Create sets for this tunnel
	acceptPortsSet := &nftables.Set{
		Table:   table,
		Name:    acceptSetName,
		KeyType: nftables.TypeInetService,
	}

	snatPortsSet := &nftables.Set{
		Table:   table,
		Name:    snatSetName,
		KeyType: nftables.TypeInetService,
	}

	masqIPsSet := &nftables.Set{
		Table:   table,
		Name:    masqIPSetName,
		KeyType: nftables.TypeIPAddr,
	}
	if IPv6 {
		masqIPsSet.KeyType = nftables.TypeIP6Addr
	}

	masqPortsSet := &nftables.Set{
		Table:   table,
		Name:    masqPortSetName,
		KeyType: nftables.TypeInetService,
	}

	if err := conn.AddSet(acceptPortsSet, nil); err != nil {
		return fmt.Errorf("failed to create accept_ports set: %w", err)
	}
	if err := conn.AddSet(snatPortsSet, nil); err != nil {
		return fmt.Errorf("failed to create snat_ports set: %w", err)
	}
	if err := conn.AddSet(masqIPsSet, nil); err != nil {
		return fmt.Errorf("failed to create masq_ips set: %w", err)
	}
	if err := conn.AddSet(masqPortsSet, nil); err != nil {
		return fmt.Errorf("failed to create masq_ports set: %w", err)
	}

	// Parse VIP for SNAT rule
	vip := net.ParseIP(vipIP)
	if vip == nil {
		return fmt.Errorf("invalid vip: %s", vipIP)
	}

	// Add INPUT accept rule (matches ports in accept_ports set)
	inputRule := &nftables.Rule{
		Table: table,
		Chain: inputChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: 1, SetName: acceptSetName},
			&expr.Verdict{Kind: expr.VerdictAccept},
		},
	}
	conn.AddRule(inputRule)

	// Add SNAT rule (replies going back out tunnel)
	snatRule := &nftables.Rule{
		Table: table,
		Chain: postroutingChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 2},
			&expr.Lookup{SourceRegister: 1, SetName: snatSetName},
			&expr.Immediate{Register: 1, Data: ipToBytes(vip, IPv6)},
			&expr.NAT{Type: expr.NATTypeSourceNAT, Family: ipFamily(IPv6), RegAddrMin: 1},
		},
	}
	conn.AddRule(snatRule)

	// Add masquerade rule (for non-local endpoints)
	ipOffset := uint32(16)
	ipLen := uint32(4)
	if IPv6 {
		ipOffset = 24
		ipLen = 16
	}

	masqRule := &nftables.Rule{
		Table: table,
		Chain: postroutingChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: ipOffset, Len: ipLen},
			&expr.Lookup{SourceRegister: 1, SetName: masqIPSetName},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: 1, SetName: masqPortSetName},
			&expr.Masq{},
		},
	}
	conn.AddRule(masqRule)

	// Add connmark rules
	fwmark := uint32(tunnelListenPort) + connmarkOffset //nolint:gosec

	connmarkInRule := &nftables.Rule{
		Table: table,
		Chain: manglePreChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(fwmark)},
			&expr.Ct{Key: expr.CtKeyMARK, Register: 1, SourceRegister: true},
		},
	}
	conn.AddRule(connmarkInRule)

	connmarkRestoreRule := &nftables.Rule{
		Table: table,
		Chain: manglePreChain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(fwmark)},
			&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
		},
	}
	conn.AddRule(connmarkRestoreRule)

	connmarkOutputRule := &nftables.Rule{
		Table: table,
		Chain: mangleOutChain,
		Exprs: []expr.Any{
			&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(fwmark)},
			&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
		},
	}
	conn.AddRule(connmarkOutputRule)

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("failed to create tunnel infrastructure: %w", err)
	}

	tunnelInfraCreated[key] = true
	slog.Info("created nftables infrastructure for tunnel", "table", table.Name, "interface", wgIf)
	return nil
}

// CleanupTunnelInfrastructure removes all chains and sets for a specific tunnel
func CleanupTunnelInfrastructure(wgIf string, IPv6 bool) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := GetTable(IPv6)

	if _, err := FilterTable(conn, table.Name, IPv6); err != nil {
		return nil // Table doesn't exist
	}

	dnatChainName, inputChainName, postChainName, manglePreName, mangleOutName := getTunnelChainNames(wgIf)
	acceptSetName, snatSetName, masqIPSetName, masqPortSetName := getTunnelSetNames(wgIf)

	// Delete chains
	chains := []string{dnatChainName, inputChainName, postChainName, manglePreName, mangleOutName}
	for _, name := range chains {
		ch, err := conn.ListChain(table, name)
		if err == nil && ch != nil {
			conn.FlushChain(ch)
			conn.DelChain(ch)
		}
	}

	// Delete sets
	sets := []string{acceptSetName, snatSetName, masqIPSetName, masqPortSetName}
	for _, name := range sets {
		set, err := conn.GetSetByName(table, name)
		if err == nil && set != nil {
			conn.DelSet(set)
		}
	}

	// Also delete any per-service DNAT maps for this tunnel
	allSets, _ := conn.GetSets(table)
	for _, set := range allSets {
		if strings.HasPrefix(set.Name, "dnat_") {
			conn.DelSet(set)
		}
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("failed to cleanup tunnel infrastructure: %w", err)
	}

	key := fmt.Sprintf("%s-%s-%v", table.Name, wgIf, IPv6)
	delete(tunnelInfraCreated, key)

	slog.Info("cleaned up nftables infrastructure for tunnel", "table", table.Name, "interface", wgIf)
	return nil
}

// ApplyDNAT creates/updates the DNAT rule and map for a specific port.
// The service parameter is used to identify the rule for later deletion.
func ApplyDNAT(
	wgIf string,
	vipIP string,
	sourcePort uint16,
	targets []DNATTarget,
	service string,
	IPv6 bool,
	protocol v1.Protocol,
	localEndpoint bool,
	tunnelListenPort int,
) error {
	if len(targets) == 0 {
		return fmt.Errorf("no targets provided for DNAT")
	}

	// Ensure tunnel infrastructure exists
	if err := EnsureTunnelInfrastructure(wgIf, vipIP, IPv6, tunnelListenPort); err != nil {
		return fmt.Errorf("failed to ensure tunnel infrastructure: %w", err)
	}

	// Delete existing rule/map for this service-port if it exists
	_ = DeleteDNATRule(wgIf, IPv6, service)

	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := GetTable(IPv6)
	dnatChainName, _, _, _, _ := getTunnelChainNames(wgIf)

	// Get tunnel's DNAT chain
	dnatChain, err := conn.ListChain(table, dnatChainName)
	if err != nil || dnatChain == nil {
		return fmt.Errorf("tunnel DNAT chain not found: %s", dnatChainName)
	}

	// Parse and validate all target IPs
	parsedTargets := make([]struct {
		ip   net.IP
		port uint16
	}, len(targets))
	for i, t := range targets {
		ip := net.ParseIP(t.IP)
		if ip == nil {
			return fmt.Errorf("invalid target ip: %s", t.IP)
		}
		parsedTargets[i].ip = ip
		parsedTargets[i].port = t.Port
	}

	// Determine protocol number
	var protoNum byte
	switch protocol {
	case v1.ProtocolTCP:
		protoNum = unix.IPPROTO_TCP
	case v1.ProtocolUDP:
		protoNum = unix.IPPROTO_UDP
	case v1.ProtocolSCTP:
		protoNum = unix.IPPROTO_SCTP
	default:
		return fmt.Errorf("unsupported protocol: %s", protocol)
	}

	// All targets should have the same port (resolved per service port)
	targetPort := parsedTargets[0].port

	// Create named DNAT map for load balancing (named so it can be updated)
	dnatMapName := fmt.Sprintf("dnat_%s", service)
	ipMapDataType := nftables.TypeIPAddr
	if IPv6 {
		ipMapDataType = nftables.TypeIP6Addr
	}

	dnatMap := &nftables.Set{
		Table:    table,
		Name:     dnatMapName,
		IsMap:    true,
		KeyType:  nftables.TypeInteger,
		DataType: ipMapDataType,
	}

	dnatMapElements := make([]nftables.SetElement, len(parsedTargets))
	for i, t := range parsedTargets {
		dnatMapElements[i] = nftables.SetElement{
			Key: binaryutil.BigEndian.PutUint32(uint32(i)), //nolint:gosec
			Val: ipToBytes(t.ip, IPv6),
		}
	}

	if err := conn.AddSet(dnatMap, dnatMapElements); err != nil {
		return fmt.Errorf("failed to create DNAT map: %w", err)
	}

	// Create DNAT rule for this port
	var dnatExprs []expr.Any
	if len(parsedTargets) > 1 {
		// Multiple targets: use numgen + map for load balancing
		dnatExprs = []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNum}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(sourcePort)},
			&expr.Numgen{Register: 1, Modulus: uint32(len(parsedTargets)), Type: unix.NFT_NG_RANDOM}, //nolint:gosec
			&expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: dnatMapName},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(targetPort)},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: ipFamily(IPv6), RegAddrMin: 1, RegAddrMax: 1, RegProtoMin: 2, RegProtoMax: 2, Specified: true},
		}
	} else {
		// Single target: direct DNAT
		dnatExprs = []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(wgIf), 0)},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNum}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(sourcePort)},
			&expr.Immediate{Register: 1, Data: ipToBytes(parsedTargets[0].ip, IPv6)},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(targetPort)},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: ipFamily(IPv6), RegAddrMin: 1, RegProtoMin: 2},
		}
	}

	dnatRule := &nftables.Rule{
		Table:    table,
		Chain:    dnatChain,
		Exprs:    dnatExprs,
		UserData: []byte(service), // Store service ID for later deletion
	}
	conn.AddRule(dnatRule)

	// Update tunnel sets with elements for this service
	acceptSetName, snatSetName, masqIPSetName, masqPortSetName := getTunnelSetNames(wgIf)

	// Add source port to accept_ports set
	acceptPortsSet, _ := conn.GetSetByName(table, acceptSetName)
	if acceptPortsSet != nil {
		if err := conn.SetAddElements(acceptPortsSet, []nftables.SetElement{
			{Key: binaryutil.BigEndian.PutUint16(sourcePort)},
		}); err != nil {
			return fmt.Errorf("failed to add element to accept_ports set: %w", err)
		}
	}

	// Add target port to snat_ports set
	snatPortsSet, _ := conn.GetSetByName(table, snatSetName)
	if snatPortsSet != nil {
		if err := conn.SetAddElements(snatPortsSet, []nftables.SetElement{
			{Key: binaryutil.BigEndian.PutUint16(targetPort)},
		}); err != nil {
			return fmt.Errorf("failed to add element to snat_ports set: %w", err)
		}
	}

	// For non-local endpoints, add to masquerade sets
	if !localEndpoint {
		masqIPsSet, _ := conn.GetSetByName(table, masqIPSetName)
		masqPortsSet, _ := conn.GetSetByName(table, masqPortSetName)

		if masqIPsSet != nil {
			for _, t := range parsedTargets {
				if err := conn.SetAddElements(masqIPsSet, []nftables.SetElement{
					{Key: ipToBytes(t.ip, IPv6)},
				}); err != nil {
					return fmt.Errorf("failed to add element to masq_ips set: %w", err)
				}
			}
		}

		if masqPortsSet != nil {
			if err := conn.SetAddElements(masqPortsSet, []nftables.SetElement{
				{Key: binaryutil.BigEndian.PutUint16(targetPort)},
			}); err != nil {
				return fmt.Errorf("failed to add element to masq_ports set: %w", err)
			}
		}
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("failed to apply DNAT rule: %w", err)
	}

	return nil
}

// DeleteDNATRule removes the DNAT rule and map for a specific service
func DeleteDNATRule(wgIf string, IPv6 bool, service string) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := GetTable(IPv6)

	// Check if table exists
	if _, err := FilterTable(conn, table.Name, IPv6); err != nil {
		return nil
	}

	// Delete the named DNAT map
	dnatMapName := fmt.Sprintf("dnat_%s", service)
	dnatMap, err := conn.GetSetByName(table, dnatMapName)
	if err == nil && dnatMap != nil {
		conn.DelSet(dnatMap)
	}

	// Find and delete the DNAT rule by matching UserData
	dnatChainName, _, _, _, _ := getTunnelChainNames(wgIf)
	dnatChain, err := conn.ListChain(table, dnatChainName)
	if err == nil && dnatChain != nil {
		rules, err := conn.GetRules(table, dnatChain)
		if err == nil {
			for _, rule := range rules {
				if string(rule.UserData) == service {
					if err := conn.DelRule(rule); err != nil {
						slog.Warn("failed to delete DNAT rule", "service", service, "err", err)
					}
				}
			}
		}
	}

	return conn.Flush()
}

// DeleteIngressChains removes DNAT rules and maps for a service from all tunnels.
// This is a compatibility function - prefer using DeleteDNATRule with explicit wgIf.
func DeleteIngressChains(IPv6 bool, service string) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}

	table := GetTable(IPv6)

	if _, err := FilterTable(conn, table.Name, IPv6); err != nil {
		return nil // Table doesn't exist
	}

	// Delete the named DNAT map for this service
	dnatMapName := fmt.Sprintf("dnat_%s", service)
	dnatMap, err := conn.GetSetByName(table, dnatMapName)
	if err == nil && dnatMap != nil {
		conn.DelSet(dnatMap)
	}

	// Search all chains starting with "kvip_dnat_" for rules with this service's UserData
	chains, err := conn.ListChains()
	if err == nil {
		for _, ch := range chains {
			if ch.Table.Name == table.Name && strings.HasPrefix(ch.Name, "kvip_dnat_") {
				rules, err := conn.GetRules(table, ch)
				if err == nil {
					for _, rule := range rules {
						if string(rule.UserData) == service {
							_ = conn.DelRule(rule)
						}
					}
				}
			}
		}
	}

	// Also try to delete legacy per-service chains (for backwards compatibility)
	legacyChains := []string{
		fmt.Sprintf(DNATChain, service),
		fmt.Sprintf(InputChain, service),
		fmt.Sprintf("kube_vip_postrouting_%s", service),
		fmt.Sprintf(MangleChain, service),
		fmt.Sprintf("kube_vip_mangle_pre_%s", service),
	}

	for _, name := range legacyChains {
		ch, err := conn.ListChain(table, name)
		if err == nil && ch != nil {
			conn.FlushChain(ch)
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

// SetupPolicyRouting sets up policy routing for WireGuard response packets.
// Uses listenPort as the routing table number and listenPort+offset as the fwmark.
func SetupPolicyRouting(wgIf string, listenPort int) error {
	link, err := netlink.LinkByName(wgIf)
	if err != nil {
		return fmt.Errorf("failed to get interface %s: %w", wgIf, err)
	}

	mark := uint32(listenPort) + connmarkOffset //nolint:gosec // Port range validated
	table := listenPort

	rule := netlink.NewRule()
	rule.Mark = mark
	rule.Table = table
	rule.Priority = 100

	if err := netlink.RuleAdd(rule); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("failed to add routing rule: %w", err)
		}
	}

	_, defaultDst, _ := net.ParseCIDR("0.0.0.0/0")
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Table:     table,
		Dst:       defaultDst,
	}

	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("failed to add route to table %d: %w", table, err)
	}

	// Bypass rpfilter for the WireGuard interface.
	// Some systems (like NixOS) have strict rpfilter rules that drop packets
	// arriving on an interface when the source address is routed via a different
	// interface. For WireGuard tunnels, packets from external IPs arrive on the
	// tunnel interface even though those IPs are normally routed via the default
	// gateway interface, causing rpfilter to drop them.
	if err := bypassRpfilterForInterface(wgIf); err != nil {
		slog.Warn("failed to bypass rpfilter for interface", "interface", wgIf, "err", err)
		// Non-fatal - some systems may not have rpfilter configured
	}

	slog.Info("policy routing configured", "interface", wgIf, "mark", mark, "table", table)
	return nil
}

// bypassRpfilterForInterface adds nftables rules to skip rpfilter checks for:
//  1. Packets arriving on the specified WireGuard interface
//  2. Established/related connections (needed because conntrack un-NAT changes
//     the source IP, causing rpfilter to fail for reply packets arriving on
//     other interfaces like flannel for instance)
func bypassRpfilterForInterface(wgIf string) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("failed to create nftables connection: %w", err)
	}

	// Try to find the mangle table and nixos-fw-rpfilter chain (NixOS-specific)
	// If it doesn't exist, try to find any rpfilter chain in the mangle table
	mangleTable := &nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "mangle",
	}

	chains, err := conn.ListChains()
	if err != nil {
		return fmt.Errorf("failed to list chains: %w", err)
	}

	var rpfilterChain *nftables.Chain
	for _, chain := range chains {
		if chain.Table.Name == "mangle" && chain.Table.Family == nftables.TableFamilyIPv4 {
			if chain.Name == "nixos-fw-rpfilter" || strings.Contains(chain.Name, "rpfilter") {
				rpfilterChain = chain
				break
			}
		}
	}

	if rpfilterChain == nil {
		slog.Debug("no rpfilter chain found in mangle table, skipping bypass rule")
		return nil
	}

	// Check if rules already exist by listing rules in the chain
	rules, err := conn.GetRules(mangleTable, rpfilterChain)
	if err != nil {
		return fmt.Errorf("failed to get rules: %w", err)
	}

	hasIfaceRule := false
	hasCtStateRule := false
	for _, rule := range rules {
		for _, e := range rule.Exprs {
			// Check for interface-based rule
			if cmp, ok := e.(*expr.Cmp); ok {
				if string(cmp.Data) == wgIf+"\x00" {
					hasIfaceRule = true
				}
			}
			// Check for ct state rule (look for CtKeySTATE expression)
			if ct, ok := e.(*expr.Ct); ok {
				if ct.Key == expr.CtKeySTATE {
					hasCtStateRule = true
				}
			}
		}
	}

	// Add rule for established/related connections if not present.
	// This is needed because when conntrack applies un-DNAT to reply packets,
	// the source IP changes (e.g., from pod IP to VIP), but the packet arrived
	// on a different interface (e.g., flannel instead of tunnel). rpfilter
	// then fails because the source IP is not reachable via the ingress interface.
	if !hasCtStateRule {
		ctStateRule := &nftables.Rule{
			Table: mangleTable,
			Chain: rpfilterChain,
			Exprs: []expr.Any{
				&expr.Ct{
					Key:      expr.CtKeySTATE,
					Register: 1,
				},
				&expr.Bitwise{
					SourceRegister: 1,
					DestRegister:   1,
					Len:            4,
					Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
					Xor:            binaryutil.NativeEndian.PutUint32(0),
				},
				&expr.Cmp{
					Op:       expr.CmpOpNeq,
					Register: 1,
					Data:     binaryutil.NativeEndian.PutUint32(0),
				},
				&expr.Counter{},
				&expr.Verdict{Kind: expr.VerdictReturn},
			},
		}
		conn.InsertRule(ctStateRule)
		slog.Info("added rpfilter bypass rule for established/related connections", "chain", rpfilterChain.Name)
	}

	// Add rule for the WireGuard interface if not present
	if !hasIfaceRule {
		ifaceRule := &nftables.Rule{
			Table: mangleTable,
			Chain: rpfilterChain,
			Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     append([]byte(wgIf), 0),
				},
				&expr.Counter{},
				&expr.Verdict{Kind: expr.VerdictReturn},
			},
		}
		conn.InsertRule(ifaceRule)
		slog.Info("added rpfilter bypass rule", "interface", wgIf, "chain", rpfilterChain.Name)
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("failed to add rpfilter bypass rules: %w", err)
	}

	return nil
}

// CleanupPolicyRouting removes the policy routing rule for a specific tunnel
func CleanupPolicyRouting(listenPort int) error {
	mark := uint32(listenPort) + connmarkOffset //nolint:gosec // Port range validated
	table := listenPort

	rule := netlink.NewRule()
	rule.Mark = mark
	rule.Table = table
	rule.Priority = 100

	if err := netlink.RuleDel(rule); err != nil {
		if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("failed to delete routing rule: %w", err)
		}
	}
	return nil
}

// CleanupAllChains removes all kube-vip nftables chains and sets from both IPv4 and IPv6 tables.
func CleanupAllChains() error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("failed to create nftables connection: %w", err)
	}

	for _, ipv6 := range []bool{false, true} {
		table := GetTable(ipv6)

		// Delete chains
		chains, err := conn.ListChains()
		if err != nil {
			slog.Warn("failed to list chains", "ipv6", ipv6, "err", err)
			continue
		}

		for _, chain := range chains {
			if chain.Table.Name == table.Name && isKubeVipChain(chain.Name) {
				slog.Info("removing stale nftables chain", "chain", chain.Name, "table", table.Name)
				conn.FlushChain(chain)
				conn.DelChain(chain)
			}
		}

		// Delete sets
		sets, err := conn.GetSets(table)
		if err == nil {
			for _, set := range sets {
				if isKubeVipSet(set.Name) {
					slog.Info("removing stale nftables set", "set", set.Name, "table", table.Name)
					conn.DelSet(set)
				}
			}
		}
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("failed to flush nftables changes: %w", err)
	}

	// Clear the infrastructure tracking
	tunnelInfraCreated = make(map[string]bool)

	return nil
}

func isKubeVipChain(name string) bool {
	return strings.HasPrefix(name, "kube_vip") || strings.HasPrefix(name, "kvip_")
}

func isKubeVipSet(name string) bool {
	return strings.HasPrefix(name, "kvip_") || strings.HasPrefix(name, "dnat_")
}
