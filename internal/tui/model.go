package tui

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DefaultInterval is how often the dashboard re-reads its source.
const DefaultInterval = 1500 * time.Millisecond

type viewKind int

const (
	viewDashboard viewKind = iota // cards on the left, the selected sandbox on the right
	viewNetwork                   // `n`: every sandbox's network, full width
)

type focusKind int

const (
	focusCards focusKind = iota
	focusPane
)

type tabKind int

const (
	tabLog tabKind = iota
	tabRules
	tabCount
)

// Network-view sandbox filters that are not sandbox names. They cannot collide with a
// name: a sandbox name never starts with a NUL.
const (
	filterAll      = ""
	filterActive   = "\x00active"
	filterInactive = "\x00inactive"
)

// statusFilter narrows the table on the current tab: allowed or blocked log entries, allow
// or deny rules. `o` cycles it, as in sbx.
type statusFilter int

const (
	statusAll statusFilter = iota
	statusAllowed
	statusBlocked
)

type modalKind int

const (
	modalNone modalKind = iota
	modalConfirm
	modalAddRule
	modalCreate
	modalSettings
	modalPicker
)

// confirmState is a yes/no question in front of something that cannot be undone.
type confirmState struct {
	title string
	lines []string
	// action is what `y` runs; it is a tea.Cmd so the Model stays free of I/O.
	action tea.Cmd
	// pending is what the status line says while the action runs.
	pending string
	busyFor string
}

// ruleForm is the add-rule modal.
type ruleForm struct {
	sandbox string // "" is global
	field   int    // 0 type, 1 host, 2 methods, 3 path
	allow   bool
	host    textField
	methods textField
	path    textField
	err     string
}

const ruleFormFields = 4

// createForm is the create wizard: agent, then workspace (and an optional name).
type createForm struct {
	step      int // 0 agent, 1 workspace and name
	agents    []AgentChoice
	agentIdx  int
	field     int // on step 1: 0 workspace, 1 name
	workspace textField
	name      textField
	err       string
}

// pickerState is the network view's sandbox dropdown.
type pickerState struct {
	options []string // filter values
	idx     int
}

// Model is the dashboard's state. It is a value: Update returns a new one.
type Model struct {
	src  Source
	info Info
	ctx  context.Context

	width, height int
	interval      time.Duration
	now           func() time.Time

	snap       Snapshot
	loaded     bool
	loadErr    error
	refreshing bool

	view   viewKind
	focus  focusKind
	tab    tabKind
	sel    string // the selected sandbox, by name, so a refresh does not move it
	row    int    // the selected row of the pane's table
	offset int    // first visible row of the pane's table

	netFilter string
	status    statusFilter
	search    textField
	searching bool

	modal    modalKind
	confirm  confirmState
	ruleForm ruleForm
	create   createForm
	picker   pickerState

	// busy marks a sandbox with an action in flight, by the verb shown on its card.
	busy      map[string]string
	statusMsg string
	statusErr bool

	noColor bool
	st      styles
}

// Option configures a Model.
type Option func(*Model)

// WithInterval sets the refresh interval.
func WithInterval(d time.Duration) Option { return func(m *Model) { m.interval = d } }

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(m *Model) { m.now = now } }

// WithNoColor renders without colour.
func WithNoColor(v bool) Option { return func(m *Model) { m.noColor = v } }

// WithContext sets the context Source calls run under.
func WithContext(ctx context.Context) Option { return func(m *Model) { m.ctx = ctx } }

// New builds a Model over a source.
func New(src Source, info Info, opts ...Option) Model {
	m := Model{
		src:      src,
		info:     info,
		ctx:      context.Background(),
		interval: DefaultInterval,
		now:      time.Now,
		width:    100,
		height:   30,
		busy:     map[string]string{},
	}
	for _, o := range opts {
		o(&m)
	}
	m.st = newStyles(m.noColor)
	return m
}

// Messages.

type snapshotMsg struct {
	snap Snapshot
	err  error
}

type tickMsg struct{}

type actionMsg struct {
	sandbox string
	out     string
	err     error
}

// execDoneMsg arrives when an attached terminal session ends.
type execDoneMsg struct {
	what string
	err  error
}

