package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeSource records what the dashboard asked it to do.
type fakeSource struct {
	mu   sync.Mutex
	snap Snapshot
	err  error

	calls    []string
	added    []NewRule
	removed  []Rule
	created  []NewSandbox
	attached []string
	shells   []string
}

func (f *fakeSource) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSource) Snapshot(context.Context) (Snapshot, error) { return f.snap, f.err }
func (f *fakeSource) Agents() []AgentChoice {
	return []AgentChoice{{Name: "shell", Summary: "a shell"}, {Name: "claude", Summary: "Claude Code"}}
}
func (f *fakeSource) Start(_ context.Context, n string) (string, error) {
	f.record("start " + n)
	return "started " + n, nil
}
func (f *fakeSource) Stop(_ context.Context, n string) (string, error) {
	f.record("stop " + n)
	return "stopped " + n, nil
}
func (f *fakeSource) Remove(_ context.Context, n string, force bool) (string, error) {
	if force {
		f.record("rm --force " + n)
	} else {
		f.record("rm " + n)
	}
	return "removed " + n, nil
}
func (f *fakeSource) Create(_ context.Context, s NewSandbox) (string, error) {
	f.created = append(f.created, s)
	return "created", nil
}
func (f *fakeSource) AddRule(_ context.Context, r NewRule) (string, error) {
	f.added = append(f.added, r)
	return "added", nil
}
func (f *fakeSource) RemoveRule(_ context.Context, r Rule) (string, error) {
	f.removed = append(f.removed, r)
	return "removed", nil
}
func (f *fakeSource) Attach(n string) *exec.Cmd {
	f.attached = append(f.attached, n)
	return exec.Command("true")
}
func (f *fakeSource) Shell(n string) *exec.Cmd {
	f.shells = append(f.shells, n)
	return exec.Command("true")
}

var t0 = time.Date(2026, 11, 5, 18, 50, 20, 0, time.UTC)

func sampleSnapshot() Snapshot {
	return Snapshot{
		Sandboxes: []Sandbox{
			{Name: "claude-web", Agent: "claude", Status: "running", Workspace: "/Users/someone/src/a/very/long/path/to/the/web", Mode: "direct"},
			{Name: "shell-api", Agent: "shell", Status: "stopped", Workspace: "/src/api", Mode: "clone"},
			{Name: "codex-docs", Agent: "codex", Status: "stopped", Workspace: "/src/docs"},
		},
		Log: []LogEntry{
			{Sandbox: "claude-web", Host: "api.anthropic.com", Allowed: true, Hits: 14, Last: t0},
			{Sandbox: "claude-web", Host: "evil.example", Allowed: false, Hits: 2, Last: t0.Add(-time.Minute)},
			{Sandbox: "shell-api", Host: "registry.npmjs.org", Allowed: true, Hits: 3, Last: t0.Add(-time.Hour)},
		},
		Rules: map[string][]Rule{
			"claude-web": {
				{Scope: "preset standard", Allow: true, Resource: "github.com:443"},
				{Scope: "sandbox claude-web", Sandbox: "claude-web", Allow: true, Resource: "api.example.com", Methods: []string{"GET"}, Path: "/v1/**", Stored: true},
				{Scope: "global", Allow: false, Resource: "evil.example", Stored: true},
			},
			"shell-api": {{Scope: "preset standard", Allow: true, Resource: "github.com:443"}},
			"":          {{Scope: "global", Allow: false, Resource: "evil.example", Stored: true}},
		},
	}
}

// newTest builds a model that has read the fake's snapshot once.
func newTest(t *testing.T, w, h int) (Model, *fakeSource) {
	t.Helper()
	src := &fakeSource{snap: sampleSnapshot()}
	m := New(src, Info{Version: "0.1.41"}, WithNoColor(true))
	m = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	m = update(t, m, snapshotMsg{snap: src.snap})
	return m, src
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// press sends keys and returns the model and the last command any key produced.
func press(t *testing.T, m Model, keys ...string) (Model, tea.Cmd) {
	t.Helper()
	var last tea.Cmd
	for _, k := range keys {
		next, cmd := m.Update(keyMsg(k))
		m = next.(Model)
		if cmd != nil {
			last = cmd
		}
	}
	return m, last
}

// typeText sends each rune of s as a key.
func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = press(t, m, string(r))
	}
	return m
}

