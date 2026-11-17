package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	ffUDP = "- AND,((PROCESS-NAME,com.dts.freefireth),(NETWORK,udp)),MOBILE"
	ffTCP = "  - AND,((PROCESS-NAME,com.dts.freefireth),(NETWORK,tcp)),WIFI"
)

// fakeResolver answers by source port, and counts how often it was asked.
func fakeResolver(calls *atomic.Int32, byPort map[int]string) func(FlowKey) string {
	return func(k FlowKey) string {
		calls.Add(1)
		return byPort[k.SrcPort]
	}
}

func rulesBalancer(t *testing.T, text string, strict bool) (*Balancer, *atomic.Int32) {
	t.Helper()
	rules, err := ParseRules(text)
	if err != nil {
		t.Fatal(err)
	}
	b := newFlowBalancer()
	b.SetRules(&RuleSet{Rules: rules, Strict: strict})
	var calls atomic.Int32
	b.SetResolver(fakeResolver(&calls, map[int]string{
		1000: "com.dts.freefireth",
		2000: "com.instagram.android",
		3000: "com.shared.a|com.dts.freefireth",
	}))
	return b, &calls
}

func net1(b *Balancer, k FlowKey) string {
	r := b.decide(b.newCtx(k))
	if r == nil {
		return "default"
	}
	if r.deny {
		return "reject"
	}
	sl := b.acquire(Fam4, map[string]struct{}{}, r)
	if sl == nil {
		return "none"
	}
	return sl.Name
}

func TestRulesFromTheRequest(t *testing.T) {
	b, _ := rulesBalancer(t, ffUDP+"\n"+ffTCP+"\n", false)
	ff := func(udp bool) FlowKey { return FlowKey{UDP: udp, SrcPort: 1000, DstIP: "203.0.113.7", DstPort: 7500} }
	if got := net1(b, ff(true)); got != "wwan0" {
		t.Fatalf("Free Fire UDP must use mobile data, got %s", got)
	}
	if got := net1(b, ff(false)); got != "wlan0" {
		t.Fatalf("Free Fire TCP must use Wi-Fi, got %s", got)
	}
	other := FlowKey{UDP: true, SrcPort: 2000, DstIP: "1.1.1.1", DstPort: 443}
	if got := net1(b, other); got != "default" {
		t.Fatalf("another app is not touched, got %s", got)
	}
	// An app sharing a user id with the game matches too.
	shared := FlowKey{UDP: true, SrcPort: 3000, DstPort: 7500}
	if got := net1(b, shared); got != "wwan0" {
		t.Fatalf("shared uid must match, got %s", got)
	}
	// Unknown app: no PROCESS-NAME rule matches.
	if got := net1(b, FlowKey{UDP: true, SrcPort: 9999, DstPort: 7500}); got != "default" {
		t.Fatalf("unknown app must not match, got %s", got)
	}
}

func TestRulesRejectAndOrder(t *testing.T) {
	b, _ := rulesBalancer(t, `
# the game may use mobile data, everything else from this app is refused
PROCESS-NAME,com.dts.freefireth,MOBILE   # comment after a rule
PROCESS-NAME,com.dts.freefireth,REJECT
AND,((PROCESS-NAME,com.instagram.android),(NETWORK,udp)),REJECT
NETWORK,udp,DIRECT
`, false)
	if got := net1(b, FlowKey{SrcPort: 1000, DstPort: 443}); got != "wwan0" {
		t.Fatalf("first match wins, got %s", got)
	}
	if got := net1(b, FlowKey{UDP: true, SrcPort: 2000, DstPort: 443}); got != "reject" {
		t.Fatalf("instagram UDP must be rejected, got %s", got)
	}
	if got := net1(b, FlowKey{SrcPort: 2000, DstPort: 443}); got != "default" {
		t.Fatalf("instagram TCP is not matched, got %s", got)
	}
	// DIRECT stops the search and hands over to split mode / balancing.
	if got := net1(b, FlowKey{UDP: true, SrcPort: 9999, DstPort: 5000}); got != "default" {
		t.Fatalf("DIRECT must give default routing, got %s", got)
	}
}