// Init starts the first read.
func (m Model) Init() tea.Cmd {
	return m.refresh()
}

func (m *Model) refreshCmd() tea.Cmd {
	src, ctx := m.src, m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		snap, err := src.Snapshot(c)
		return snapshotMsg{snap: snap, err: err}
	}
}

func (m Model) refresh() tea.Cmd { return m.refreshCmd() }

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// Update handles a message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampRow()
		return m, nil

	case snapshotMsg:
		m.refreshing = false
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil && !reflect.DeepEqual(m.snap, msg.snap) {
			m.snap = msg.snap
			m.fixSelection()
			m.clampRow()
		}
		return m, m.tick()

	case tickMsg:
		if m.refreshing {
			return m, nil
		}
		m.refreshing = true
		return m, m.refreshCmd()

	case actionMsg:
		delete(m.busy, msg.sandbox)
		m.setResult(msg.out, msg.err)
		return m, m.refreshNow()

	case execDoneMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("%s: %v", msg.what, msg.err), true)
		} else {
			m.setStatus(msg.what+" ended", false)
		}
		return m, m.refreshNow()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// refreshNow reads the source immediately, after an action, unless a read is in flight.
func (m *Model) refreshNow() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	return m.refreshCmd()
}

func (m *Model) setStatus(s string, isErr bool) {
	m.statusMsg, m.statusErr = s, isErr
}

