//go:build linux
// +build linux

// These syscalls are only supported on Linux, so this uses a build directive during compilation. Other OS's will use the arp_unsupported.go and receive an error

package vip

import (
	"bytes"
	"encoding/binary"
	"fmt"
	log "log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	opARPRequest = 1
	opARPReply   = 2
	ethHwLen     = 6
	ipoibHwLen   = 20
)

var (
	ethernetBroadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	// arpRequest is used to flip between garp request or garp reply
	arpRequest   = true
	netClassPath = "/sys/class/net"
)

func htons(p uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], p)
	return *(*uint16)(unsafe.Pointer(&b))
}

func getInterfaceBroadcastAddress(ifaceName string) (net.HardwareAddr, error) {
	data, err := os.ReadFile(netClassPath + "/" + ifaceName + "/broadcast")
	if err != nil {
		return nil, err
	}
	return net.ParseMAC(strings.TrimSpace(string(data)))
}

func getHwType(ifaceName string) (uint16, error) {
	data, err := os.ReadFile(netClassPath + "/" + ifaceName + "/type")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("failed to convert hardware type to integer: %w", err)
	}
	if n < 0 || n > math.MaxUint16 {
		return 0, fmt.Errorf("hardware type %d out of range", n)
	}
	return uint16(n), nil
}

// arpHeader specifies the header for an ARP message.
type arpHeader struct {
	hardwareType          uint16
	protocolType          uint16
	hardwareAddressLength uint8
	protocolAddressLength uint8
	opcode                uint16
}

// arpMessage represents an ARP message.
type arpMessage struct {
	arpHeader
	senderHardwareAddress []byte
	senderProtocolAddress []byte
	targetHardwareAddress []byte
	targetProtocolAddress []byte
}

type arpLinkParams struct {
	hardwareType uint16
	hwLen        uint8
	broadcast    net.HardwareAddr
}

// sockaddrLLExt matches linux sockaddr_ll with room for INFINIBAND_HALEN (20).
// Standard Go syscall.SockaddrLinklayer only has Addr [8]byte.
type sockaddrLLExt struct {
	Family   uint16
	Protocol uint16
	Ifindex  int32
	Hatype   uint16
	Pkttype  uint8
	Halen    uint8
	Addr     [20]byte
}

