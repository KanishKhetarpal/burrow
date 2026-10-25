// Package wol builds and sends Wake-on-LAN magic packets.
//
// Burrow v1 wakes the PC with a smart plug, so nothing here runs in production.
// It is built and tested now so a Raspberry Pi waker can be dropped in later by
// switching wake.method to "waker" with no code change (docs/02-ARCHITECTURE.md
// §2.2, D7).
package wol

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// PacketLen is the fixed size of a magic packet: 6 sync bytes plus the MAC
// repeated 16 times.
const PacketLen = 6 + 16*6

// Ports are the two UDP ports WoL implementations listen on. Both are used
// because motherboards differ in which one they honour.
var Ports = []int{9, 7}

// Repeats and RepeatDelay spread the packets over time: a NIC that has just
// been powered by the switch can miss the first one.
const (
	Repeats     = 3
	RepeatDelay = 200 * time.Millisecond
)

// BuildPacket returns the magic packet for a MAC address.
func BuildPacket(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("wol: need a 6-byte MAC, got %d bytes", len(mac))
	}
	pkt := make([]byte, 0, PacketLen)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xFF)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, mac...)
	}
	return pkt, nil
}

// ParseMAC accepts the usual separators (":", "-", none) and returns a 6-byte
// address.
func ParseMAC(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(s)
	if err != nil {
		// net.ParseMAC rejects the bare 12-hex-digit form.
		if len(s) == 12 {
			var withColons string
			for i := 0; i < 12; i += 2 {
				if i > 0 {
					withColons += ":"
				}
				withColons += s[i : i+2]
			}
			mac, err = net.ParseMAC(withColons)
		}
		if err != nil {
			return nil, fmt.Errorf("wol: parse MAC %q: %w", s, err)
		}
	}
	if len(mac) != 6 {
		return nil, fmt.Errorf("wol: %q is not a 6-byte MAC", s)
	}
	return mac, nil
}

// BroadcastAddr returns the directed broadcast address for a CIDR prefix, e.g.
// 192.168.1.0/24 -> 192.168.1.255.
//
// A directed broadcast reaches the right subnet even when the sender has
// several interfaces, which 255.255.255.255 does not reliably do.
func BroadcastAddr(prefix netip.Prefix) (netip.Addr, error) {
	if !prefix.IsValid() {
		return netip.Addr{}, errors.New("wol: invalid prefix")
	}
	if !prefix.Addr().Is4() {
		return netip.Addr{}, errors.New("wol: directed broadcast needs an IPv4 prefix")
	}
	base := prefix.Masked().Addr().As4()
	bits := prefix.Bits()
	hostBits := 32 - bits
	var out [4]byte
	copy(out[:], base[:])
	// Set every host bit.
	for i := 0; i < hostBits; i++ {
		byteIdx := 3 - i/8
		bitIdx := uint(i % 8)
		out[byteIdx] |= 1 << bitIdx
	}
	return netip.AddrFrom4(out), nil
}

// Targets lists the destinations a wake is sent to: the directed broadcast
// when a LAN prefix is known, plus the limited broadcast as a fallback.
func Targets(lanPrefix string) ([]netip.Addr, error) {
	targets := []netip.Addr{}
	if lanPrefix != "" {
		p, err := netip.ParsePrefix(lanPrefix)
		if err != nil {
			return nil, fmt.Errorf("wol: parse lan prefix: %w", err)
		}
		b, err := BroadcastAddr(p)
		if err != nil {
			return nil, err
		}
		targets = append(targets, b)
	}
	targets = append(targets, netip.AddrFrom4([4]byte{255, 255, 255, 255}))
	return targets, nil
}

// Sender sends magic packets. Split out so the waker's handler can be tested
// against a recorder instead of real UDP sockets.
type Sender interface {
	Send(pkt []byte, to netip.AddrPort) error
}

// UDPSender is the real implementation.
type UDPSender struct{}

// Send writes one packet to one address:port.
func (UDPSender) Send(pkt []byte, to netip.AddrPort) error {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(to))
	if err != nil {
		return fmt.Errorf("wol: dial %s: %w", to, err)
	}
	defer conn.Close()
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("wol: write to %s: %w", to, err)
	}
	return nil
}

// Wake sends the magic packet for mac to every target port and address,
// Repeats times. It returns the first error but keeps trying the rest: one
// unreachable broadcast address should not stop the others.
func Wake(s Sender, mac net.HardwareAddr, lanPrefix string, sleep func(time.Duration)) error {
	pkt, err := BuildPacket(mac)
	if err != nil {
		return err
	}
	targets, err := Targets(lanPrefix)
	if err != nil {
		return err
	}
	if sleep == nil {
		sleep = time.Sleep
	}

	var firstErr error
	for r := 0; r < Repeats; r++ {
		if r > 0 {
			sleep(RepeatDelay)
		}
		for _, addr := range targets {
			for _, port := range Ports {
				if err := s.Send(pkt, netip.AddrPortFrom(addr, uint16(port))); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}
