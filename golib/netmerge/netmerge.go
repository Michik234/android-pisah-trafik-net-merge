// Package netmerge is the gomobile-facing API of the Android port. The Kotlin side (VpnService)
// owns the TUN, tracks the available networks and calls in here; everything about picking a
// network for each flow lives in the engine copied from upstream net-merge.
package netmerge

import (
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"netmerge-android/internal/engine"
	"netmerge-android/internal/nics"
	"netmerge-android/internal/tun"
)

var (
	mu      sync.Mutex
	sched   = engine.NewScheduler(nil)
	bal     = engine.NewBalancer(sched)
	dev     tun.Tun
	stop    chan struct{}
	indexes = map[string]int{} // network name -> stable index
	last    []nics.Nic         // last list given to the scheduler, to re-apply weights
	lastJS  string             // last SetNetworks input, re-applied when the mode changes
	auto    = true
)

func init() { sched.SetAuto(true) }

// network is what Kotlin reports for each usable network.
type network struct {
	Name   string  `json:"name"`
	Handle string  `json:"handle"` // Network#getNetworkHandle, as a decimal string (it is 64-bit)
	V4     bool    `json:"v4"`
	V6     bool    `json:"v6"`
	Weight float64 `json:"weight"`
	// The system's bandwidth estimate (NetworkCapabilities link bandwidth), kbit/s; 0 = unknown.
	DownKbps float64 `json:"downKbps"`
	// True for mobile data. Split mode sends game flows over these.
	Cellular bool `json:"cellular"`
}

// SetNetworks replaces the set of networks flows may use. It may be called at any time, before
// or after Start. The argument is a JSON array of {name, handle, v4, v6, weight}.
func SetNetworks(js string) error {
	var in []network
	if err := json.Unmarshal([]byte(js), &in); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	var maxDown float64
	for _, n := range in {
		maxDown = max(maxDown, n.DownKbps)
	}
	list := make([]nics.Nic, 0, len(in))
	for _, n := range in {
		h, err := strconv.ParseUint(n.Handle, 10, 64)
		if err != nil {
			return errors.New("bad network handle " + n.Handle)
		}
		idx, ok := indexes[n.Name]
		if !ok {
			idx = len(indexes) + 1
			indexes[n.Name] = idx
		}
		nics.SetHandle(idx, h)
		w := n.Weight
		if w <= 0 {
			w = 1
			// With speed-based balancing, start from the system's estimate (relative to the fastest
			// network, floored so a slow one is still tried and measured).
			if auto && maxDown > 0 && n.DownKbps > 0 {
				w = max(0.1, n.DownKbps/maxDown)
			}
		}
		nic := nics.Nic{Name: n.Name, Index: idx, Weight: w, Cellular: n.Cellular}
		// Unspecified source addresses: the Network binding chooses the real source.
		if n.V4 {
			nic.IP = net.IPv4zero
		}
		if n.V6 {
			nic.IP6 = net.IPv6unspecified
		}
		list = append(list, nic)
	}
	sched.Update(list)
	last, lastJS = list, js
	// Flows on a network that just went away would otherwise hang until they time out.
	sched.CloseGone()
	return nil
}

// SetAutoWeights picks how new connections are split. true (the default) follows each network's
// measured speed; false splits them evenly, which helps when the networks' upload speeds are
// closer than their download speeds.
func SetAutoWeights(on bool) {
	mu.Lock()
	auto = on
	sched.SetAuto(on)
	js, list := lastJS, last
	mu.Unlock()
	if js != "" {
		SetNetworks(js) // recompute the configured weights for the new mode
	} else {
		sched.Update(list)
	}
}

// Start begins serving the VPN's TUN. fd must be a detached descriptor (ParcelFileDescriptor
// #detachFd); the engine owns and closes it.
func Start(fd int, mtu int) error {
	mu.Lock()
	defer mu.Unlock()
	if stop != nil {
		close(stop)
		stop = nil
	}
	if err := dev.Start(bal, fd, mtu); err != nil {
		return err
	}
	stop = make(chan struct{})
	go sched.RunProbes(stop)
	go tick(stop)
	return nil
}

func tick(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			sched.Tick(now.Sub(last))
			last = now
		}
	}
}

// Stop tears the stack down and closes the TUN descriptor.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if stop != nil {
		close(stop)
		stop = nil
	}
	dev.Stop()
}

// IsRunning reports whether the engine is serving a TUN.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return stop != nil
}

// SetIPv6Mode sets how IPv6 flows are handled: "auto", "on" or "off" (see upstream).
func SetIPv6Mode(mode string) error {
	m, ok := engine.ParseV6Mode(mode)
	if !ok {
		return errors.New("IPv6 mode must be auto, on or off")
	}
	bal.SetV6Mode(m)
	return nil
}

