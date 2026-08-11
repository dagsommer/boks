package policy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
)

// Decision is one logged policy outcome. It is the unit `boks policy log` displays and the
// only record Boks keeps of sandbox network activity. It stays on this machine.
type Decision struct {
	Time    time.Time `json:"time"`
	Stage   Stage     `json:"stage"`
	Host    string    `json:"host"`
	Port    int       `json:"port"`
	Allowed bool      `json:"allowed"`
	Reason  string    `json:"reason"`
	Rule    string    `json:"rule,omitempty"`
	Policy  string    `json:"policy"`
	// Sandbox is the sandbox the flow came from, when the proxy knows it.
	Sandbox string `json:"sandbox,omitempty"`
}

func (d Decision) String() string {
	verb := "DENY "
	if d.Allowed {
		verb = "ALLOW"
	}
	return fmt.Sprintf("%s %s %-7s %s:%d  %s",
		d.Time.Format(time.RFC3339), verb, d.Stage, d.Host, d.Port, d.Reason)
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
	return e.record(stage, t, e.policy.Evaluate(t))
}

// CheckResolved evaluates an address a permitted hostname resolved to, against deny rules
// only. See Policy.EvaluateDeny for why the allow rules are not consulted.
func (e *Engine) CheckResolved(t Target) Decision {
	return e.record(StageDial, t, e.policy.EvaluateDeny(t))
}

func (e *Engine) record(stage Stage, t Target, v Verdict) Decision {
	d := Decision{
		Time:    e.now(),
		Stage:   stage,
		Host:    t.Host,
		Port:    t.Port,
		Allowed: v.Allowed,
		Reason:  v.Reason,
		Rule:    v.Rule,
		Policy:  e.policy.Name,
		Sandbox: e.sandbox,
	}
	e.log.Record(d)
	return d
}
