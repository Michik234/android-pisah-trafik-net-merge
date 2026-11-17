package engine

import (
	"fmt"
	"net/netip"
	"path"
	"strings"
	"sync/atomic"
)

// Rules pick a network (or a refusal) for a flow by what it is, in the syntax of Clash's rules, so a
// rule can be pasted as it is:
//
//	AND,((PROCESS-NAME,com.dts.freefireth),(NETWORK,udp)),MOBILE
//	PROCESS-NAME,com.dts.freefireth,WIFI
//	NETWORK,udp,REJECT
//
// They are checked top to bottom, the first match wins, and a flow that matches none (or matches a
// DIRECT rule) is handled by split mode and the normal balancing. The process is the app that owns
// the connection; the Kotlin side finds it (see SetResolver) and only when a rule asks for it.

// RuleAction is what a matching rule does with the flow.
type RuleAction int

const (
	ActDefault RuleAction = iota // leave it to split mode / normal balancing
	ActMobile                    // use mobile data
	ActWifi                      // use Wi-Fi (any network that is not cellular)
	ActReject                    // refuse it
)

func (a RuleAction) String() string {
	return [...]string{"DIRECT", "MOBILE", "WIFI", "REJECT"}[a]
}

func parseAction(s string) (RuleAction, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "MOBILE", "CELLULAR", "DATA", "MOBILE-DATA", "4G", "5G", "LTE":
		return ActMobile, nil
	case "WIFI", "WI-FI", "WLAN":
		return ActWifi, nil
	case "REJECT", "BLOCK", "DROP", "REJECT-DROP":
		return ActReject, nil
	case "DIRECT", "DEFAULT", "AUTO":
		return ActDefault, nil
	}
	return 0, fmt.Errorf("unknown action %q (use MOBILE, WIFI, REJECT or DIRECT)", s)
}

// FlowKey describes one new flow as the TUN saw it.
type FlowKey struct {
	UDP     bool
	SrcIP   string // the app's side: the VPN's address
	SrcPort int
	DstIP   string
	DstPort int
}

// Rule is one parsed line.
type Rule struct {
	Text   string
	Action RuleAction
	match  func(*flowCtx) bool
	proc   bool // asks which app owns the connection
}

// RuleSet is the rules plus how they fall back.
type RuleSet struct {
	Rules []Rule
	// Strict forbids falling back: a MOBILE rule never uses Wi-Fi when mobile data is down, and the
	// other way round.
	Strict bool
}

// NeedsProcess reports whether any rule asks for the app that owns the connection.
func (rs *RuleSet) NeedsProcess() bool {
	for i := range rs.Rules {
		if rs.Rules[i].proc {
			return true
		}
	}
	return false
}

// flowCtx is a flow while it is being decided. The process is looked up the first time it is
// needed and not before, so rules that do not ask for it cost nothing.
type flowCtx struct {
	b        *Balancer
	k        FlowKey
	proc     string
	procDone bool
	rule     string // which rule decided it, for the monitor
}

func (b *Balancer) newCtx(k FlowKey) *flowCtx { return &flowCtx{b: b, k: k} }

func (c *flowCtx) process() string {
	if !c.procDone {
		c.procDone = true
		if f := c.b.resolver.Load(); f != nil {
			c.proc = (*f)(c.k)
		}
	}
	return c.proc
}

// meta is what the flow list records. The app is looked up for it only while the monitor is open.
func (c *flowCtx) meta(game bool) flowMeta {
	m := flowMeta{rule: c.rule, game: game && c.rule == ""}
	if c.procDone {
		m.process = c.proc
	} else if c.b.wantProc.Load() {
		m.process = c.process()
	}
	return m
}

// SetResolver installs the function that tells which app owns a connection ("" = unknown; several
// packages sharing one user id are joined with "|"). nil removes it.
func (b *Balancer) SetResolver(f func(FlowKey) string) {
	if f == nil {
		b.resolver.Store(nil)
		return
	}
	b.resolver.Store(&f)
}

// WantProcesses makes every new flow carry its app in the monitor, not just the ones a rule asks about.
func (b *Balancer) WantProcesses(on bool) { b.wantProc.Store(on) }

