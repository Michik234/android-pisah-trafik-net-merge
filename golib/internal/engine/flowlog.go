package engine

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The flow log backs the app's live connection monitor: every TCP connection and UDP session the
// engine opens is recorded with its destination, the network it left on and its byte counts, so
// the user can see where traffic goes (and find a game's ports). It lives in memory only and holds
// destinations, not contents; it is bounded and never written anywhere.

const (
	maxTrackedFlows = 4096             // beyond this new flows still work, they just are not listed
	maxClosedFlows  = 200              // finished flows kept for the list
	closedFlowTTL   = 10 * time.Minute // and for how long
)

// Flow is one connection being watched. All methods are safe on a nil *Flow, which is what a flow
// that was not tracked (table full) gets.
type Flow struct {
	id      uint64
	proto   string // "tcp" or "udp"
	dst     string // destination IP
	port    int
	network string // name of the network it left on
	game    bool   // matched split mode's game ports
	started time.Time

	in, out atomic.Int64
	last    atomic.Int64 // unix nanoseconds of the last byte
	ended   atomic.Int64 // unix nanoseconds when it closed; 0 = still open
}

type flowTable struct {
	mu     sync.Mutex
	next   uint64
	active map[uint64]*Flow
	closed []*Flow // oldest first
}

// FlowInfo is one row of the monitor.
type FlowInfo struct {
	ID      uint64 `json:"id"`
	Proto   string `json:"proto"`
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	Network string `json:"net"`
	Active  bool   `json:"active"`
	Game    bool   `json:"game"`
	In      int64  `json:"in"`  // bytes from the server
	Out     int64  `json:"out"` // bytes to the server
	Started int64  `json:"started"`
	Last    int64  `json:"last"` // unix ms of the last byte (or of the start if there was none)
	Ended   int64  `json:"ended"`
}

// track registers a new flow. proto is "tcp" or "udp".
func (b *Balancer) track(proto, dst string, port int, network string, game bool) *Flow {
	t := &b.flows
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.active) >= maxTrackedFlows {
		return nil
	}
	if t.active == nil {
		t.active = make(map[uint64]*Flow)
	}
	t.next++
	f := &Flow{id: t.next, proto: proto, dst: dst, port: port, network: network, game: game, started: time.Now()}
	f.last.Store(f.started.UnixNano())
	t.active[f.id] = f
	return f
}

// untrack marks a flow finished. It is safe to call more than once.
func (b *Balancer) untrack(f *Flow) {
	if f == nil || !f.ended.CompareAndSwap(0, time.Now().UnixNano()) {
		return
	}
	t := &b.flows
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.active, f.id)
	t.closed = append(t.closed, f)
	if n := len(t.closed); n > maxClosedFlows {
		t.closed = append([]*Flow(nil), t.closed[n-maxClosedFlows:]...)
	}
}

func (f *Flow) addIn(n int) {
	if f != nil && n > 0 {
		f.in.Add(int64(n))
		f.last.Store(time.Now().UnixNano())
	}
}

func (f *Flow) addOut(n int) {
	if f != nil && n > 0 {
		f.out.Add(int64(n))
		f.last.Store(time.Now().UnixNano())
	}
}

func (f *Flow) info() FlowInfo {
	end := f.ended.Load()
	i := FlowInfo{
		ID: f.id, Proto: f.proto, IP: f.dst, Port: f.port, Network: f.network,
		Active: end == 0, Game: f.game, In: f.in.Load(), Out: f.out.Load(),
		Started: f.started.UnixMilli(), Last: f.last.Load() / 1e6,
	}
	if end != 0 {
		i.Ended = end / 1e6
	}
	return i
}

// Flows lists up to limit flows: open ones first (most recently active first), then finished ones
// (most recently finished first). Finished flows older than ten minutes are dropped.
func (b *Balancer) Flows(limit int) []FlowInfo {
	t := &b.flows
	t.mu.Lock()
	cutoff := time.Now().Add(-closedFlowTTL).UnixNano()
	keep := t.closed[:0]
	for _, f := range t.closed {
		if f.ended.Load() >= cutoff {
			keep = append(keep, f)
		}
	}
	t.closed = keep
	open := make([]FlowInfo, 0, len(t.active))
	for _, f := range t.active {
		open = append(open, f.info())
	}
	done := make([]FlowInfo, 0, len(t.closed))
	for i := len(t.closed) - 1; i >= 0; i-- { // t.closed is in the order flows ended: newest last
		done = append(done, t.closed[i].info())
	}
	t.mu.Unlock()

	sort.Slice(open, func(i, j int) bool {
		if open[i].Last != open[j].Last {
			return open[i].Last > open[j].Last
		}
		return open[i].ID > open[j].ID
	})
	out := append(open, done...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ClearFlows forgets the finished flows.
func (b *Balancer) ClearFlows() {
	b.flows.mu.Lock()
	b.flows.closed = nil
	b.flows.mu.Unlock()
}

// isGameFlow reports whether split mode treats a flow to port as a game flow.
func (b *Balancer) isGameFlow(udp bool, port int) bool {
	c := b.split.p.Load()
	if c == nil || !c.Active() {
		return false
	}
	if udp {
		return c.UDP.Has(port)
	}
	return c.TCP.Has(port)
}
