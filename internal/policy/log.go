package policy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stage names the point at which a decision was taken. A destination is checked more than
// once on the way out, and knowing which check refused matters when debugging: a `sni`
// denial after a `connect` allow means the client asked for one host and then spoke to
// another.
type Stage string

const (
	// StageHTTP is a plaintext HTTP request through the forward proxy.
	StageHTTP Stage = "http"
	// StageConnect is the target of an HTTP CONNECT request.
	StageConnect Stage = "connect"
	// StageSNI is the server name inside the TLS ClientHello carried by a tunnel.
	StageSNI Stage = "sni"
	// StageDial is the address a permitted hostname resolved to.
	StageDial Stage = "dial"
	// StageRequest is a request read inside a flow Boks terminated. It exists only for
	// inspected flows, because it is the one thing a blind tunnel cannot show.
	StageRequest Stage = "request"
	// StageNetwork is a connection judged in the host-side network stack, before it was
	// dialled — a flow that never used the proxy at all. It carries no hostname, because
	// the guest put an address in a SYN and nothing else; the destination is whatever the
	// packet said. This is the stage that decides for a guest that is not cooperating.
	StageNetwork Stage = "network"
)

// Mode records how a flow reached its destination, and therefore how much of it Boks could
// read. It is a transparency guarantee, not a debugging aid: a user must be able to look at
// the log and separate what Boks read from what it merely carried.
//
// The three values and their names are Docker Sandboxes' own, taken from real `sbx policy
// log` output. Matching the reference product's vocabulary is worth more than inventing
// clearer words for the same three things.
type Mode string

const (
	// ModeForward is a flow Boks handled at the HTTP level, and therefore could read.
	// Either plaintext HTTP, where there was never anything to break, or HTTPS that Boks
	// terminated and re-originated — which happens only for a host with a credential
	// rule, because reading the request is the only way to add a header to it.
	ModeForward Mode = "forward"
	// ModeForwardBypass is a CONNECT tunnel spliced byte-for-byte: the flow used the
	// proxy, but bypassed inspection. TLS is end-to-end, the client validated the
	// origin's own certificate chain, and Boks saw ciphertext only. This is the default
	// for every destination without a credential rule.
	ModeForwardBypass Mode = "forward-bypass"
	// ModeTransparent is a flow that never used the proxy and was judged at the network
	// layer instead — the case a raw socket or a non-HTTP protocol such as SSH produces.
	// Only address and port rules can apply there, because a raw connection carries no
	// hostname.
	//
	// This is what the host-side network stack writes (internal/network): every TCP
	// connection the guest opens is judged before it is dialled, whether or not the guest
	// used the proxy. A decision in this mode says Boks saw an address and a port and
	// nothing else — not that it read anything.
	ModeTransparent Mode = "transparent"
)

