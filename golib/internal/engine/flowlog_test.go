package engine

import (
	"net"
	"testing"
	"time"

	"netmerge-android/internal/nics"
)

func newFlowBalancer() *Balancer {
	return NewBalancer(NewScheduler([]nics.Nic{
		{Name: "wlan0", IP: net.ParseIP("192.168.1.5"), Weight: 1},
		{Name: "wwan0", IP: net.ParseIP("10.20.30.40"), Weight: 1, Cellular: true},
	}))
}

func TestFlowLogTrackAndSnapshot(t *testing.T) {
	b := newFlowBalancer()
	a := b.track("tcp", "93.184.216.34", 443, "Wi-Fi", false)
	g := b.track("udp", "203.0.113.7", 7500, "Mobile data", true)
	a.addIn(1000)
	a.addOut(200)
	g.addOut(64)
	g.addIn(0) // nothing read: must not count or touch "last"

	got := b.Flows(0)
	if len(got) != 2 {
		t.Fatalf("want 2 flows, got %d", len(got))
	}
	byPort := map[int]FlowInfo{got[0].Port: got[0], got[1].Port: got[1]}
	f := byPort[443]
	if f.Proto != "tcp" || f.IP != "93.184.216.34" || f.Network != "Wi-Fi" || !f.Active || f.Game || f.In != 1000 || f.Out != 200 {
		t.Fatalf("bad tcp row: %+v", f)
	}
	u := byPort[7500]
	if u.Proto != "udp" || !u.Game || u.Network != "Mobile data" || u.Out != 64 || u.In != 0 {
		t.Fatalf("bad udp row: %+v", u)
	}
	// most recently active first
	time.Sleep(2 * time.Millisecond)
	g.addIn(10)
	if got := b.Flows(0); got[0].Port != 7500 {
		t.Fatalf("most recent activity must be first, got %+v", got[0])
	}
}

func TestFlowLogCloseMovesToFinished(t *testing.T) {
	b := newFlowBalancer()
	a := b.track("tcp", "1.1.1.1", 443, "Wi-Fi", false)
	c := b.track("tcp", "8.8.8.8", 443, "Wi-Fi", false)
	b.untrack(a)
	b.untrack(a) // twice is harmless
	got := b.Flows(0)
	if len(got) != 2 || got[0].ID != c.id || !got[0].Active || got[1].ID != a.id || got[1].Active || got[1].Ended == 0 {
		t.Fatalf("open first, finished after: %+v", got)
	}
	b.untrack(c)
	if got := b.Flows(0); got[0].ID != c.id { // finished most recently first
		t.Fatalf("finished flows newest first: %+v", got)
	}
	b.ClearFlows()
	if got := b.Flows(0); len(got) != 0 {
		t.Fatalf("clear must forget finished flows: %+v", got)
	}
}

func TestFlowLogBounds(t *testing.T) {
	b := newFlowBalancer()
	for i := 0; i < maxClosedFlows+50; i++ {
		b.untrack(b.track("udp", "9.9.9.9", 53, "Wi-Fi", false))
	}
	if got := b.Flows(0); len(got) != maxClosedFlows {
		t.Fatalf("finished flows must be capped at %d, got %d", maxClosedFlows, len(got))
	}
	if got := b.Flows(5); len(got) != 5 {
		t.Fatalf("limit not applied: %d", len(got))
	}
	// a full table stops listing new flows but must not fail or crash
	b2 := newFlowBalancer()
	for i := 0; i < maxTrackedFlows; i++ {
		b2.track("tcp", "1.2.3.4", 80, "Wi-Fi", false)
	}
	extra := b2.track("tcp", "1.2.3.4", 81, "Wi-Fi", false)
	if extra != nil {
		t.Fatal("table full: expected nil flow")
	}
	extra.addIn(5) // nil-safe
	extra.addOut(5)
	b2.untrack(extra)
}

func TestFlowLogDropsOldFinished(t *testing.T) {
	b := newFlowBalancer()
	f := b.track("tcp", "1.1.1.1", 443, "Wi-Fi", false)
	b.untrack(f)
	f.ended.Store(time.Now().Add(-closedFlowTTL - time.Minute).UnixNano())
	if got := b.Flows(0); len(got) != 0 {
		t.Fatalf("old finished flow must be dropped: %+v", got)
	}
}

func TestIsGameFlow(t *testing.T) {
	b := newFlowBalancer()
	b.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000-8000"), TCP: ports("5222")})
	if !b.isGameFlow(true, 7500) || b.isGameFlow(false, 7500) || !b.isGameFlow(false, 5222) || b.isGameFlow(true, 443) {
		t.Fatal("game flag must follow the per-protocol port sets")
	}
	b.SetSplit(SplitConfig{Enabled: false, UDP: ports("7000-8000")})
	if b.isGameFlow(true, 7500) {
		t.Fatal("no game flows while split mode is off")
	}
}