// runCmd runs a command and feeds its message back, as the tea runtime would.
func runCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	return update(t, m, cmd())
}

func TestNavigationMovesSelectionAndWraps(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	if m.sel != "claude-web" {
		t.Fatalf("first sandbox should be selected, got %q", m.sel)
	}
	m, _ = press(t, m, "down")
	if m.sel != "shell-api" {
		t.Fatalf("down: got %q", m.sel)
	}
	m, _ = press(t, m, "down", "down")
	if m.sel != "claude-web" {
		t.Fatalf("down past the end should wrap, got %q", m.sel)
	}
	m, _ = press(t, m, "up")
	if m.sel != "codex-docs" {
		t.Fatalf("up from the top should wrap, got %q", m.sel)
	}
}

func TestSelectionFollowsTheSandboxAcrossRefreshes(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, _ = press(t, m, "down") // shell-api
	snap := sampleSnapshot()
	snap.Sandboxes = []Sandbox{snap.Sandboxes[2], snap.Sandboxes[1], snap.Sandboxes[0]}
	m = update(t, m, snapshotMsg{snap: snap})
	if m.sel != "shell-api" {
		t.Fatalf("selection moved on refresh: %q", m.sel)
	}
	// The selected sandbox going away selects the first remaining one.
	snap.Sandboxes = snap.Sandboxes[:1]
	m = update(t, m, snapshotMsg{snap: snap})
	if m.sel != "codex-docs" {
		t.Fatalf("selection after removal: %q", m.sel)
	}
	_ = src
}

func TestFocusAndTabSwitching(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	m, _ = press(t, m, "tab")
	if m.focus != focusPane || m.tab != tabLog {
		t.Fatalf("tab should focus the pane on the log tab: focus=%v tab=%v", m.focus, m.tab)
	}
	m, _ = press(t, m, "down")
	if m.row != 1 {
		t.Fatalf("down in the pane should move the row, got %d", m.row)
	}
	m, _ = press(t, m, "right")
	if m.tab != tabRules || m.row != 0 {
		t.Fatalf("right should switch to the rules tab and reset the row: tab=%v row=%d", m.tab, m.row)
	}
	m, _ = press(t, m, "right")
	if m.tab != tabLog {
		t.Fatalf("right on the last tab should wrap, got %v", m.tab)
	}
	m, _ = press(t, m, "left")
	if m.focus != focusCards {
		t.Fatal("left on the first tab should return to the cards")
	}
	m, _ = press(t, m, "right", "esc")
	if m.focus != focusCards {
		t.Fatal("esc should return to the cards")
	}
}