// setResult shows the last meaningful line of what a command printed.
func (m *Model) setResult(out string, err error) {
	if err != nil {
		text := strings.TrimSpace(err.Error())
		if t := lastLine(out); t != "" && !strings.Contains(text, t) {
			text = t + " — " + text
		}
		m.setStatus(text, true)
		return
	}
	m.setStatus(lastLine(out), false)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// Selection.

func (m *Model) selectedIndex() int {
	for i, s := range m.snap.Sandboxes {
		if s.Name == m.sel {
			return i
		}
	}
	return -1
}

// Selected returns the selected sandbox, if there is one.
func (m *Model) Selected() (Sandbox, bool) {
	if i := m.selectedIndex(); i >= 0 {
		return m.snap.Sandboxes[i], true
	}
	return Sandbox{}, false
}

// fixSelection keeps the selection on a sandbox that exists: the same one if it is still
// there, otherwise the first.
func (m *Model) fixSelection() {
	if m.selectedIndex() >= 0 {
		return
	}
	if len(m.snap.Sandboxes) > 0 {
		m.sel = m.snap.Sandboxes[0].Name
	} else {
		m.sel = ""
		if m.view == viewDashboard {
			m.focus = focusCards
		}
	}
}

func (m *Model) moveSelection(d int) {
	n := len(m.snap.Sandboxes)
	if n == 0 {
		return
	}
	i := m.selectedIndex()
	if i < 0 {
		i = 0
	} else {
		i = (i + d + n) % n
	}
	m.sel = m.snap.Sandboxes[i].Name
	m.row, m.offset = 0, 0
}

// The pane's rows.

// paneSandbox is the sandbox the pane is about; "" in the network view's aggregate filters.
func (m *Model) paneScope() (name string, set map[string]bool) {
	if m.view == viewDashboard {
		return m.sel, nil
	}
	switch m.netFilter {
	case filterAll:
		return "", nil
	case filterActive, filterInactive:
		set = map[string]bool{}
		for _, s := range m.snap.Sandboxes {
			if s.Running() == (m.netFilter == filterActive) {
				set[s.Name] = true
			}
		}
		return "", set
	default:
		return m.netFilter, nil
	}
}

// showSandboxColumn reports whether the tables carry a Sandbox column: whenever more than
// one sandbox can appear in them.
func (m *Model) showSandboxColumn() bool {
	name, _ := m.paneScope()
	return name == ""
}

// scopeLog is the log for the pane's scope, before the status filter and search.
func (m *Model) scopeLog() []LogEntry {
	name, set := m.paneScope()
	var out []LogEntry
	for _, e := range m.snap.Log {
		switch {
		case name != "" && e.Sandbox != name:
			continue
		case set != nil && !set[e.Sandbox]:
			continue
		}
		out = append(out, e)
	}
	return out
}

// LogRows are the network log rows the pane shows.
func (m *Model) LogRows() []LogEntry {
	q := strings.ToLower(m.search.String())
	var out []LogEntry
	for _, e := range m.scopeLog() {
		if m.status == statusAllowed && !e.Allowed || m.status == statusBlocked && e.Allowed {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Host+" "+e.Sandbox), q) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (m *Model) scopeRules() []Rule {
	name, set := m.paneScope()
	if name != "" {
		return m.snap.Rules[name]
	}
	all := m.snap.Rules[""]
	if set == nil {
		return all
	}
	var out []Rule
	for _, r := range all {
		if r.Sandbox == "" || set[r.Sandbox] {
			out = append(out, r)
		}
	}
	return out
}

// RuleRows are the network rule rows the pane shows.
func (m *Model) RuleRows() []Rule {
	q := strings.ToLower(m.search.String())
	var out []Rule
	for _, r := range m.scopeRules() {
		if m.status == statusAllowed && !r.Allow || m.status == statusBlocked && r.Allow {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.Resource+" "+r.Scope), q) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m *Model) rowCount() int {
	if m.tab == tabLog {
		return len(m.LogRows())
	}
	return len(m.RuleRows())
}

func (m *Model) clampRow() {
	n := m.rowCount()
	if m.row >= n {
		m.row = n - 1
	}
	if m.row < 0 {
		m.row = 0
	}
	if m.offset > m.row {
		m.offset = m.row
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) paneFocused() bool {
	return m.view == viewNetwork || m.focus == focusPane
}

// Keys.

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	switch m.modal {
	case modalConfirm:
		return m.keyConfirm(k)
	case modalAddRule:
		return m.keyRuleForm(k)
	case modalCreate:
		return m.keyCreate(k)
	case modalSettings:
		if k.Type == tea.KeyEsc || k.Type == tea.KeyEnter || k.Type == tea.KeyCtrlS || k.String() == "q" {
			m.modal = modalNone
		}
		return m, nil
	case modalPicker:
		return m.keyPicker(k)
	}
	if m.searching {
		return m.keySearch(k)
	}

	key := k.String()
	switch key {
	case "ctrl+s":
		m.modal = modalSettings
		return m, nil
	case "q":
		return m, tea.Quit
	case "c":
		return m.openCreate()
	}
	if m.view == viewNetwork {
		return m.keyNetwork(k)
	}
	if m.focus == focusCards {
		return m.keyCards(k)
	}
	return m.keyPane(k)
}

func (m Model) keyCards(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "right", "l", "tab":
		if _, ok := m.Selected(); ok {
			m.focus = focusPane
		}
	case "enter":
		return m.attach()
	case "s":
		return m.startStop()
	case "x":
		return m.shell()
	case "r", "delete":
		return m.askRemove()
	case "n":
		m.openNetwork()
	}
	return m, nil
}

// keyPane handles the right-hand pane of the dashboard, which shares its table keys with
// the network view.
func (m Model) keyPane(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "tab", "shift+tab":
		m.focus = focusCards
		return m, nil
	case "left", "h":
		if m.tab == 0 {
			m.focus = focusCards
			return m, nil
		}
	case "n":
		m.openNetwork()
		return m, nil
	}
	return m.keyTable(k)
}

func (m Model) keyNetwork(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		if m.search.String() != "" {
			m.search.Clear()
			m.clampRow()
			return m, nil
		}
		m.view = viewDashboard
		m.focus = focusCards
		m.row, m.offset = 0, 0
		return m, nil
	case "s":
		m.openPicker()
		return m, nil
	case "tab":
		return m.switchTab(1), nil
	case "shift+tab":
		return m.switchTab(-1), nil
	}
	return m.keyTable(k)
}