// SetRules replaces the rules; nil turns them off. It applies to flows opened from now on.
func (b *Balancer) SetRules(rs *RuleSet) { b.rules.p.Store(rs) }

// Rules returns the active rules, or nil.
func (b *Balancer) Rules() *RuleSet { return b.rules.p.Load() }

type rulesHolder struct{ p atomic.Pointer[RuleSet] }

func procMatch(pattern, procs string) bool {
	for _, p := range strings.Split(procs, "|") {
		if p == "" {
			continue
		}
		if strings.EqualFold(pattern, p) {
			return true
		}
		if strings.ContainsAny(pattern, "*?") {
			if ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(p)); ok {
				return true
			}
		}
	}
	return false
}

// decide applies the rules, then split mode. nil means "no restriction".
func (b *Balancer) decide(c *flowCtx) *route {
	if rs := b.rules.p.Load(); rs != nil {
		for i := range rs.Rules {
			r := &rs.Rules[i]
			if !r.match(c) {
				continue
			}
			c.rule = fmt.Sprintf("rule %d: %s", i+1, r.Action)
			switch r.Action {
			case ActReject:
				return &route{deny: true}
			case ActMobile:
				return &route{skip: b.S.Exclude(func(_ string, cellular bool) bool { return cellular }), strict: rs.Strict}
			case ActWifi:
				return &route{skip: b.S.Exclude(func(_ string, cellular bool) bool { return !cellular }), strict: rs.Strict}
			}
			break // DIRECT: no special treatment, and no later rule gets a say
		}
	}
	return b.splitRoute(c.k.UDP, c.k.DstPort)
}

// ParseRules reads rules, one per line. Blank lines and "#" comments are skipped, and a leading
// "- " (as in a Clash YAML list) or surrounding quotes are ignored.
func ParseRules(text string) ([]Rule, error) {
	var out []Rule
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		line = strings.Trim(line, `"'`)
		r, err := parseRule(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func parseRule(line string) (Rule, error) {
	parts, err := splitTop(line)
	if err != nil {
		return Rule{}, err
	}
	for len(parts) > 2 && strings.EqualFold(parts[len(parts)-1], "no-resolve") {
		parts = parts[:len(parts)-1]
	}
	typ := strings.ToUpper(parts[0])
	if len(parts) < 2 || (len(parts) == 2 && !strings.Contains(" MATCH FINAL AND OR NOT ", " "+typ+" ")) {
		return Rule{}, fmt.Errorf("a rule needs a type, a value and an action, like PROCESS-NAME,com.example.app,WIFI")
	}
	act, err := parseAction(parts[len(parts)-1])
	if err != nil {
		return Rule{}, err
	}
	m, proc, err := parseCondition(parts[:len(parts)-1])
	if err != nil {
		return Rule{}, err
	}
	return Rule{Text: line, Action: act, match: m, proc: proc}, nil
}

// splitTop splits at the commas that are not inside parentheses.
func splitTop(s string) ([]string, error) {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced parentheses in %q", s)
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses in %q", s)
	}
	return append(out, strings.TrimSpace(s[start:])), nil
}

// unwrap removes one pair of parentheses that encloses the whole string.
func unwrap(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return s
	}
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return s // the first "(" closes early: "(a),(b)"
			}
		}
	}
	return strings.TrimSpace(s[1 : len(s)-1])
}

func always(*flowCtx) bool { return true }