func TestRulesConditions(t *testing.T) {
	cases := []struct {
		rule string
		k    FlowKey
		want string
	}{
		{"DST-PORT,7000-8000,MOBILE", FlowKey{DstPort: 7500}, "wwan0"},
		{"DST-PORT,7000-8000,MOBILE", FlowKey{DstPort: 9000}, "default"},
		{"IP-CIDR,203.0.113.0/24,MOBILE", FlowKey{DstIP: "203.0.113.9", DstPort: 1}, "wwan0"},
		{"IP-CIDR,203.0.113.0/24,MOBILE,no-resolve", FlowKey{DstIP: "198.51.100.9", DstPort: 1}, "default"},
		{"IP-CIDR,203.0.113.9,REJECT", FlowKey{DstIP: "203.0.113.9"}, "reject"},
		{"IP-CIDR,2001:db8::/32,REJECT", FlowKey{DstIP: "2001:db8::5"}, "reject"},
		{"NETWORK,tcp,WIFI", FlowKey{}, "wlan0"},
		{"NETWORK,udp,WIFI", FlowKey{}, "default"},
		{"OR,((NETWORK,udp),(DST-PORT,443)),REJECT", FlowKey{DstPort: 443}, "reject"},
		{"OR,((NETWORK,udp),(DST-PORT,443)),REJECT", FlowKey{DstPort: 80}, "default"},
		{"NOT,((NETWORK,udp)),WIFI", FlowKey{}, "wlan0"},
		{"NOT,((NETWORK,udp)),WIFI", FlowKey{UDP: true}, "default"},
		{"AND,((NETWORK,udp),(OR,((DST-PORT,7000),(DST-PORT,7001)))),MOBILE", FlowKey{UDP: true, DstPort: 7001}, "wwan0"},
		{"MATCH,REJECT", FlowKey{DstPort: 1234}, "reject"},
		{"PROCESS-NAME,com.dts.*,MOBILE", FlowKey{SrcPort: 1000}, "wwan0"},
		{"PROCESS-NAME,COM.DTS.FREEFIRETH,MOBILE", FlowKey{SrcPort: 1000}, "wwan0"},
		{"and,((process-name,com.dts.freefireth),(network,udp)),cellular", FlowKey{UDP: true, SrcPort: 1000}, "wwan0"},
		{"AND,(PROCESS-NAME,com.dts.freefireth),(NETWORK,udp),DATA", FlowKey{UDP: true, SrcPort: 1000}, "wwan0"},
		{`"PROCESS-NAME,com.instagram.android,wifi"`, FlowKey{SrcPort: 2000}, "wlan0"},
	}
	for _, c := range cases {
		b, _ := rulesBalancer(t, c.rule, false)
		if got := net1(b, c.k); got != c.want {
			t.Errorf("%s on %+v = %s, want %s", c.rule, c.k, got, c.want)
		}
	}
}

func TestRulesParseErrors(t *testing.T) {
	bad := map[string]string{
		"PROCESS-NAME,com.x":                          "a type, a value and an action",
		"PROCESS-NAME,com.x,FLY":                      "unknown action",
		"NETWORK,icmp,WIFI":                           "tcp or udp",
		"DST-PORT,abc,WIFI":                           "not a port",
		"IP-CIDR,notanip,WIFI":                        "address range",
		"DOMAIN-SUFFIX,example.com,REJECT":            "IP addresses, not names",
		"GEOIP,ID,DIRECT":                             "not supported",
		"FOO,bar,WIFI":                                "unknown rule type",
		"AND,((NETWORK,udp),(NETWORK,tcp),WIFI":       "unbalanced",
		"AND,(),WIFI":                                 "empty",
		"AND,WIFI":                                    "needs conditions",
		"NOT,((NETWORK,udp),(NETWORK,tcp)),WIFI":      "exactly one",
		"AND,((PROCESS-NAME,a,b),(NETWORK,udp)),WIFI": "exactly one value",
	}
	for rule, want := range bad {
		_, err := ParseRules("NETWORK,udp,WIFI\n" + rule)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%q: got %v, want an error with %q on line 2", rule, err, want)
		}
	}
	if rs, err := ParseRules("\n# only comments\n   \n"); err != nil || len(rs) != 0 {
		t.Errorf("comments only: %v %v", rs, err)
	}
}