// keyTable is the keys of a focused table: rows, tabs, filters, and the row's action.
func (m Model) keyTable(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		if m.row > 0 {
			m.row--
		}
	case "down", "j":
		if m.row < m.rowCount()-1 {
			m.row++
		}
	case "pgup":
		m.row -= 10
		m.clampRow()
	case "pgdown":
		m.row += 10
		m.clampRow()
	case "home", "g":
		m.row = 0
	case "end", "G":
		m.row = m.rowCount() - 1
		m.clampRow()
	case "left", "h":
		return m.switchTab(-1), nil
	case "right", "l":
		return m.switchTab(1), nil
	case "o":
		m.status = (m.status + 1) % 3
		m.clampRow()
	case "/":
		m.searching = true
	case "a":
		if m.tab == tabRules {
			return m.openRuleForm()
		}
	case "r", "d", "delete":
		if m.tab == tabRules {
			return m.askRemoveRule()
		}
	case "enter":
		if m.tab == tabRules {
			return m.askRemoveRule()
		}
		return m.toggleFromLog()
	}
	return m, nil
}

func (m Model) switchTab(d int) Model {
	t := (int(m.tab) + d + int(tabCount)) % int(tabCount)
	m.tab = tabKind(t)
	m.row, m.offset = 0, 0
	m.status = statusAll
	return m
}

func (m Model) keySearch(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.search.Clear()
		m.searching = false
	case tea.KeyEnter:
		m.searching = false
	default:
		m.search.Key(k)
	}
	m.row, m.offset = 0, 0
	return m, nil
}

func (m *Model) openNetwork() {
	m.view = viewNetwork
	m.netFilter = filterAll
	m.row, m.offset = 0, 0
	m.status = statusAll
}

func (m *Model) openPicker() {
	opts := []string{filterAll, filterActive, filterInactive}
	names := make([]string, 0, len(m.snap.Sandboxes))
	for _, s := range m.snap.Sandboxes {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	opts = append(opts, names...)
	m.picker = pickerState{options: opts}
	for i, o := range opts {
		if o == m.netFilter {
			m.picker.idx = i
		}
	}
	m.modal = modalPicker
}

func (m Model) keyPicker(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "s":
		m.modal = modalNone
	case "up", "k":
		if m.picker.idx > 0 {
			m.picker.idx--
		}
	case "down", "j":
		if m.picker.idx < len(m.picker.options)-1 {
			m.picker.idx++
		}
	case "enter":
		m.netFilter = m.picker.options[m.picker.idx]
		m.modal = modalNone
		m.row, m.offset = 0, 0
	}
	return m, nil
}

// filterLabel names a network-view filter for the dropdown.
func filterLabel(f string) string {
	switch f {
	case filterAll:
		return "All sandboxes"
	case filterActive:
		return "Active only"
	case filterInactive:
		return "Inactive only"
	}
	return f
}

// Actions.

// action runs fn off the UI goroutine. sandbox, if set, is marked busy with verb until it
// finishes; status is what the status line says meanwhile.
func (m *Model) action(sandbox, verb, status string, fn func(ctx context.Context) (string, error)) tea.Cmd {
	if sandbox != "" {
		m.busy[sandbox] = verb
	}
	m.setStatus(status, false)
	ctx := m.ctx
	return func() tea.Msg {
		out, err := fn(ctx)
		return actionMsg{sandbox: sandbox, out: out, err: err}
	}
}

func (m Model) startStop() (tea.Model, tea.Cmd) {
	s, ok := m.Selected()
	if !ok || m.busy[s.Name] != "" {
		return m, nil
	}
	src := m.src
	if s.Running() {
		return m, m.action(s.Name, "stopping", "stopping "+s.Name+"…", func(ctx context.Context) (string, error) { return src.Stop(ctx, s.Name) })
	}
	return m, m.action(s.Name, "starting", "starting "+s.Name+"…", func(ctx context.Context) (string, error) { return src.Start(ctx, s.Name) })
}

func (m Model) attach() (tea.Model, tea.Cmd) {
	s, ok := m.Selected()
	if !ok {
		return m, nil
	}
	cmd := m.src.Attach(s.Name)
	if cmd == nil {
		return m, nil
	}
	return m, session(cmd, "agent session in "+s.Name)
}

func (m Model) shell() (tea.Model, tea.Cmd) {
	s, ok := m.Selected()
	if !ok {
		return m, nil
	}
	cmd := m.src.Shell(s.Name)
	if cmd == nil {
		return m, nil
	}
	return m, session(cmd, "shell in "+s.Name)
}

