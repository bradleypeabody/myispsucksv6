//go:build linux

package ndp

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

const (
	probeTimeout  = 500 * time.Millisecond
	cacheValidTTL = 60 * time.Second
	cacheNegTTL   = 5 * time.Second
)

type cacheState int

const (
	cacheIncomplete cacheState = iota
	cacheValid
	cacheInvalid
)

type cacheEntry struct {
	state   cacheState
	expires time.Time
}

// Run starts the NDP proxy and blocks until ctx is done.
func (p *Proxy) Run(ctx context.Context) error {
	p.lazyInit()

	wanIfi, err := net.InterfaceByName(p.WANInterface)
	if err != nil {
		return &proxyError{"ndp: WAN interface: " + err.Error()}
	}
	lanIfi, err := net.InterfaceByName(p.LANInterface)
	if err != nil {
		return &proxyError{"ndp: LAN interface: " + err.Error()}
	}

	wanFd, err := openPacketSocket(wanIfi.Index, icmpv6NS, true)
	if err != nil {
		return &proxyError{"ndp: open WAN socket: " + err.Error()}
	}
	defer unix.Close(wanFd)

	lanFd, err := openPacketSocket(lanIfi.Index, icmpv6NA, false)
	if err != nil {
		return &proxyError{"ndp: open LAN socket: " + err.Error()}
	}
	defer unix.Close(lanFd)

	slog.Info("ndp: proxy started", "wan", p.WANInterface, "lan", p.LANInterface)

	var (
		mu      sync.Mutex
		cache   = make(map[netip.Addr]*cacheEntry)
		current netip.Prefix
	)

	go func() {
		<-ctx.Done()
		unix.Close(wanFd)
		unix.Close(lanFd)
	}()

	getLANLinkLocal := func() (netip.Addr, error) {
		addrs, err := lanIfi.Addrs()
		if err != nil {
			return netip.Addr{}, err
		}
		for _, a := range addrs {
			prefix, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			addr := prefix.Addr()
			if addr.Is6() && addr.IsLinkLocalUnicast() {
				return addr, nil
			}
		}
		return netip.Addr{}, &proxyError{"ndp: no LAN link-local address found"}
	}

	probe := func(target netip.Addr) {
		llAddr, err := getLANLinkLocal()
		if err != nil {
			slog.Warn("ndp: probe: can't get LAN link-local", "err", err)
			mu.Lock()
			cache[target] = &cacheEntry{state: cacheInvalid, expires: time.Now().Add(cacheNegTTL)}
			mu.Unlock()
			return
		}

		frame, err := buildNS(lanIfi, llAddr, target)
		if err != nil {
			slog.Warn("ndp: probe: build NS failed", "target", target, "err", err)
			mu.Lock()
			cache[target] = &cacheEntry{state: cacheInvalid, expires: time.Now().Add(cacheNegTTL)}
			mu.Unlock()
			return
		}

		t16 := target.As16()
		dst := unix.SockaddrLinklayer{
			Protocol: htons(ethTypeIPv6),
			Ifindex:  lanIfi.Index,
			Halen:    6,
			Addr:     [8]byte{0x33, 0x33, 0xff, t16[13], t16[14], t16[15]},
		}
		if err := unix.Sendto(lanFd, frame, 0, &dst); err != nil && ctx.Err() == nil {
			slog.Warn("ndp: probe: sendto failed", "target", target, "err", err)
		}

		tv := unix.NsecToTimeval(probeTimeout.Nanoseconds())
		unix.SetsockoptTimeval(lanFd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		buf := make([]byte, 1500)
		deadline := time.Now().Add(probeTimeout)
		for time.Now().Before(deadline) {
			n, _, err := unix.Recvfrom(lanFd, buf, 0)
			if err != nil {
				break
			}
			if n < icmpv6TypeOffset+24 {
				continue
			}
			if buf[icmpv6TypeOffset] != icmpv6NA {
				continue
			}
			naTarget, ok := netip.AddrFromSlice(buf[icmpv6TypeOffset+8 : icmpv6TypeOffset+24])
			if !ok {
				continue
			}
			if naTarget.Unmap() == target {
				mu.Lock()
				cache[target] = &cacheEntry{state: cacheValid, expires: time.Now().Add(cacheValidTTL)}
				mu.Unlock()
				slog.Debug("ndp: probe: target live", "target", target)
				unix.SetsockoptTimeval(lanFd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})
				return
			}
		}
		unix.SetsockoptTimeval(lanFd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})

		mu.Lock()
		cache[target] = &cacheEntry{state: cacheInvalid, expires: time.Now().Add(cacheNegTTL)}
		mu.Unlock()
		slog.Debug("ndp: probe: target not found", "target", target)
	}

	buf := make([]byte, 1500)
	for {
		select {
		case pfx := <-p.updateCh:
			slog.Info("ndp: proxy prefix updated", "prefix", pfx)
			mu.Lock()
			current = pfx
			cache = make(map[netip.Addr]*cacheEntry)
			mu.Unlock()
		default:
		}

		if ctx.Err() != nil {
			return nil
		}

		tv := unix.NsecToTimeval(time.Second.Nanoseconds())
		unix.SetsockoptTimeval(wanFd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

		n, _, err := unix.Recvfrom(wanFd, buf, 0)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("ndp: WAN recvfrom error", "err", err)
			continue
		}
		if n < icmpv6TypeOffset+24 {
			continue
		}
		if buf[icmpv6TypeOffset] != icmpv6NS {
			continue
		}

		target, ok := netip.AddrFromSlice(buf[icmpv6TypeOffset+8 : icmpv6TypeOffset+24])
		if !ok {
			continue
		}
		target = target.Unmap()

		// IPv6 source is at Eth(14)+8 = offset 22, 16 bytes.
		nsSrc, ok := netip.AddrFromSlice(buf[ethHeaderLen+8 : ethHeaderLen+24])
		if !ok {
			continue
		}
		nsSrc = nsSrc.Unmap()

		mu.Lock()
		pfx := current
		mu.Unlock()

		if !pfx.IsValid() || !pfx.Contains(target) {
			continue
		}

		mu.Lock()
		entry := cache[target]
		now := time.Now()
		if entry != nil && now.Before(entry.expires) {
			switch entry.state {
			case cacheValid:
				mu.Unlock()
				go func(t, src netip.Addr) {
					if err := sendNA(wanFd, wanIfi, t, src); err != nil && ctx.Err() == nil {
						slog.Warn("ndp: sendNA failed", "target", t, "err", err)
					}
				}(target, nsSrc)
				continue
			case cacheInvalid:
				mu.Unlock()
				continue
			case cacheIncomplete:
				mu.Unlock()
				continue
			}
		}
		cache[target] = &cacheEntry{state: cacheIncomplete, expires: now.Add(probeTimeout * 2)}
		mu.Unlock()

		go func(t, src netip.Addr) {
			probe(t)
			mu.Lock()
			e := cache[t]
			mu.Unlock()
			if e != nil && e.state == cacheValid {
				if err := sendNA(wanFd, wanIfi, t, src); err != nil && ctx.Err() == nil {
					slog.Warn("ndp: sendNA failed after probe", "target", t, "err", err)
				}
			}
		}(target, nsSrc)
	}
}

