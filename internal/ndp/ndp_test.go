package ndp

import (
	"bytes"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/bpf"
)

var (
	testWANMAC = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	testLANMAC = net.HardwareAddr{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	testLANIfi = &net.Interface{Name: "lan0", HardwareAddr: testLANMAC, Index: 2}
)

// TestSetPrefixDrainAndReplace verifies that SetPrefix keeps only the latest value.
func TestSetPrefixDrainAndReplace(t *testing.T) {
	p := &Proxy{WANInterface: "wan0", LANInterface: "lan0"}
	p1 := netip.MustParsePrefix("2607:fb90:1111::/64")
	p2 := netip.MustParsePrefix("2607:fb90:2222::/64")

	p.SetPrefix(p1)
	p.SetPrefix(p2)

	select {
	case got := <-p.updateCh:
		if got != p2 {
			t.Errorf("got %s, want %s", got, p2)
		}
	default:
		t.Error("channel empty after SetPrefix")
	}
}

// TestBuildNSFrameStructure verifies the Ethernet+IPv6+ICMPv6 NS frame layout.
func TestBuildNSFrameStructure(t *testing.T) {
	src := netip.MustParseAddr("fe80::1")
	target := netip.MustParseAddr("2607:fb90:1234:5678::abcd")

	frame, err := buildNS(testLANIfi, src, target)
	if err != nil {
		t.Fatalf("buildNS: %v", err)
	}

	// Must be at least Eth(14)+IPv6(40)+ICMPv6(32) = 86 bytes.
	if len(frame) < 86 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}

	// Ethernet type = 0x86DD
	etherType := uint16(frame[12])<<8 | uint16(frame[13])
	if etherType != ethTypeIPv6 {
		t.Errorf("EtherType = 0x%04x, want 0x86DD", etherType)
	}

	// IPv6 next header = 58 (ICMPv6)
	if frame[ethHeaderLen+6] != 58 {
		t.Errorf("IPv6 next header = %d, want 58", frame[ethHeaderLen+6])
	}

	// IPv6 hop limit = 255 (RFC 4861 requirement)
	if frame[ethHeaderLen+7] != 255 {
		t.Errorf("hop limit = %d, want 255", frame[ethHeaderLen+7])
	}

	// ICMPv6 type = 135 (NS)
	if frame[icmpv6TypeOffset] != icmpv6NS {
		t.Errorf("ICMPv6 type = %d, want %d", frame[icmpv6TypeOffset], icmpv6NS)
	}

	// ICMPv6 code = 0
	if frame[icmpv6TypeOffset+1] != 0 {
		t.Errorf("ICMPv6 code = %d, want 0", frame[icmpv6TypeOffset+1])
	}

	// Target address at offset icmpv6TypeOffset+8
	got, ok := netip.AddrFromSlice(frame[icmpv6TypeOffset+8 : icmpv6TypeOffset+24])
	if !ok {
		t.Fatal("can't parse target from NS frame")
	}
	if got.Unmap() != target {
		t.Errorf("NS target = %s, want %s", got.Unmap(), target)
	}
}

// TestBuildNAFrameStructure verifies the NA frame layout.
func TestBuildNAFrameStructure(t *testing.T) {
	target := netip.MustParseAddr("2607:fb90:1234:5678::abcd")
	nsSrc := netip.MustParseAddr("2607:fb90:1234:5678::1")

	// Build the frame directly using internal helpers.
	t16 := target.As16()
	d16 := nsSrc.As16()
	payload := make([]byte, 32)
	payload[0] = icmpv6NA
	payload[4] = 0x40
	copy(payload[8:24], t16[:])
	payload[24] = 2
	payload[25] = 1
	copy(payload[26:32], testWANMAC)
	cksum := icmpv6Checksum(t16[:], d16[:], payload)
	payload[2] = byte(cksum >> 8)
	payload[3] = byte(cksum)
	dstMAC := []byte{0x33, 0x33, d16[12], d16[13], d16[14], d16[15]}
	frame := buildFrame(testWANMAC, dstMAC, t16[:], d16[:], payload)

	if frame[icmpv6TypeOffset] != icmpv6NA {
		t.Errorf("ICMPv6 type = %d, want %d (NA)", frame[icmpv6TypeOffset], icmpv6NA)
	}
	// Solicited flag (byte 4 of ICMPv6 payload = offset icmpv6TypeOffset+4)
	if frame[icmpv6TypeOffset+4] != 0x40 {
		t.Errorf("NA flags byte = 0x%02x, want 0x40 (Solicited)", frame[icmpv6TypeOffset+4])
	}
	// Override flag must NOT be set
	if frame[icmpv6TypeOffset+4]&0x20 != 0 {
		t.Error("NA Override flag should not be set (we are a proxy, not the host)")
	}
	// Router flag must NOT be set
	if frame[icmpv6TypeOffset+4]&0x80 != 0 {
		t.Error("NA Router flag should not be set")
	}
	// TLLA option type = 2
	if frame[icmpv6TypeOffset+24] != 2 {
		t.Errorf("TLLA option type = %d, want 2", frame[icmpv6TypeOffset+24])
	}
}