func TestRulesLazyProcessLookup(t *testing.T) {
	// Rules that never ask for the app must not cost a lookup.
	b, calls := rulesBalancer(t, "NETWORK,udp,WIFI\nDST-PORT,443,REJECT", false)
	net1(b, FlowKey{UDP: true, SrcPort: 1000})
	net1(b, FlowKey{SrcPort: 1000, DstPort: 443})
	if calls.Load() != 0 {
		t.Fatalf("resolver called %d times with no PROCESS-NAME rule", calls.Load())
	}
	if b.Rules().NeedsProcess() {
		t.Fatal("NeedsProcess must be false")
	}
	// One lookup per flow even when several rules ask, and none after the flow was decided early.
	b, calls = rulesBalancer(t, "PROCESS-NAME,a,WIFI\nPROCESS-NAME,b,WIFI\nPROCESS-NAME,c,REJECT", false)
	net1(b, FlowKey{SrcPort: 1000})
	if calls.Load() != 1 || !b.Rules().NeedsProcess() {
		t.Fatalf("want one lookup, got %d", calls.Load())
	}
}

func TestRulesStrictAndFallback(t *testing.T) {
	b, _ := rulesBalancer(t, "PROCESS-NAME,com.dts.freefireth,MOBILE", false)
	b.S.SetState(b.S.slots[1], Fam4, false) // modem has no internet
	k := FlowKey{UDP: true, SrcPort: 1000, DstPort: 7500}
	if got := net1(b, k); got != "wlan0" {
		t.Fatalf("non-strict rule must fall back to Wi-Fi, got %s", got)
	}
	b, _ = rulesBalancer(t, "PROCESS-NAME,com.dts.freefireth,MOBILE", true)
	b.S.SetState(b.S.slots[1], Fam4, false)
	if got := net1(b, k); got != "wwan0" {
		t.Fatalf("strict rule must stay on mobile data, got %s", got)
	}
	// With no mobile network at all, strict gives nothing to use.
	b2 := NewBalancer(NewScheduler(nil))
	rules, _ := ParseRules("NETWORK,udp,MOBILE")
	b2.SetRules(&RuleSet{Rules: rules, Strict: true})
	if got := net1(b2, FlowKey{UDP: true}); got != "none" {
		t.Fatalf("got %s", got)
	}
}

func TestRulesWinOverSplitAndFlowLog(t *testing.T) {
	b, _ := rulesBalancer(t, "PROCESS-NAME,com.instagram.android,REJECT", false)
	b.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000-8000")})
	b.WantProcesses(true)

	// The rule rejects it even though nothing about split mode would.
	_, err := b.DialPinnedFlow(context.Background(), FlowKey{SrcPort: 2000, DstIP: "93.184.216.34", DstPort: 443})
	if !errors.Is(err, errBlocked) {
		t.Fatalf("want errBlocked, got %v", err)
	}
	pc, err := b.ListenUDPFlow(context.Background(), Fam4, FlowKey{SrcPort: 2000, DstIP: "203.0.113.9", DstPort: 9999})
	if pc != nil || !errors.Is(err, errBlocked) {
		t.Fatalf("UDP must be refused, got %v %v", pc, err)
	}
	rows := b.Flows(0)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}
	for _, r := range rows {
		if r.Network != blockedNetwork || r.Process != "com.instagram.android" || r.Rule != "rule 1: REJECT" || r.Game {
			t.Fatalf("bad row: %+v", r)
		}
	}
	// Another app falls through to split mode untouched.
	if r := b.decide(b.newCtx(FlowKey{UDP: true, SrcPort: 1000, DstPort: 7500})); r == nil || r.deny {
		t.Fatalf("split mode must still route game ports, got %+v", r)
	}
	// Rules off: nothing is rejected.
	b.SetRules(nil)
	if r := b.decide(b.newCtx(FlowKey{SrcPort: 2000, DstPort: 443})); r != nil && r.deny {
		t.Fatal("no rules, no rejection")
	}
}

func TestMonitorShowsProcessOnlyWhenWanted(t *testing.T) {
	b, calls := rulesBalancer(t, "NETWORK,udp,DIRECT", false)
	fc := b.newCtx(FlowKey{SrcPort: 1000})
	if m := fc.meta(false); m.process != "" || calls.Load() != 0 {
		t.Fatalf("no lookup while the monitor is closed: %+v, %d calls", m, calls.Load())
	}
	b.WantProcesses(true)
	fc = b.newCtx(FlowKey{SrcPort: 1000})
	if m := fc.meta(false); m.process != "com.dts.freefireth" || calls.Load() != 1 {
		t.Fatalf("lookup while the monitor is open: %+v, %d calls", m, calls.Load())
	}
}