// openPacketSocket opens an AF_PACKET SOCK_RAW socket bound to ifIndex,
// with a BPF filter accepting only ICMPv6 messages of the given type.
// promisc puts the interface into promiscuous mode on this socket so the NIC
// delivers frames addressed to multicast groups the kernel hasn't joined —
// required on the WAN socket to receive NS for LAN hosts' solicited-node groups.
func openPacketSocket(ifIndex, icmpv6Type int, promisc bool) (int, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(ethTypeIPv6)))
	if err != nil {
		return -1, err
	}
	if err := attachICMPv6Filter(fd, icmpv6Type); err != nil {
		unix.Close(fd)
		return -1, err
	}
	sa := &unix.SockaddrLinklayer{
		Protocol: htons(ethTypeIPv6),
		Ifindex:  ifIndex,
	}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if promisc {
		mreq := &unix.PacketMreq{
			Ifindex: int32(ifIndex),
			Type:    unix.PACKET_MR_PROMISC,
		}
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, mreq); err != nil {
			unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// attachICMPv6Filter attaches a BPF program that accepts only IPv6 packets
// carrying ICMPv6 messages of the given type.
func attachICMPv6Filter(fd, icmpv6Type int) error {
	// BPF program (classical BPF):
	//   ldh  [12]            ; Ethernet type field
	//   jne  #0x86dd, drop   ; must be IPv6
	//   ldb  [20]            ; IPv6 next-header (byte 6 of IPv6 = offset 20)
	//   jne  #58,    drop    ; must be ICMPv6 (next-header 58)
	//   ldb  [54]            ; ICMPv6 type (Eth(14)+IPv6(40)=54)
	//   jne  #<type>, drop   ; must match requested type
	//   ret  #0xffff         ; accept packet
	//   ret  #0              ; drop packet
	insns, err := bpf.Assemble([]bpf.Instruction{
		bpf.LoadAbsolute{Off: 12, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: ethTypeIPv6, SkipTrue: 5},
		bpf.LoadAbsolute{Off: 20, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 58, SkipTrue: 3},
		bpf.LoadAbsolute{Off: 54, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(icmpv6Type), SkipTrue: 1},
		bpf.RetConstant{Val: 0xffff},
		bpf.RetConstant{Val: 0},
	})
	if err != nil {
		return err
	}
	prog := unix.SockFprog{
		Len:    uint16(len(insns)),
		Filter: (*unix.SockFilter)(unsafe.Pointer(&insns[0])),
	}
	return unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &prog)
}

