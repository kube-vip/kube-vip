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
	"golang.org/x/sys/unix"
)

const (
	NatTable  = "kube_vip_%s"
	SNatChain = "kube_vip_snat_%s"
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
	slog.Debug("[egress]", "Creating Chain for service", service, "IPv6", IPv6)
	// These don't return errors, so not 100% sure how to guarantee things were created
	conn.AddChain(GetSNatChain(IPv6, service))
	conn.Flush()
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
				switch data[0] {
				case "tcp":
					//nolint:gosec
					tcpElements = append(tcpElements, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(uint16(port))})
				case "udp":
					//nolint:gosec
					udpElements = append(udpElements, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(uint16(port))})
				default:
					slog.Error("[egress]", "unknown protocol", data[0])
				}
			}
		}
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