func (m Model) askRemove() (tea.Model, tea.Cmd) {
	s, ok := m.Selected()
	if !ok || m.busy[s.Name] != "" {
		return m, nil
	}
	lines := []string{
		"Delete " + s.Name + " and its filesystem?",
		"Anything written inside it that is not in a shared workspace is gone.",
	}
	if s.Mode == "clone" {
		lines = append(lines, "It works on a clone: commits not taken out with 'boks bundle' are lost.")
	}
	if s.Running() {
		lines = append(lines, "It is running, and whatever is inside it is killed.")
	}
	src, name, force := m.src, s.Name, s.Running()
	verb := "removing"
	m.confirm = confirmState{
		title: "Remove Sandbox",
		lines: lines,
		action: func() tea.Msg {
			out, err := src.Remove(m.ctx, name, force)
			return actionMsg{sandbox: name, out: out, err: err}
		},
		pending: verb + " " + name + "…",
		busyFor: name,
	}
	m.modal = modalConfirm
	return m, nil
}

func (m Model) currentRule() (Rule, bool) {
	rows := m.RuleRows()
	if m.row < 0 || m.row >= len(rows) {
		return Rule{}, false
	}
	return rows[m.row], true
}

func (m Model) askRemoveRule() (tea.Model, tea.Cmd) {
	r, ok := m.currentRule()
	if !ok {
		return m, nil
	}
	if !r.Stored {
		m.setStatus(fmt.Sprintf("%s comes from %s, not from the policy store; it cannot be removed here", r.Resource, r.Scope), true)
		return m, nil
	}
	src := m.src
	m.confirm = confirmState{
		title: "Remove Network Rule",
		lines: []string{
			fmt.Sprintf("Remove %s %s from %s?", decisionWord(r.Allow), describeRule(r), r.Scope),
		},
		action: func() tea.Msg {
			out, err := src.RemoveRule(m.ctx, r)
			return actionMsg{out: out, err: err}
		},
		pending: "removing rule…",
	}
	m.modal = modalConfirm
	return m, nil
}

func (m Model) keyConfirm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "y", "Y":
		c := m.confirm
		m.modal = modalNone
		m.confirm = confirmState{}
		if c.busyFor != "" {
			m.busy[c.busyFor] = "removing"
		}
		m.setStatus(c.pending, false)
		return m, c.action
	case "n", "N", "esc", "q", "enter":
		// Enter is "no": the safe answer is the one a stray keypress gives.
		m.modal = modalNone
		m.confirm = confirmState{}
		m.setStatus("cancelled", false)
	}
	return m, nil
}

// toggleFromLog writes the rule a log row's inline action names: block an allowed host,
// allow a blocked one, scoped to the sandbox the row came from.
func (m Model) toggleFromLog() (tea.Model, tea.Cmd) {
	rows := m.LogRows()
	if m.row < 0 || m.row >= len(rows) {
		return m, nil
	}
	e := rows[m.row]
	r := NewRule{Sandbox: e.Sandbox, Allow: !e.Allowed, Resource: e.Host}
	src := m.src
	return m, m.action("", "", decisionWord(r.Allow)+"ing "+e.Host+"…", func(ctx context.Context) (string, error) {
		return src.AddRule(ctx, r)
	})
}

func (m Model) openRuleForm() (tea.Model, tea.Cmd) {
	name, _ := m.paneScope()
	m.ruleForm = ruleForm{sandbox: name, allow: true, field: 1}
	m.modal = modalAddRule
	return m, nil
}