func rawSendto(fd int, b []byte, sa *sockaddrLLExt) error {
	if len(b) == 0 {
		return syscall.EINVAL
	}

	_, _, errno := syscall.Syscall6(
		syscall.SYS_SENDTO,
		uintptr(fd),
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
		0,
		uintptr(unsafe.Pointer(sa)),
		unsafe.Sizeof(*sa),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func getARPLinkParams(ifaceName string) (arpLinkParams, error) {
	hwType, err := getHwType(ifaceName)
	if err != nil {
		log.Warn("failed to get hardware type, falling back to ethernet", "iface", ifaceName, "err", err)
		hwType = unix.ARPHRD_ETHER
	}

	switch hwType {
	case unix.ARPHRD_INFINIBAND:
		log.Info("using ipoib", "iface", ifaceName)
		bcastAddr, err := getInterfaceBroadcastAddress(ifaceName)
		if err != nil {
			return arpLinkParams{}, fmt.Errorf("failed to get broadcast address: %w", err)
		}
		if len(bcastAddr) != ipoibHwLen {
			return arpLinkParams{}, fmt.Errorf("broadcast address length for ipoib is %d, got %d", ipoibHwLen, len(bcastAddr))
		}
		return arpLinkParams{hwType, ipoibHwLen, bcastAddr}, nil
	case unix.ARPHRD_ETHER:
		log.Info("using ethernet", "iface", ifaceName)
		return arpLinkParams{hwType, ethHwLen, ethernetBroadcast}, nil
	default:
		log.Warn("unexpected hardware type, falling back to ethernet", "iface", ifaceName, "hwType", hwType)
		return arpLinkParams{unix.ARPHRD_ETHER, ethHwLen, ethernetBroadcast}, nil
	}
}

// bytes returns the wire representation of the ARP message.
func (m *arpMessage) bytes() ([]byte, error) {
	buf := new(bytes.Buffer)

	if err := binary.Write(buf, binary.BigEndian, m.arpHeader); err != nil {
		return nil, fmt.Errorf("binary write failed: %v", err)
	}
	buf.Write(m.senderHardwareAddress)
	buf.Write(m.senderProtocolAddress)
	buf.Write(m.targetHardwareAddress)
	buf.Write(m.targetProtocolAddress)

	return buf.Bytes(), nil
}

// gratuitousARP return a gARP request or gARP reply alternatively
// because different devices may support either one of them
func gratuitousARP(ip net.IP, mac net.HardwareAddr, linkParams arpLinkParams) (*arpMessage, error) {
	if ip.To4() == nil {
		return nil, fmt.Errorf("%q is not an IPv4 address", ip)
	}
	if len(mac) != int(linkParams.hwLen) || len(linkParams.broadcast) != int(linkParams.hwLen) {
		return nil, fmt.Errorf("hardware address length %d, got %v and %v", linkParams.hwLen, mac, linkParams.broadcast)
	}

	m := &arpMessage{
		arpHeader: arpHeader{
			linkParams.hardwareType,
			0x0800,           // IPv4
			linkParams.hwLen, // MAC Address Length
			net.IPv4len,      // 32-bit IPv4 Address
			opARPReply,       // ARP Reply
		},
	}

	// https://tools.ietf.org/html/rfc5944#section-4.6
	// In either case, the ARP Sender Hardware Address is
	// set to the link-layer address to which this cache entry should be
	// updated.
	m.senderHardwareAddress = mac

	// When using an ARP Reply packet, the Target Hardware
	// Address is also set to the link-layer address to which this cache
	// entry should be updated (this field is not used in an ARP Request
	// packet).
	m.targetHardwareAddress = mac

	// In either case, the ARP Sender Protocol Address and
	// ARP Target Protocol Address are both set to the IP address of the
	// cache entry to be updated,
	m.senderProtocolAddress = ip.To4()
	m.targetProtocolAddress = ip.To4()

	// send arpRequest and arpReply alternatively
	arpRequest = !arpRequest
	if arpRequest {
		m.arpHeader.opcode = opARPRequest

		// this field is not used in an ARP Request packet
		m.targetHardwareAddress = linkParams.broadcast
	}

	return m, nil
}

func sendARPExtended(fd int, iface *net.Interface, m *arpMessage, linkParams arpLinkParams, b []byte) error {
	if iface.Index < 0 || iface.Index > math.MaxInt32 {
		return fmt.Errorf("interface index %d out of range", iface.Index)
	}
	sa := sockaddrLLExt{
		Family:   syscall.AF_PACKET,
		Protocol: htons(syscall.ETH_P_ARP),
		Ifindex:  int32(iface.Index),
		Hatype:   m.hardwareType,
		Halen:    m.hardwareAddressLength,
	}
	copy(sa.Addr[:], linkParams.broadcast)
	if err := rawSendto(fd, b, &sa); err != nil {
		return fmt.Errorf("failed to send: %v", err)
	}
	return nil
}

func sendARPEthernet(fd int, iface *net.Interface, m *arpMessage, linkParams arpLinkParams, b []byte) error {
	ll := syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_ARP),
		Ifindex:  iface.Index,
		Pkttype:  0, // syscall.PACKET_HOST
		Hatype:   m.hardwareType,
		Halen:    m.hardwareAddressLength,
	}
	target := linkParams.broadcast
	copy(ll.Addr[:], target)

	if err := syscall.Sendto(fd, b, 0, &ll); err != nil {
		return fmt.Errorf("failed to send: %v", err)
	}
	return nil
}

// sendARP sends the given ARP message via the specified interface.
func sendARP(iface *net.Interface, m *arpMessage, linkParams arpLinkParams) error {
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, int(htons(syscall.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("failed to get raw socket: %v", err)
	}
	defer syscall.Close(fd)

	if err := syscall.BindToDevice(fd, iface.Name); err != nil {
		return fmt.Errorf("failed to bind to device: %v", err)
	}

	b, err := m.bytes()
	if err != nil {
		return fmt.Errorf("failed to convert ARP message: %w", err)
	}

	// IPoIB (and any HW len > 8): extended sockaddr + raw sendto.
	if m.hardwareAddressLength > 8 {
		return sendARPExtended(fd, iface, m, linkParams, b)
	}

	return sendARPEthernet(fd, iface, m, linkParams, b)
}

// ARPSendGratuitous sends a gratuitous ARP message via the specified interface.
func ARPSendGratuitous(address, ifaceName string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get interface %q: %v", ifaceName, err)
	}

	ip := net.ParseIP(address)
	if ip == nil {
		return fmt.Errorf("failed to parse address %s", address)
	}

	linkParams, err := getARPLinkParams(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get ARP link params: %w", err)
	}

	// This is a debug message, enable debugging to ensure that the gratuitous arp is repeating
	m, err := gratuitousARP(ip, iface.HardwareAddr, linkParams)
	if err != nil {
		return err
	}
	return sendARP(iface, m, linkParams)
}