func TestTabCountsAndHintsFollowTheSelection(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	v := m.View()
	for _, want := range []string{"Network Log (2)", "Network Rules (3)", "Enter run", "c create", "n network", "v0.1.41"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	m, _ = press(t, m, "down")
	v = m.View()
	if !strings.Contains(v, "Network Log (1)") || !strings.Contains(v, "Network Rules (1)") {
		t.Errorf("counts did not follow the selection:\n%s", v)
	}
	m, _ = press(t, m, "tab", "right")
	if v = m.View(); !strings.Contains(v, "a add rule") {
		t.Errorf("the rules tab's hint should offer adding a rule:\n%s", v)
	}
}

func TestTruncation(t *testing.T) {
	cases := []struct{ got, want string }{
		{truncRight("abcdef", 4), "abc…"},
		{truncRight("abc", 4), "abc"},
		{truncRight("abc", 0), ""},
		{truncLeft("/Users/someone/src/project", 10), "…c/project"},
		{truncLeft("/a/b", 10), "/a/b"},
		{truncLeft("abc", 1), "…"},
		{padRight("ab", 4), "ab  "},
		{padLeft("ab", 4), "  ab"},
		{padRight("abcdef", 3), "ab…"},
		{spread("left", "right", 12), "left   right"},
		{spread("a long left side", "right", 12), "a lon… right"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %q, want %q", i, c.got, c.want)
		}
	}
	// Wide runes are measured in cells, not bytes or runes.
	if w := width(truncRight("日本語のテキスト", 7)); w > 7 {
		t.Errorf("wide text truncated to %d cells", w)
	}
}

func TestCardTruncatesWorkspaceFromTheLeft(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	v := m.View()
	if !strings.Contains(v, "…") || !strings.Contains(v, "long/path/to/the/web") {
		t.Errorf("long workspace should keep its end:\n%s", v)
	}
	if !strings.Contains(v, "↵ run") {
		t.Error("the selected card should offer run")
	}
	if !strings.Contains(v, "stop") || !strings.Contains(v, "start") {
		t.Error("a running card offers stop and a stopped one start")
	}
}

func TestLayoutSharesWidthAmongFlexColumns(t *testing.T) {
	cols := []column{{width: 5}, {width: 4, flex: true}, {width: 4, flex: true}, {width: 3}}
	ws := layout(cols, 40, 2)
	sum := 6 // three gaps
	for _, w := range ws {
		sum += w
	}
	if sum != 40 || ws[0] != 5 || ws[3] != 3 {
		t.Fatalf("widths %v sum to %d", ws, sum)
	}
	// Too narrow: columns keep their minimum and the row is cut, never negative.
	ws = layout(cols, 5, 2)
	for _, w := range ws {
		if w < 0 {
			t.Fatalf("negative width in %v", ws)
		}
	}
}

func TestLogTableRows(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	v := m.View()
	for _, want := range []string{"2 total", "● 1 allowed", "● 1 blocked", "Date ↑", "Host", "Hits", "Status",
		"api.anthropic.com", "Allowed", "evil.example", "Blocked", "05 Nov,"} {
		if !strings.Contains(v, want) {
			t.Errorf("log table lacks %q", want)
		}
	}
	if strings.Contains(v, "Sandbox  ") && strings.Contains(v, "registry.npmjs.org") {
		t.Error("a single sandbox's log should not show other sandboxes' rows")
	}
	// The network view shows every sandbox, with a Sandbox column.
	m, _ = press(t, m, "n")
	v = m.View()
	for _, want := range []string{"All sandboxes ▾", "Sandbox", "registry.npmjs.org", "shell-api", "3 sandbox(es), 1 running", "Esc back"} {
		if !strings.Contains(v, want) {
			t.Errorf("network view lacks %q:\n%s", want, v)
		}
	}
	// The selected row names its action.
	if !strings.Contains(v, "block") {
		t.Error("the selected allowed row should offer block")
	}
}

func TestLogTableHeaderAlignsWithRows(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	var header, row string
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, "Date ↑") {
			header = l
		}
		if strings.Contains(l, "api.anthropic.com") {
			row = l
		}
	}
	if header == "" || row == "" {
		t.Fatal("header or row missing")
	}
	if hi, ri := cellIndex(header, "Host"), cellIndex(row, "api.anthropic.com"); hi != ri {
		t.Errorf("Host header at cell %d, host at %d\n%s\n%s", hi, ri, header, row)
	}
	if hi, ri := cellIndex(header, "Status")+len("Status"), cellIndex(row, "Allowed")+len("Allowed"); hi != ri {
		t.Errorf("Status is right-aligned: header ends at %d, value at %d", hi, ri)
	}
}

// cellIndex is the terminal column at which sub starts in s.
func cellIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return width(s[:i])
}

func TestStatusFilterAndSearch(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	m, _ = press(t, m, "n")
	if n := len(m.LogRows()); n != 3 {
		t.Fatalf("all rows: %d", n)
	}
	m, _ = press(t, m, "o")
	if n := len(m.LogRows()); n != 2 {
		t.Fatalf("allowed only: %d", n)
	}
	m, _ = press(t, m, "o")
	if rows := m.LogRows(); len(rows) != 1 || rows[0].Allowed {
		t.Fatalf("blocked only: %+v", rows)
	}
	m, _ = press(t, m, "o", "/")
	m = typeText(t, m, "npm")
	if rows := m.LogRows(); len(rows) != 1 || rows[0].Host != "registry.npmjs.org" {
		t.Fatalf("search: %+v", rows)
	}
	m, _ = press(t, m, "esc")
	if m.searching || len(m.LogRows()) != 3 {
		t.Fatal("esc while searching clears the search")
	}
}

