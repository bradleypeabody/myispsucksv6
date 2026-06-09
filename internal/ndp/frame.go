package ndp

import (
	"encoding/binary"
	"net"
	"net/netip"
)

const (
	ethTypeIPv6   = 0x86DD
	ipv6HeaderLen = 40
	ethHeaderLen  = 14
	icmpv6NS      = 135
	icmpv6NA      = 136
	// Offset of ICMPv6 type within an Ethernet frame: Eth(14) + IPv6(40)
	icmpv6TypeOffset = ethHeaderLen + ipv6HeaderLen // 54
)

// buildNS constructs a raw Ethernet+IPv6+ICMPv6 Neighbor Solicitation frame.
// src is the router's link-local address; target is the address being probed.
func buildNS(ifi *net.Interface, src, target netip.Addr) ([]byte, error) {
	// Destination: solicited-node multicast ff02::1:ff<last3>.
	t16 := target.As16()
	dstIP := [16]byte{0xff, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01, 0xff, t16[13], t16[14], t16[15]}

	// ICMPv6 NS payload: type(1)+code(1)+cksum(2)+reserved(4)+target(16)+SLLA(8)
	payload := make([]byte, 32)
	payload[0] = icmpv6NS
	copy(payload[8:24], t16[:])
	payload[24] = 1 // SLLA option type
	payload[25] = 1 // length in 8-byte units
	copy(payload[26:32], ifi.HardwareAddr)

	s16 := src.As16()
	cksum := icmpv6Checksum(s16[:], dstIP[:], payload)
	payload[2] = byte(cksum >> 8)
	payload[3] = byte(cksum)

	dstMAC := multicastMAC(t16)
	return buildFrame(ifi.HardwareAddr, dstMAC, s16[:], dstIP[:], payload), nil
}

// buildFrame assembles a raw Ethernet + IPv6 frame.
func buildFrame(srcMAC, dstMAC, srcIP, dstIP, payload []byte) []byte {
	frame := make([]byte, ethHeaderLen+ipv6HeaderLen+len(payload))
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], ethTypeIPv6)

	h := frame[ethHeaderLen:]
	h[0] = 0x60 // version 6
	binary.BigEndian.PutUint16(h[4:6], uint16(len(payload)))
	h[6] = 58  // next header = ICMPv6
	h[7] = 255 // hop limit (255 required by RFC 4861)
	copy(h[8:24], srcIP)
	copy(h[24:40], dstIP)

	copy(frame[ethHeaderLen+ipv6HeaderLen:], payload)
	return frame
}

// icmpv6Checksum computes the ICMPv6 checksum per RFC 2463.
func icmpv6Checksum(src, dst, payload []byte) uint16 {
	length := uint32(len(payload))
	var sum uint32
	sum += sumBytes(src)
	sum += sumBytes(dst)
	sum += length >> 16
	sum += length & 0xffff
	sum += 58 // next-header = ICMPv6
	sum += sumBytes(payload)
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func sumBytes(b []byte) uint32 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 != 0 {
		s += uint32(b[len(b)-1]) << 8
	}
	return s
}

func multicastMAC(ip16 [16]byte) []byte {
	return []byte{0x33, 0x33, ip16[12], ip16[13], ip16[14], ip16[15]}
}

func htons(v uint16) uint16 {
	return (v>>8)&0xff | (v&0xff)<<8
}
