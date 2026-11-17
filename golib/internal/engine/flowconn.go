package engine

import (
	"context"
	"errors"
	"net"
	"sync"

	"netmerge-android/internal/nics"
)

// slotConn counts a pinned TCP flow's bytes and releases its slot on Close. It exposes
// CloseRead/CloseWrite because the TUN relay half-closes each direction separately.
type slotConn struct {
	*net.TCPConn
	sl   *Slot
	fam  Family
	s    *Scheduler
	once sync.Once
	b    *Balancer
	flow *Flow // for the live monitor; may be nil
}

func (c *slotConn) Read(p []byte) (int, error) {
	n, err := c.TCPConn.Read(p)
	c.sl.bytesIn[c.fam].Add(int64(n))
	c.flow.addIn(n)
	return n, err
}

func (c *slotConn) Write(p []byte) (int, error) {
	n, err := c.TCPConn.Write(p)
	c.sl.bytesOut[c.fam].Add(int64(n))
	c.flow.addOut(n)
	return n, err
}

// release frees the slot's balancing count. It happens at the first half-close, not at the final
// Close: the relay can keep a finished flow open for up to a minute waiting for the peer's FIN,
// and counting those as active would steer new flows away from the NIC that just finished.
func (c *slotConn) release() { c.once.Do(func() { c.s.ReleaseFor(c.sl, c.fam) }) }

func (c *slotConn) CloseRead() error  { c.release(); return c.TCPConn.CloseRead() }
func (c *slotConn) CloseWrite() error { c.release(); return c.TCPConn.CloseWrite() }
func (c *slotConn) Close() error {
	c.release()
	c.sl.conns.Delete(c)
	c.b.untrack(c.flow)
	return c.TCPConn.Close()
}

// DialPinned is DialTCP for front-ends that hand the conn to someone else to pump.
func (b *Balancer) DialPinned(ctx context.Context, host string, port int) (net.Conn, error) {
	return b.DialPinnedFlow(ctx, FlowKey{DstIP: host, DstPort: port})
}

// DialPinnedFlow is DialPinned for a flow whose source is known, so rules can ask which app it is.
func (b *Balancer) DialPinnedFlow(ctx context.Context, k FlowKey) (net.Conn, error) {
	fc := b.newCtx(k)
	c, sl, f, err := b.dialTCP(ctx, k.DstIP, k.DstPort, fc)
	if err != nil {
		return nil, err
	}
	sc := &slotConn{
		TCPConn: c.(*net.TCPConn), sl: sl, fam: f, s: b.S, b: b,
		flow: b.track("tcp", k.DstIP, k.DstPort, sl.Name, fc.meta(b.isGameFlow(false, k.DstPort))),
	}
	sl.conns.Store(sc, struct{}{})
	return sc, nil
}

type slotPacketConn struct {
	net.PacketConn
	sl   *Slot
	fam  Family
	b    *Balancer
	flow *Flow // for the live monitor; may be nil
}

func (c *slotPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(p)
	c.sl.bytesIn[c.fam].Add(int64(n))
	c.flow.addIn(n)
	return n, a, err
}

func (c *slotPacketConn) Close() error {
	c.sl.conns.Delete(c)
	if c.b != nil {
		c.b.untrack(c.flow)
	}
	return c.PacketConn.Close()
}

func (c *slotPacketConn) WriteTo(p []byte, a net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, a)
	c.sl.bytesOut[c.fam].Add(int64(n))
	c.flow.addOut(n)
	return n, err
}

// ListenUDP opens a UDP socket pinned to one NIC (a UDP "flow" = the socket, so every datagram
// of that flow leaves the same way). UDP flows are short and numerous (DNS), so they don't hold
// a balancing slot: the pick still spreads them via the scheduler's tie-breaking.
func (b *Balancer) ListenUDP(ctx context.Context, f Family) (net.PacketConn, error) {
	return b.listenUDP(ctx, f, nil)
}

// ListenUDPTo is ListenUDP for a flow headed to dstIP:dstPort: split mode may pick the network by
// the port, and the live monitor lists the flow under that destination.
func (b *Balancer) ListenUDPTo(ctx context.Context, f Family, dstIP string, dstPort int) (net.PacketConn, error) {
	return b.ListenUDPFlow(ctx, f, FlowKey{UDP: true, DstIP: dstIP, DstPort: dstPort})
}

// ListenUDPFlow is ListenUDPTo for a flow whose source is known, so rules can ask which app it is.
func (b *Balancer) ListenUDPFlow(ctx context.Context, f Family, k FlowKey) (net.PacketConn, error) {
	k.UDP = true
	return b.listenUDP(ctx, f, b.newCtx(k))
}

// listenUDP opens the socket. fc is nil for a flow nothing is known about (no rules, no listing).
func (b *Balancer) listenUDP(ctx context.Context, f Family, fc *flowCtx) (net.PacketConn, error) {
	var rt *route
	var dstIP string
	var dstPort int
	if fc != nil {
		rt, dstIP, dstPort = b.decide(fc), fc.k.DstIP, fc.k.DstPort
	}
	if rt != nil && rt.deny {
		if dstPort != 0 {
			b.untrack(b.track("udp", dstIP, dstPort, blockedNetwork, fc.meta(false)))
		}
		return nil, errBlocked
	}
	network := "udp4"
	if f == Fam6 {
		network = "udp6"
	}
	tried := map[string]struct{}{}
	lastErr := errors.New("no usable network adapter")
	for {
		sl := b.acquire(f, tried, rt)
		if sl == nil {
			return nil, lastErr
		}
		tried[sl.key] = struct{}{}
		lc := net.ListenConfig{Control: nics.ForceEgress(sl.Index, f == Fam6)}
		pc, err := lc.ListenPacket(ctx, network, net.JoinHostPort(sl.ip(f).String(), "0"))
		b.S.ReleaseFor(sl, f)
		if err != nil {
			if classify(err).linkDown {
				b.S.Fail(sl, f, b.FailCooldown)
			}
			lastErr = err
			continue
		}
		spc := &slotPacketConn{PacketConn: pc, sl: sl, fam: f, b: b}
		if dstPort != 0 {
			spc.flow = b.track("udp", dstIP, dstPort, sl.Name, fc.meta(b.isGameFlow(true, dstPort)))
		}
		sl.conns.Store(spc, struct{}{})
		return spc, nil
	}
}