func TestNetworkViewSandboxPicker(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	m, _ = press(t, m, "n", "s")
	if m.modal != modalPicker {
		t.Fatal("s in the network view opens the sandbox picker")
	}
	if v := m.View(); !strings.Contains(v, "Active only") || !strings.Contains(v, "codex-docs") {
		t.Errorf("picker:\n%s", v)
	}
	m, _ = press(t, m, "down", "enter")
	if m.netFilter != filterActive || len(m.LogRows()) != 2 {
		t.Fatalf("active only: filter %q rows %d", m.netFilter, len(m.LogRows()))
	}
	m, _ = press(t, m, "esc")
	if m.view != viewDashboard {
		t.Fatal("esc leaves the network view")
	}
}

func TestRemoveAsksFirstAndCancelDoesNothing(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, cmd := press(t, m, "r")
	if m.modal != modalConfirm || cmd != nil {
		t.Fatalf("r must ask before removing: modal=%v cmd=%v", m.modal, cmd != nil)
	}
	v := m.View()
	for _, want := range []string{"Remove Sandbox", "Delete claude-web", "It is running", "y yes, remove"} {
		if !strings.Contains(v, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, v)
		}
	}
	for _, no := range []string{"n", "esc", "enter"} {
		m, _ = press(t, m, "r")
		var cmd tea.Cmd
		m, cmd = press(t, m, no)
		if m.modal != modalNone || cmd != nil {
			t.Fatalf("%q should cancel without acting", no)
		}
	}
	if len(src.calls) != 0 {
		t.Fatalf("nothing should have run: %v", src.calls)
	}
}

func TestRemoveConfirmedRunsRmWithForceForARunningSandbox(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, _ = press(t, m, "r")
	m, cmd := press(t, m, "y")
	if m.busy["claude-web"] != "removing" {
		t.Errorf("the card should show the removal in flight: %v", m.busy)
	}
	m = runCmd(t, m, cmd)
	if len(src.calls) != 1 || src.calls[0] != "rm --force claude-web" {
		t.Fatalf("calls: %v", src.calls)
	}
	if m.busy["claude-web"] != "" || !strings.Contains(m.statusMsg, "removed claude-web") {
		t.Errorf("after: busy=%v status=%q", m.busy, m.statusMsg)
	}
	// A stopped sandbox is removed without --force.
	m, _ = press(t, m, "down", "r")
	_, cmd = press(t, m, "y")
	cmd()
	if src.calls[1] != "rm shell-api" {
		t.Fatalf("calls: %v", src.calls)
	}
}

func TestStartStopAndExecUseTheSource(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, cmd := press(t, m, "s")
	m = runCmd(t, m, cmd)
	m, _ = press(t, m, "down")
	_, cmd = press(t, m, "s")
	cmd()
	if strings.Join(src.calls, ",") != "stop claude-web,start shell-api" {
		t.Fatalf("calls: %v", src.calls)
	}
	m, cmd = press(t, m, "enter")
	if cmd == nil || len(src.attached) != 1 || src.attached[0] != "shell-api" {
		t.Fatalf("enter should attach: %v", src.attached)
	}
	_, cmd = press(t, m, "x")
	if cmd == nil || len(src.shells) != 1 || src.shells[0] != "shell-api" {
		t.Fatalf("x should open a shell: %v", src.shells)
	}
}

func TestRuleRemovalAsksAndRefusesUnstoredRules(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, _ = press(t, m, "tab", "right") // rules tab, row 0: a preset rule
	m, cmd := press(t, m, "r")
	if m.modal != modalNone || cmd != nil || !strings.Contains(m.statusMsg, "preset standard") {
		t.Fatalf("a preset rule cannot be removed: modal=%v status=%q", m.modal, m.statusMsg)
	}
	m, _ = press(t, m, "down", "r")
	if m.modal != modalConfirm {
		t.Fatal("removing a stored rule asks first")
	}
	if v := m.View(); !strings.Contains(v, "api.example.com GET /v1/**") {
		t.Errorf("the confirmation names the rule with its selector:\n%s", v)
	}
	_, cmd = press(t, m, "y")
	cmd()
	if len(src.removed) != 1 || src.removed[0].Resource != "api.example.com" || src.removed[0].Sandbox != "claude-web" {
		t.Fatalf("removed: %+v", src.removed)
	}
}