// Decision is one logged policy outcome. It is the unit `boks policy log` displays and the
// only record Boks keeps of sandbox network activity. It stays on this machine.
//
// The action and resource are recorded as structured strings rather than folded into the
// prose reason, so that the display layer can group, filter and aggregate them, and so that
// a later policy over something other than the network — a filesystem path, an MCP server —
// can be logged in the same shape instead of a second, incompatible one.
type Decision struct {
	Time time.Time `json:"time"`
	// Type is the policy domain. Only "network" exists today.
	Type    string `json:"type"`
	Stage   Stage  `json:"stage"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Allowed bool   `json:"allowed"`
	// Action is the operation that was judged, as "net:connect:tcp".
	Action string `json:"action"`
	// Resource is what it was judged against, as "net:domain:example.com:443" or
	// "net:ip:203.0.113.7:443".
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
	// Rule is the rule that decided, or an explicit statement that none applied.
	Rule   string `json:"rule,omitempty"`
	Policy string `json:"policy"`
	// Mode says how the flow was carried, and so whether Boks could read it. Empty when
	// the disposition was not yet known at the point the decision was taken.
	Mode Mode `json:"mode,omitempty"`
	// Sandbox is the sandbox the flow came from, when the proxy knows it.
	Sandbox string `json:"sandbox,omitempty"`
}

// TypeNetwork is the policy domain every decision Boks takes today belongs to.
const TypeNetwork = "network"

// ActionConnectTCP is the only operation Boks judges so far: opening a TCP connection to a
// destination. It is spelled out rather than implied so that the log does not have to be
// re-interpreted when a second action exists.
const ActionConnectTCP = "net:connect:tcp"

// ResourceOf renders a target as a typed resource string.
func ResourceOf(t Target) string {
	kind := "domain"
	if t.IsIP() {
		kind = "ip"
	}
	return fmt.Sprintf("net:%s:%s:%d", kind, t.Host, t.Port)
}

// NoRuleFor is what the decision log records when nothing matched: the same structured
// action/resource vocabulary a matching rule is reported in, rather than an empty field the
// reader has to interpret.
//
// It is exported because `boks policy check` has to say exactly what the log would say. The
// value of that command is being the same answer the engine gives, and two spellings of "no
// rule matched" would be two answers.
func NoRuleFor(t Target) string {
	return fmt.Sprintf("no applicable policies for op(action=%s, resource=%s)", ActionConnectTCP, ResourceOf(t))
}

func (d Decision) String() string {
	verb := "DENY "
	if d.Allowed {
		verb = "ALLOW"
	}
	mode := string(d.Mode)
	if mode == "" {
		mode = "-"
	}
	return fmt.Sprintf("%s %s %-7s %-14s %s:%d  %s",
		d.Time.Format(time.RFC3339), verb, d.Stage, mode, d.Host, d.Port, d.Reason)
}

// Sink receives decisions as they are made. Implementations must be safe for concurrent
// use and must never block for long: they sit in the path of every request.
type Sink interface {
	Record(Decision)
}

// Log keeps the most recent decisions in memory and fans them out to sinks.
//
// Observability is what makes a policy usable — a denial you cannot see is
// indistinguishable from a bug in your program — so recording is not optional and cannot
// be turned off.
type Log struct {
	mu      sync.Mutex
	entries []Decision
	next    int
	full    bool
	sinks   []Sink
}

// DefaultCapacity is how many decisions a Log keeps in memory.
const DefaultCapacity = 512

// NewLog creates a ring buffer holding capacity decisions.
func NewLog(capacity int) *Log {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Log{entries: make([]Decision, capacity)}
}

// AddSink attaches a sink. Sinks receive every decision recorded after they are added.
func (l *Log) AddSink(s Sink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sinks = append(l.sinks, s)
}

// Record stores a decision and forwards it to every sink.
func (l *Log) Record(d Decision) {
	l.mu.Lock()
	l.entries[l.next] = d
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
	sinks := append([]Sink(nil), l.sinks...)
	l.mu.Unlock()

	for _, s := range sinks {
		s.Record(d)
	}
}

// Recent returns up to n decisions, oldest first. n <= 0 returns everything retained.
func (l *Log) Recent(n int) []Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	size := len(l.entries)
	count := l.next
	if l.full {
		count = size
	}
	out := make([]Decision, 0, count)
	start := 0
	if l.full {
		start = l.next
	}
	for i := 0; i < count; i++ {
		out = append(out, l.entries[(start+i)%size])
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// FileSink appends decisions to a file as JSON lines, so that a proxy running in one
// process and `boks policy log` running in another agree on history.
//
// Secrets never reach a Decision: the struct has no field for a header value or a request
// body, which is a design choice, not an omission. A logger that could print a credential
// would eventually print one.
type FileSink struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

// NewFileSink opens path for appending, creating it and its directory with owner-only
// permissions. The log records where a sandbox tried to connect, which is not something to
// leave world-readable.
func NewFileSink(path string) (*FileSink, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating decision log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening decision log %s: %w", path, err)
	}
	return &FileSink{w: f, c: f}, nil
}

// NewWriterSink writes decisions as JSON lines to w, for tests and for piping.
func NewWriterSink(w io.Writer) *FileSink { return &FileSink{w: w} }

// Record appends one JSON line. Write errors are dropped on purpose: failing to record a
// decision must not fail the request that produced it, and the in-memory log still has it.
func (s *FileSink) Record(d Decision) {
	line, err := json.Marshal(d)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(append(line, '\n'))
}

// Close releases the underlying file, if any.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return nil
	}
	return s.c.Close()
}

// ReadDecisions reads a JSON-lines decision log, returning at most the last n entries.
// Malformed lines are skipped rather than failing the whole read: a log truncated by a
// crash should still be readable.
func ReadDecisions(r io.Reader, n int) ([]Decision, error) {
	dec := json.NewDecoder(r)
	var out []Decision
	for {
		var d Decision
		err := dec.Decode(&d)
		if err == io.EOF {
			break
		}
		if err != nil {
			// A partial trailing line is the common case; stop rather than error.
			break
		}
		out = append(out, d)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// Filter narrows a decision log to the part someone is asking about.
//
// The log is one file for the whole machine, which is right — decisions from a sandbox that
// no longer exists are exactly what you want after a run fails — but it makes the unfiltered
// view a firehose: every sandbox anyone has ever run, back to whenever the file was created.
// Both fields are already on every decision, so this is a display concern rather than a
// change to what is recorded.
type Filter struct {
	// Sandbox keeps only decisions from this sandbox. Empty keeps all of them.
	Sandbox string
	// Since keeps only decisions at or after this instant. The zero time keeps all.
	Since time.Time
}

// Match reports whether a decision survives the filter.
func (f Filter) Match(d Decision) bool {
	if f.Sandbox != "" && d.Sandbox != f.Sandbox {
		return false
	}
	if !f.Since.IsZero() && d.Time.Before(f.Since) {
		return false
	}
	return true
}

// Empty reports whether the filter would keep everything.
func (f Filter) Empty() bool { return f.Sandbox == "" && f.Since.IsZero() }

// Apply returns the decisions the filter keeps, in order.
func (f Filter) Apply(decisions []Decision) []Decision {
	if f.Empty() {
		return decisions
	}
	out := make([]Decision, 0, len(decisions))
	for _, d := range decisions {
		if f.Match(d) {
			out = append(out, d)
		}
	}
	return out
}

// ParseSince reads the two spellings of "how far back": a duration before now ("2h", "90m",
// "45s") and an absolute time (RFC 3339, or a plain date).
//
// A duration is what someone debugging a run reaches for, and an absolute time is what
// someone comparing against another record has. Both are accepted because guessing which one
// a string is meant to be is easy and refusing one of them is not.
//
// A negative duration is taken as its magnitude: "-2h" and "2h" both mean two hours ago,
// since there is no sense in which a decision log has entries in the future.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time: use a duration such as 30m or 2h, "+
		"or a time such as 2026-08-13 or 2026-08-13T09:30:00Z", s)
}

// Aggregate is a set of decisions that were, for a reader's purposes, the same decision:
// one destination, one mode, one outcome, one reason.
type Aggregate struct {
	Sandbox  string
	Type     string
	Host     string
	Port     int
	Mode     Mode
	Allowed  bool
	Rule     string
	Reason   string
	First    time.Time
	LastSeen time.Time
	Count    int
}

// Aggregated collapses a decision log into one row per destination and mode, newest first.
//
// A log with a line per request is a log nobody reads: a single dependency install produces
// hundreds of identical allows and buries the one denial that explains the failure. The
// per-decision records are still on disk — this is a display concern, and the raw form
// stays available.
//
// The stage is deliberately not part of the identity. It is Boks' own notion of where in
// the pipeline a check happened; what a reader wants is "this destination, carried this
// way, was allowed or refused for this reason, this many times".
func Aggregated(decisions []Decision) []Aggregate {
	type key struct {
		sandbox, typ, host string
		port               int
		mode               Mode
		allowed            bool
		rule, reason       string
	}
	index := map[key]int{}
	var out []Aggregate
	for _, d := range decisions {
		k := key{d.Sandbox, d.Type, d.Host, d.Port, d.Mode, d.Allowed, d.Rule, d.Reason}
		if i, ok := index[k]; ok {
			out[i].Count++
			if d.Time.After(out[i].LastSeen) {
				out[i].LastSeen = d.Time
			}
			if d.Time.Before(out[i].First) {
				out[i].First = d.Time
			}
			continue
		}
		index[k] = len(out)
		out = append(out, Aggregate{
			Sandbox: d.Sandbox, Type: d.Type, Host: d.Host, Port: d.Port,
			Mode: d.Mode, Allowed: d.Allowed, Rule: d.Rule, Reason: d.Reason,
			First: d.Time, LastSeen: d.Time, Count: 1,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// Destination renders the aggregate's host and port the way a user typed the rule.
func (a Aggregate) Destination() string {
	if strings.Contains(a.Host, ":") { // IPv6 literal
		return "[" + a.Host + "]:" + strconv.Itoa(a.Port)
	}
	return a.Host + ":" + strconv.Itoa(a.Port)
}

// DefaultLogPath is where the decision log lives when no path is given.
//
// XDG state on Linux, ~/Library/Application Support on macOS, %LocalAppData% on Windows —
// the same places the rest of Boks' host-side state will live.
func DefaultLogPath() string {
	return filepath.Join(StateDir(), "policy-log.jsonl")
}

// StateDir is where Boks keeps host-side state that is never shared into a guest. It is
// here rather than in a state package so that the policy log and the secret store agree on
// one location without either depending on the sandbox lifecycle.
func StateDir() string {
	if dir := os.Getenv("BOKS_STATE_DIR"); dir != "" {
		return dir
	}
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, "boks")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "boks")
		}
	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			return filepath.Join(dir, "boks")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "state", "boks")
		}
	}
	return filepath.Join(os.TempDir(), "boks")
}

// Engine evaluates a policy and records every decision it takes.
//
// Callers use the Engine rather than the Policy directly so that no decision path can
// forget to log. The Policy is copied in, so an Engine's behaviour cannot change under a
// request that is already in flight.
type Engine struct {
	policy  Policy
	log     *Log
	sandbox string
	now     func() time.Time
}

// NewEngine builds an engine over a policy. A nil log gets a default-sized one.
func NewEngine(p Policy, l *Log) *Engine {
	if l == nil {
		l = NewLog(DefaultCapacity)
	}
	return &Engine{policy: p, log: l, now: time.Now}
}

// WithSandbox labels decisions with the sandbox they came from.
func (e *Engine) WithSandbox(name string) *Engine {
	c := *e
	c.sandbox = name
	return &c
}

// Policy returns the policy being enforced.
func (e *Engine) Policy() Policy { return e.policy }

// Log returns the decision log.
func (e *Engine) Log() *Log { return e.log }

// Check evaluates a target at a given stage and records the outcome.
func (e *Engine) Check(stage Stage, t Target) Decision {
	return e.CheckMode(stage, t, NoMode)
}

// CheckMode is Check with the flow's disposition attached, so the log can say whether Boks
// read the connection or only carried it.
func (e *Engine) CheckMode(stage Stage, t Target, m Mode) Decision {
	return e.record(stage, t, e.policy.Evaluate(t), m)
}

// CheckResolved evaluates an address a permitted hostname resolved to, against deny rules
// only. See Policy.EvaluateDeny for why the allow rules are not consulted.
func (e *Engine) CheckResolved(t Target, m Mode) Decision {
	return e.record(StageDial, t, e.policy.EvaluateDeny(t), m)
}

// NoMode marks a decision taken before the flow's disposition is known.
const NoMode Mode = ""

// Note records that a permitted flow's disposition turned out differently from what was
// logged when it was allowed — a flow marked for inspection that ended up carried blind,
// say. Without it the log could claim Boks read something it did not, or the reverse, and
// that claim is the whole point of recording the mode at all.
//
// The reason is written by Boks, never taken from traffic.
func (e *Engine) Note(stage Stage, t Target, m Mode, reason string) Decision {
	return e.record(stage, t, Verdict{Allowed: true, Reason: reason}, m)
}

// NoteRefused records that a flow was refused for a reason no rule expresses — UDP and ICMP
// are dropped categorically, and the link refuses them before any policy check happens.
//
// It exists because Note asserts Allowed, and a dropped datagram recorded as allowed is
// worse than one not recorded at all: the log would claim Boks carried traffic it threw
// away. The reason is written by Boks, never taken from traffic.
func (e *Engine) NoteRefused(stage Stage, t Target, m Mode, reason string) Decision {
	return e.record(stage, t, Verdict{Allowed: false, Reason: reason}, m)
}

func (e *Engine) record(stage Stage, t Target, v Verdict, m Mode) Decision {
	resource := ResourceOf(t)
	rule := v.Rule
	if rule == "" {
		rule = NoRuleFor(t)
	}
	d := Decision{
		Time:     e.now(),
		Type:     TypeNetwork,
		Stage:    stage,
		Host:     t.Host,
		Port:     t.Port,
		Allowed:  v.Allowed,
		Action:   ActionConnectTCP,
		Resource: resource,
		Reason:   v.Reason,
		Rule:     rule,
		Policy:   e.policy.Name,
		Mode:     m,
		Sandbox:  e.sandbox,
	}
	e.log.Record(d)
	return d
}