func (m Model) keyRuleForm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.ruleForm
	switch k.Type {
	case tea.KeyEsc:
		m.modal = modalNone
		return m, nil
	case tea.KeyTab, tea.KeyDown:
		f.field = (f.field + 1) % ruleFormFields
		return m, nil
	case tea.KeyShiftTab, tea.KeyUp:
		f.field = (f.field + ruleFormFields - 1) % ruleFormFields
		return m, nil
	case tea.KeyEnter:
		host := strings.TrimSpace(f.host.String())
		if host == "" {
			f.err = "a host or CIDR is required"
			f.field = 1
			return m, nil
		}
		r := NewRule{Sandbox: f.sandbox, Allow: f.allow, Resource: host, Path: strings.TrimSpace(f.path.String())}
		for _, meth := range strings.Split(f.methods.String(), ",") {
			if meth = strings.TrimSpace(meth); meth != "" {
				r.Methods = append(r.Methods, strings.ToUpper(meth))
			}
		}
		m.modal = modalNone
		src := m.src
		return m, m.action("", "", "adding rule for "+r.Resource+"…", func(ctx context.Context) (string, error) {
			return src.AddRule(ctx, r)
		})
	}
	if f.field == 0 {
		switch k.String() {
		case "1", "left", "up", "a":
			f.allow = true
		case "2", "right", "b", "d":
			f.allow = false
		case " ":
			f.allow = !f.allow
		}
		return m, nil
	}
	f.err = ""
	switch f.field {
	case 1:
		f.host.Key(k)
	case 2:
		f.methods.Key(k)
	case 3:
		f.path.Key(k)
	}
	return m, nil
}

func (m Model) openCreate() (tea.Model, tea.Cmd) {
	m.create = createForm{agents: m.src.Agents()}
	m.modal = modalCreate
	return m, nil
}

func (m Model) keyCreate(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.create
	if k.Type == tea.KeyEsc {
		if f.step > 0 {
			f.step--
			return m, nil
		}
		m.modal = modalNone
		return m, nil
	}
	if f.step == 0 {
		switch k.String() {
		case "up", "k":
			if f.agentIdx > 0 {
				f.agentIdx--
			}
		case "down", "j":
			if f.agentIdx < len(f.agents)-1 {
				f.agentIdx++
			}
		case "enter":
			if len(f.agents) > 0 {
				f.step = 1
			}
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyTab, tea.KeyDown, tea.KeyUp, tea.KeyShiftTab:
		f.field = 1 - f.field
		return m, nil
	case tea.KeyEnter:
		ws := strings.TrimSpace(f.workspace.String())
		if ws == "" {
			f.err = "a workspace directory is required"
			return m, nil
		}
		ns := NewSandbox{Agent: f.agents[f.agentIdx].Name, Workspace: ws, Name: strings.TrimSpace(f.name.String())}
		m.modal = modalNone
		src := m.src
		label := ns.Name
		if label == "" {
			label = ns.Agent + " in " + ws
		}
		return m, m.action("", "", "creating "+label+"… (an image pull can take a while)", func(ctx context.Context) (string, error) {
			return src.Create(ctx, ns)
		})
	}
	f.err = ""
	if f.field == 0 {
		f.workspace.Key(k)
	} else {
		f.name.Key(k)
	}
	return m, nil
}

func decisionWord(allow bool) string {
	if allow {
		return "allow"
	}
	return "block"
}

// describeRule is a rule as `boks policy ls` shows it, selector included.
func describeRule(r Rule) string {
	s := r.Resource
	if len(r.Methods) > 0 {
		s += " " + strings.Join(r.Methods, ",")
	}
	if r.Path != "" {
		s += " " + r.Path
	}
	if len(r.Query) > 0 {
		s += " ?" + strings.Join(r.Query, "&")
	}
	return s
}

// textField is a one-line text input: enough for a host, a path or a name.
type textField struct{ r []rune }

func (t *textField) String() string { return string(t.r) }
func (t *textField) Clear()         { t.r = nil }

// Key applies a keypress; it reports whether the key was an edit.
func (t *textField) Key(k tea.KeyMsg) bool {
	switch k.Type {
	case tea.KeyBackspace:
		if len(t.r) > 0 {
			t.r = t.r[:len(t.r)-1]
		}
	case tea.KeyCtrlU:
		t.r = nil
	case tea.KeyCtrlW:
		s := strings.TrimRight(string(t.r), " ")
		if i := strings.LastIndexAny(s, " /.,"); i >= 0 {
			t.r = []rune(s[:i+1])
		} else {
			t.r = nil
		}
	case tea.KeyRunes, tea.KeySpace:
		if k.Paste || k.Type == tea.KeySpace || len(k.Runes) > 0 {
			if k.Type == tea.KeySpace {
				t.r = append(t.r, ' ')
			} else {
				t.r = append(t.r, k.Runes...)
			}
		}
	default:
		return false
	}
	return true
}