func TestAddRuleForm(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, _ = press(t, m, "tab", "right", "a")
	if m.modal != modalAddRule {
		t.Fatal("a on the rules tab opens the form")
	}
	if v := m.View(); !strings.Contains(v, "Add Network Rule") || !strings.Contains(v, "sandbox:claude-web") {
		t.Errorf("form:\n%s", v)
	}
	// Enter with no host is refused in the form.
	m, cmd := press(t, m, "enter")
	if cmd != nil || m.ruleForm.err == "" {
		t.Fatal("an empty host must not be submitted")
	}
	m = typeText(t, m, "api.example.org")
	m, _ = press(t, m, "tab")
	m = typeText(t, m, "get,head")
	m, _ = press(t, m, "tab")
	m = typeText(t, m, "/v1/**")
	m, _ = press(t, m, "tab", "2") // to the type field, block
	m, cmd = press(t, m, "enter")
	if m.modal != modalNone {
		t.Fatal("enter submits")
	}
	cmd()
	want := NewRule{Sandbox: "claude-web", Allow: false, Resource: "api.example.org", Methods: []string{"GET", "HEAD"}, Path: "/v1/**"}
	if len(src.added) != 1 || !equalRule(src.added[0], want) {
		t.Fatalf("added %+v, want %+v", src.added, want)
	}
}

func equalRule(a, b NewRule) bool {
	return a.Sandbox == b.Sandbox && a.Allow == b.Allow && a.Resource == b.Resource &&
		strings.Join(a.Methods, ",") == strings.Join(b.Methods, ",") && a.Path == b.Path
}

func TestLogRowEnterBlocksOrAllowsForItsSandbox(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, _ = press(t, m, "tab")
	_, cmd := press(t, m, "enter")
	cmd()
	m, _ = press(t, m, "down")
	_, cmd = press(t, m, "enter")
	cmd()
	if len(src.added) != 2 ||
		!equalRule(src.added[0], NewRule{Sandbox: "claude-web", Allow: false, Resource: "api.anthropic.com"}) ||
		!equalRule(src.added[1], NewRule{Sandbox: "claude-web", Allow: true, Resource: "evil.example"}) {
		t.Fatalf("added: %+v", src.added)
	}
}

func TestCreateWizard(t *testing.T) {
	m, src := newTest(t, 160, 50)
	m, _ = press(t, m, "c")
	if v := m.View(); !strings.Contains(v, "Create Sandbox") || !strings.Contains(v, "1/2") {
		t.Fatalf("wizard:\n%s", v)
	}
	m, _ = press(t, m, "down", "enter")
	if v := m.View(); !strings.Contains(v, "2/2") || !strings.Contains(v, "claude") {
		t.Fatalf("step 2:\n%s", v)
	}
	m, cmd := press(t, m, "enter")
	if cmd != nil || m.create.err == "" {
		t.Fatal("a workspace is required")
	}
	m = typeText(t, m, "/src/x")
	m, _ = press(t, m, "tab")
	m = typeText(t, m, "mine")
	m, cmd = press(t, m, "enter")
	cmd()
	if len(src.created) != 1 || src.created[0] != (NewSandbox{Agent: "claude", Workspace: "/src/x", Name: "mine"}) {
		t.Fatalf("created: %+v", src.created)
	}
	// Esc steps back, then closes.
	m, _ = press(t, m, "c", "enter", "esc")
	if m.modal != modalCreate || m.create.step != 0 {
		t.Fatal("esc on step 2 goes back to step 1")
	}
	m, _ = press(t, m, "esc")
	if m.modal != modalNone {
		t.Fatal("esc on step 1 closes")
	}
}