// TestICMPv6ChecksumNonZero verifies the checksum doesn't trivially return 0.
func TestICMPv6ChecksumNonZero(t *testing.T) {
	src := netip.MustParseAddr("fe80::1").As16()
	dst := netip.MustParseAddr("ff02::1:ff00:abcd").As16()
	payload := make([]byte, 32)
	payload[0] = icmpv6NS

	cksum := icmpv6Checksum(src[:], dst[:], payload)
	if cksum == 0 {
		t.Error("checksum is 0, likely a bug")
	}
}

// TestICMPv6ChecksumRoundTrip verifies that embedding the checksum gives a net
// checksum of 0xffff (i.e., verification passes).
func TestICMPv6ChecksumRoundTrip(t *testing.T) {
	src := netip.MustParseAddr("fe80::1").As16()
	dst := netip.MustParseAddr("ff02::1:ff00:abcd").As16()
	payload := make([]byte, 32)
	payload[0] = icmpv6NS

	cksum := icmpv6Checksum(src[:], dst[:], payload)
	payload[2] = byte(cksum >> 8)
	payload[3] = byte(cksum)

	// Re-computing over the same data with the checksum embedded:
	// S + ~S = 0xFFFF in ones-complement, then ^0xFFFF = 0x0000 → valid packet.
	result := icmpv6Checksum(src[:], dst[:], payload)
	if result != 0x0000 {
		t.Errorf("round-trip checksum = 0x%04x, want 0x0000 (valid)", result)
	}
}

// TestHtons verifies byte-swap.
func TestHtons(t *testing.T) {
	if htons(0x86DD) != 0xDD86 {
		t.Errorf("htons(0x86DD) = 0x%04x, want 0xDD86", htons(0x86DD))
	}
}

// TestMulticastMAC verifies the EUI-48 multicast MAC for a solicited-node addr.
func TestMulticastMAC(t *testing.T) {
	target := netip.MustParseAddr("2607:fb90:1234:5678::abcd")
	t16 := target.As16()
	mac := multicastMAC(t16)
	want := net.HardwareAddr{0x33, 0x33, t16[12], t16[13], t16[14], t16[15]}
	if !bytes.Equal(mac, want) {
		t.Errorf("multicastMAC = %v, want %v", mac, want)
	}
}

// TestBPFAssembly verifies the BPF filter assembles without error.
// We test both NS (135) and NA (136) filter programs.
func TestBPFAssembly(t *testing.T) {
	for _, typ := range []int{icmpv6NS, icmpv6NA} {
		insns, err := bpf.Assemble([]bpf.Instruction{
			bpf.LoadAbsolute{Off: 12, Size: 2},
			bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: ethTypeIPv6, SkipTrue: 5},
			bpf.LoadAbsolute{Off: 20, Size: 1},
			bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 58, SkipTrue: 3},
			bpf.LoadAbsolute{Off: 54, Size: 1},
			bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(typ), SkipTrue: 1},
			bpf.RetConstant{Val: 0xffff},
			bpf.RetConstant{Val: 0},
		})
		if err != nil {
			t.Errorf("BPF assemble for type %d: %v", typ, err)
		}
		if len(insns) != 8 {
			t.Errorf("expected 8 BPF instructions, got %d", len(insns))
		}
	}
}

// TestBuildFrameLength verifies the output frame is exactly the right size.
func TestBuildFrameLength(t *testing.T) {
	payload := make([]byte, 32)
	frame := buildFrame(testWANMAC, testLANMAC, make([]byte, 16), make([]byte, 16), payload)
	want := ethHeaderLen + ipv6HeaderLen + len(payload)
	if len(frame) != want {
		t.Errorf("frame length = %d, want %d", len(frame), want)
	}
}

// TestSumBytesOddLength ensures the odd-byte case in sumBytes is handled.
func TestSumBytesOddLength(t *testing.T) {
	b := []byte{0x01, 0x02, 0x03}
	got := sumBytes(b)
	// 0x0102 + 0x0300 (pad) = 0x0402
	want := uint32(0x0102 + 0x0300)
	if got != want {
		t.Errorf("sumBytes odd = 0x%04x, want 0x%04x", got, want)
	}
}