// SetSplit turns split mode on or off and sets its ports. In split mode, connections to the game
// ports leave over mobile data while everything else uses Wi-Fi, both at the same time. With
// gameOnWifi it is the other way round: the game uses Wi-Fi and everything else mobile data.
// With blockOthers every other connection is rejected (like REJECT in Clash) so the network serves
// the game alone; DNS stays allowed when allowDNS is true. Ports are
// lists like "7000-8000,3074" ("" or "none" = none, "all" = every port except 53, 123, 443, 5353).
// udpPorts and tcpPorts are destination ports. With strict, a side never borrows the other one's
// network when its own is down. Applies to new connections.
func SetSplit(enabled bool, udpPorts, tcpPorts string, strict, gameOnWifi, blockOthers, allowDNS bool) error {
	udp, tcp, err := engine.SplitPorts(udpPorts, tcpPorts)
	if err != nil {
		return err
	}
	bal.SetSplit(engine.SplitConfig{Enabled: enabled, UDP: udp, TCP: tcp, Strict: strict, GameWifi: gameOnWifi, BlockOthers: blockOthers, BlockDNS: !allowDNS})
	return nil
}

// NormalizeSplitPorts reads the port boxes the way SetSplit does and returns what it understood as
// JSON {"udp": "...", "tcp": "..."}, with ranges merged and sorted ("none" when empty), or the
// error to show the user. Ports can be pasted as written anywhere: "UDP: 7086–41192, 10010, 10013",
// one per line, with en dashes, or a whole "UDP: ... TCP: ..." block in either box.
func NormalizeSplitPorts(udpPorts, tcpPorts string) (string, error) {
	udp, tcp, err := engine.SplitPorts(udpPorts, tcpPorts)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]string{"udp": udp.String(), "tcp": tcp.String()})
	return string(b), nil
}

// ProcessResolver is implemented by the Kotlin side: it tells which app owns a connection
// (ConnectivityManager.getConnectionOwnerUid, Android 10+). It returns the package name, several
// joined with "|" when apps share a user id, or "" when it cannot tell.
type ProcessResolver interface {
	Name(udp bool, srcIP string, srcPort int, dstIP string, dstPort int) string
}

// SetProcessResolver installs the app lookup used by PROCESS-NAME rules and the live list. nil
// removes it.
func SetProcessResolver(r ProcessResolver) {
	if r == nil {
		bal.SetResolver(nil)
		return
	}
	bal.SetResolver(func(k engine.FlowKey) string {
		return r.Name(k.UDP, k.SrcIP, k.SrcPort, k.DstIP, k.DstPort)
	})
}

// SetRules sets the Clash-style rules, one per line, for example
//
//	AND,((PROCESS-NAME,com.dts.freefireth),(NETWORK,udp)),MOBILE
//	AND,((PROCESS-NAME,com.dts.freefireth),(NETWORK,tcp)),WIFI
//
// Actions: MOBILE, WIFI, REJECT, DIRECT. The first matching rule wins; a connection that matches
// none follows split mode and the normal balancing. With strict, a MOBILE or WIFI rule never uses
// the other network when its own is down. The text is checked even when enabled is false, and an
// error says which line is wrong; nothing changes then. Applies to new connections.
func SetRules(enabled bool, text string, strict bool) error {
	rules, err := engine.ParseRules(text)
	if err != nil {
		return err
	}
	if !enabled || len(rules) == 0 {
		bal.SetRules(nil)
		return nil
	}
	bal.SetRules(&engine.RuleSet{Rules: rules, Strict: strict})
	return nil
}

// SetMonitorProcesses makes the live list name the app of every connection (not only those a rule
// asks about). The UI turns it on while the list is open.
func SetMonitorProcesses(on bool) { bal.WantProcesses(on) }

// Flows returns the live connection list as JSON: up to limit TCP connections and UDP sessions,
// open ones first, each with its destination IP and port, the network it uses, whether it is a
// game flow (split mode), and its byte counts. Only destinations are kept, in memory, never
// contents, and never written anywhere.
func Flows(limit int) string {
	b, _ := json.Marshal(bal.Flows(limit))
	return string(b)
}

// ClearFlows forgets the finished connections from the list.
func ClearFlows() { bal.ClearFlows() }

// SplitState describes split mode in words, for the UI.
func SplitState() string { return bal.SplitState() }

type stat struct {
	Name     string  `json:"name"`
	State    string  `json:"state"`
	State6   string  `json:"state6"`
	Active   int64   `json:"active"`
	Total    int64   `json:"total"`
	Failed   int64   `json:"failed"`
	BytesIn  int64   `json:"bytesIn"`
	BytesOut int64   `json:"bytesOut"`
	RateIn   float64 `json:"rateIn"`
	RateOut  float64 `json:"rateOut"`
	Weight   float64 `json:"weight"`
	Present  bool    `json:"present"`
}

// Stats returns a JSON array with one entry per network: probe state, flows and throughput.
func Stats() string {
	snap := sched.Snapshot()
	out := make([]stat, 0, len(snap))
	for _, s := range snap {
		out = append(out, stat{
			Name: s.Name, State: s.State, State6: s.State6,
			Active: s.Active, Total: s.Total, Failed: s.Failed,
			BytesIn: s.BytesIn, BytesOut: s.BytesOut, RateIn: s.RateIn, RateOut: s.RateOut,
			Weight: s.Weight, Present: s.Present,
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}