func TestSettingsPanel(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	m, _ = press(t, m, "ctrl+s")
	if v := m.View(); !strings.Contains(v, "Settings") || !strings.Contains(v, "0.1.41") {
		t.Fatalf("settings:\n%s", v)
	}
	m, _ = press(t, m, "esc")
	if m.modal != modalNone {
		t.Fatal("esc closes the settings")
	}
}

func TestEveryTerminalSizeRendersWithoutPanicking(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	states := []Model{m}
	n, _ := press(t, m, "n")
	states = append(states, n)
	r, _ := press(t, m, "r")
	states = append(states, r)
	a, _ := press(t, m, "tab", "right", "a")
	states = append(states, a)
	c, _ := press(t, m, "c")
	states = append(states, c)
	p, _ := press(t, m, "n", "s")
	states = append(states, p)
	for _, s := range states {
		for w := 0; w <= 200; w += 7 {
			for h := 0; h <= 60; h += 5 {
				s = update(t, s, tea.WindowSizeMsg{Width: w, Height: h})
				v := s.View()
				lines := strings.Split(v, "\n")
				if h >= minHeight && w >= minWidth && len(lines) != h {
					t.Fatalf("%dx%d: %d lines", w, h, len(lines))
				}
				for _, l := range lines {
					if width(l) > max(w, 0) {
						t.Fatalf("%dx%d: line wider than the terminal: %q", w, h, l)
					}
				}
			}
		}
	}
}

func TestSmallTerminalDegrades(t *testing.T) {
	m, _ := newTest(t, 20, 5)
	if v := m.View(); !strings.Contains(v, "boks: the terminal") {
		t.Errorf("tiny terminal: %q", v)
	}
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 30})
	v := m.View()
	if strings.Contains(v, "Network Log") {
		t.Error("a narrow terminal drops the network pane")
	}
	if !strings.Contains(v, "claude-web") {
		t.Error("a narrow terminal still shows the cards")
	}
	// Tab, which would move into the hidden pane, opens the network view for the sandbox.
	m, _ = press(t, m, "tab")
	if m.view != viewNetwork || m.netFilter != "claude-web" || len(m.LogRows()) != 2 {
		t.Fatalf("narrow tab: view=%v filter=%q", m.view, m.netFilter)
	}
}

func TestNoColorEmitsNoEscapes(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	for _, s := range []Model{m, mustPress(t, m, "n"), mustPress(t, m, "r")} {
		if strings.Contains(s.View(), "\x1b[") {
			t.Fatal("NO_COLOR output contains escape sequences")
		}
	}
}

func mustPress(t *testing.T, m Model, keys ...string) Model {
	m, _ = press(t, m, keys...)
	return m
}

func TestRefreshDoesNotOverlap(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	next, cmd := m.Update(tickMsg{})
	m = next.(Model)
	if cmd == nil || !m.refreshing {
		t.Fatal("a tick starts a refresh")
	}
	if _, cmd = m.Update(tickMsg{}); cmd != nil {
		t.Fatal("a tick during a refresh must not start another")
	}
}

func TestRefreshErrorKeepsTheLastSnapshot(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	m = update(t, m, snapshotMsg{err: errors.New("containerd went away")})
	if len(m.snap.Sandboxes) != 3 {
		t.Fatal("a failed refresh must not blank the dashboard")
	}
	if v := m.View(); !strings.Contains(v, "refresh failed: containerd went away") {
		t.Errorf("the failure should be shown:\n%s", v)
	}
}

func TestEmptyDashboard(t *testing.T) {
	src := &fakeSource{snap: Snapshot{Notice: "The Boks daemon is not running."}}
	m := New(src, Info{}, WithNoColor(true))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = update(t, m, snapshotMsg{snap: src.snap})
	v := m.View()
	if !strings.Contains(v, "No sandboxes yet.") || !strings.Contains(v, "daemon is not running") {
		t.Errorf("empty:\n%s", v)
	}
	// Keys that need a sandbox do nothing rather than panic.
	m, _ = press(t, m, "enter", "s", "x", "r", "tab", "down", "right")
	if m.modal != modalNone {
		t.Fatal("no sandbox, no confirmation")
	}
}