// sendNA builds and sends a Neighbor Advertisement on fd.
// target is the proxied address (IPv6 source); nsSrc is the NS solicitor (IPv6 dst).
func sendNA(fd int, wanIfi *net.Interface, target, nsSrc netip.Addr) error {
	t16 := target.As16()
	d16 := nsSrc.As16()

	// ICMPv6 NA: type(1)+code(1)+cksum(2)+flags(4)+target(16)+TLLA(8)
	// Flags: S=1 (Solicited), O=0, R=0 → byte[4]=0x40
	payload := make([]byte, 32)
	payload[0] = icmpv6NA
	payload[4] = 0x40
	copy(payload[8:24], t16[:])
	payload[24] = 2 // TLLA option type
	payload[25] = 1 // 8-byte units
	copy(payload[26:32], wanIfi.HardwareAddr)

	cksum := icmpv6Checksum(t16[:], d16[:], payload)
	payload[2] = byte(cksum >> 8)
	payload[3] = byte(cksum)

	// Use multicast MAC derived from destination IPv6 — the unicast dst IP ensures
	// only the intended host processes it while not requiring an ARP/ND lookup.
	var dstMAC [6]byte
	copy(dstMAC[:], []byte{0x33, 0x33, d16[12], d16[13], d16[14], d16[15]})

	frame := buildFrame(wanIfi.HardwareAddr, dstMAC[:], t16[:], d16[:], payload)
	sa := &unix.SockaddrLinklayer{
		Protocol: htons(ethTypeIPv6),
		Ifindex:  wanIfi.Index,
		Halen:    6,
	}
	copy(sa.Addr[:], dstMAC[:])
	return unix.Sendto(fd, frame, 0, sa)
}

func isTimeout(err error) bool {
	errno, ok := err.(unix.Errno)
	return ok && (errno == unix.EAGAIN || errno == unix.EWOULDBLOCK)
}