// parseCondition turns "TYPE,arg..." into a matcher. proc reports that it asks for the app.
func parseCondition(body []string) (m func(*flowCtx) bool, proc bool, err error) {
	if len(body) == 0 || body[0] == "" {
		return nil, false, fmt.Errorf("empty condition")
	}
	typ := strings.ToUpper(body[0])
	arg := func() (string, error) {
		if len(body) != 2 || body[1] == "" {
			return "", fmt.Errorf("%s needs exactly one value, like %s,value", typ, typ)
		}
		return body[1], nil
	}
	switch typ {
	case "MATCH", "FINAL":
		return always, false, nil

	case "AND", "OR", "NOT":
		subs, err := parseList(typ, body[1:])
		if err != nil {
			return nil, false, err
		}
		var ms []func(*flowCtx) bool
		for _, s := range subs {
			sm, sp, err := parseCondition(s)
			if err != nil {
				return nil, false, err
			}
			ms = append(ms, sm)
			proc = proc || sp
		}
		if typ == "NOT" {
			if len(ms) != 1 {
				return nil, false, fmt.Errorf("NOT takes exactly one condition")
			}
			return func(c *flowCtx) bool { return !ms[0](c) }, proc, nil
		}
		and := typ == "AND"
		return func(c *flowCtx) bool {
			for _, f := range ms {
				if f(c) != and { // AND stops at the first false, OR at the first true
					return !and
				}
			}
			return and
		}, proc, nil

	case "PROCESS-NAME":
		v, err := arg()
		if err != nil {
			return nil, false, err
		}
		return func(c *flowCtx) bool { return procMatch(v, c.process()) }, true, nil

	case "NETWORK":
		v, err := arg()
		if err != nil {
			return nil, false, err
		}
		switch strings.ToLower(v) {
		case "udp":
			return func(c *flowCtx) bool { return c.k.UDP }, false, nil
		case "tcp":
			return func(c *flowCtx) bool { return !c.k.UDP }, false, nil
		}
		return nil, false, fmt.Errorf("NETWORK must be tcp or udp, not %q", v)

	case "DST-PORT":
		v, err := arg()
		if err != nil {
			return nil, false, err
		}
		ports, err := ParsePorts(v)
		if err != nil || ports.Empty() {
			return nil, false, fmt.Errorf("DST-PORT %q: not a port or range", v)
		}
		return func(c *flowCtx) bool { return ports.Has(c.k.DstPort) }, false, nil

	case "IP-CIDR", "IP-CIDR6":
		v, err := arg()
		if err != nil {
			return nil, false, err
		}
		pfx, err := netip.ParsePrefix(v)
		if err != nil {
			a, aerr := netip.ParseAddr(v)
			if aerr != nil {
				return nil, false, fmt.Errorf("%s %q: not an address range like 203.0.113.0/24", typ, v)
			}
			pfx = netip.PrefixFrom(a, a.BitLen())
		}
		return func(c *flowCtx) bool {
			a, err := netip.ParseAddr(c.k.DstIP)
			return err == nil && pfx.Contains(a.Unmap())
		}, false, nil

	case "DOMAIN", "DOMAIN-SUFFIX", "DOMAIN-KEYWORD", "DOMAIN-REGEX", "GEOSITE":
		return nil, false, fmt.Errorf("%s is not supported: net-merge sees IP addresses, not names", typ)
	case "GEOIP", "RULE-SET", "PROCESS-PATH", "PROCESS-NAME-REGEX", "SRC-IP-CIDR", "SRC-PORT", "IN-PORT", "SUB-RULE":
		return nil, false, fmt.Errorf("%s is not supported (use PROCESS-NAME, NETWORK, DST-PORT, IP-CIDR, AND, OR, NOT, MATCH)", typ)
	}
	return nil, false, fmt.Errorf("unknown rule type %q (use PROCESS-NAME, NETWORK, DST-PORT, IP-CIDR, AND, OR, NOT, MATCH)", body[0])
}

// parseList reads the conditions of AND/OR/NOT, in Clash's form "((A),(B))" or "(A),(B)".
func parseList(typ string, rest []string) ([][]string, error) {
	if len(rest) == 0 {
		return nil, fmt.Errorf("%s needs conditions, like %s,((PROCESS-NAME,com.example.app),(NETWORK,udp))", typ, typ)
	}
	items := rest
	if len(rest) == 1 {
		inner := unwrap(rest[0])
		parts, err := splitTop(inner)
		if err != nil {
			return nil, err
		}
		items = parts
		for _, p := range parts { // "PROCESS-NAME,x" on its own is one condition, "(a),(b)" is a list
			if !strings.HasPrefix(p, "(") {
				return [][]string{parts}, nil
			}
		}
	}
	var out [][]string
	for _, it := range items {
		parts, err := splitTop(unwrap(it))
		if err != nil {
			return nil, err
		}
		out = append(out, parts)
	}
	return out, nil
}
