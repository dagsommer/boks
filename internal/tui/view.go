package tui

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// Below these the dashboard drops the right pane, and below the second it gives up and
// says so: a layout squeezed past the point of legibility is worse than a message.
const (
	minPaneWidth = 90
	minWidth     = 30
	minHeight    = 8

	cardWidth  = 45
	cardHeight = 9
)

type styles struct {
	r *lipgloss.Renderer

	border, borderSel, cardTitle, name, faint, text, bold   lipgloss.Style
	green, yellow, red, blue, tabActive, sep, key, errStyle lipgloss.Style
	paneBar, paneBarFocus                                   lipgloss.Style
}

func newStyles(noColor bool) styles {
	r := lipgloss.DefaultRenderer()
	if noColor {
		r = lipgloss.NewRenderer(io.Discard)
		r.SetColorProfile(termenv.Ascii)
	}
	c := func(light, dark string) lipgloss.AdaptiveColor {
		return lipgloss.AdaptiveColor{Light: light, Dark: dark}
	}
	s := styles{r: r}
	s.border = r.NewStyle().Foreground(c("248", "240"))
	s.borderSel = r.NewStyle().Foreground(c("26", "75"))
	s.cardTitle = r.NewStyle().Bold(true).Foreground(c("98", "141"))
	s.name = r.NewStyle().Bold(true).Foreground(c("239", "254"))
	s.faint = r.NewStyle().Foreground(c("60", "246"))
	s.text = r.NewStyle().Foreground(c("240", "252"))
	s.bold = r.NewStyle().Bold(true)
	s.green = r.NewStyle().Foreground(c("22", "71"))
	s.yellow = r.NewStyle().Foreground(c("136", "179"))
	s.red = r.NewStyle().Foreground(c("88", "167"))
	s.blue = r.NewStyle().Foreground(c("26", "75"))
	s.tabActive = r.NewStyle().Bold(true).Underline(true).Foreground(c("26", "75"))
	s.sep = r.NewStyle().Foreground(c("248", "240"))
	s.key = r.NewStyle().Bold(true).Foreground(c("239", "254"))
	s.errStyle = r.NewStyle().Foreground(c("88", "167"))
	s.paneBar = r.NewStyle().Foreground(c("248", "240"))
	s.paneBarFocus = r.NewStyle().Bold(true).Foreground(c("31", "75"))
	return s
}

// View renders the dashboard.
func (m Model) View() string {
	w, h := m.width, m.height
	if w < minWidth || h < minHeight {
		return truncRight(fmt.Sprintf("boks: the terminal is %dx%d; the dashboard needs %dx%d.", w, h, minWidth, minHeight), w)
	}
	st := m.st

	lines := m.header(st)
	bodyH := h - len(lines) - 2
	var body []string
	if m.view == viewNetwork {
		body = m.networkView(st, w, bodyH)
	} else {
		body = m.dashboard(st, w, bodyH)
	}
	// Every part is cut to the height it was given, so the screen is exactly h lines and
	// nothing scrolls the header away.
	body = body[:min(len(body), max(bodyH, 0))]
	for len(body) < bodyH {
		body = append(body, "")
	}
	lines = append(lines, body...)
	lines = append(lines, m.statusLine(st, w), m.hintLine(st, w))

	switch m.modal {
	case modalConfirm:
		lines = overlay(lines, m.confirmBox(st, w), w)
	case modalAddRule:
		lines = overlay(lines, m.ruleFormBox(st, w), w)
	case modalCreate:
		lines = overlay(lines, m.createBox(st, w), w)
	case modalSettings:
		lines = overlay(lines, m.settingsBox(st, w), w)
	case modalPicker:
		lines = overlayAt(lines, m.pickerBox(st, w), 2, 5, w)
	}
	for i := range lines {
		lines[i] = cutTo(lines[i], w)
	}
	return strings.Join(lines, "\n")
}

func (m Model) header(st styles) []string {
	w := m.width
	inner := w - 4
	title := st.name.Render("Boks") + "  " + st.faint.Render("Run untrusted developer tooling in isolated microVMs")
	settings := st.faint.Render("(Ctrl+s)") + " " + st.bold.Render("⚙")
	if m.view == viewNetwork {
		esc := " " + st.faint.Render("❮ esc") + " "
		escW := 7
		rest := inner - escW - 1
		top := " " + st.border.Render("╭"+strings.Repeat("─", escW)+"┬"+strings.Repeat("─", max(rest+1, 0))+"╮")
		mid := " " + st.border.Render("│") + esc + st.border.Render("│") + " " + spread(title, settings, max(rest-1, 0)) + " " + st.border.Render("│")
		bot := " " + st.border.Render("╰"+strings.Repeat("─", escW)+"┴"+strings.Repeat("─", max(rest+1, 0))+"╯")
		return []string{top, mid, bot}
	}
	top := " " + st.border.Render("╭"+strings.Repeat("─", max(inner, 0))+"╮")
	mid := " " + st.border.Render("│") + " " + spread(title, settings, max(inner-2, 0)) + " " + st.border.Render("│")
	bot := " " + st.border.Render("╰"+strings.Repeat("─", max(inner, 0))+"╯")
	return []string{top, mid, bot}
}

func (m Model) dashboard(st styles, w, h int) []string {
	showPane := w >= minPaneWidth
	cardW := cardWidth
	if !showPane {
		cardW = w - 2
	}
	left := m.cards(st, cardW, h)
	if !showPane {
		out := make([]string, h)
		for i := range out {
			if i < len(left) {
				out[i] = " " + left[i]
			}
		}
		return out
	}
	paneW := w - 1 - cardW - 3
	pane := m.pane(st, paneW, h)
	bar := st.paneBar.Render("│")
	if m.focus == focusPane {
		bar = st.paneBarFocus.Render("┃")
	}
	out := make([]string, h)
	for i := 0; i < h; i++ {
		l := strings.Repeat(" ", cardW)
		if i < len(left) {
			l = cutTo(left[i], cardW)
		}
		p := ""
		if i < len(pane) {
			p = pane[i]
		}
		out[i] = " " + l + " " + bar + " " + p
	}
	return out
}

// cards renders the left column, scrolled so the selected card is on screen.
func (m Model) cards(st styles, w, h int) []string {
	if !m.loaded {
		return []string{st.faint.Render("reading sandboxes…")}
	}
	if m.loadErr != nil && len(m.snap.Sandboxes) == 0 {
		return wrap(st.errStyle, "could not list sandboxes: "+m.loadErr.Error(), w)
	}
	if len(m.snap.Sandboxes) == 0 {
		msg := []string{st.text.Render("No sandboxes yet."), "", st.faint.Render("c creates one, or run")}
		msg = append(msg, st.faint.Render("'boks run AGENT DIR' in a terminal."))
		if m.snap.Notice != "" {
			msg = append(msg, "")
			msg = append(msg, wrap(st.faint, m.snap.Notice, w)...)
		}
		return msg
	}
	per := cardHeight + 1
	visible := max(h/per, 1)
	sel := max(m.selectedIndex(), 0)
	first := visibleWindow(0, sel, len(m.snap.Sandboxes), visible)
	// Keep the first card's offset stable while the selection stays inside the window.
	var out []string
	for i := first; i < len(m.snap.Sandboxes) && len(out) < h; i++ {
		out = append(out, m.card(st, m.snap.Sandboxes[i], i == sel, w)...)
		out = append(out, "")
	}
	if len(out) > h {
		out = out[:h]
	}
	return out
}

func (m Model) card(st styles, s Sandbox, selected bool, w int) []string {
	bs := st.border
	if selected {
		bs = st.borderSel
	}
	tl, tr, bl, br, hz, vt := "╭", "╮", "╰", "╯", "─", "│"
	if selected && m.noColor {
		tl, tr, bl, br, hz, vt = "┏", "┓", "┗", "┛", "━", "┃"
	}
	inner := w - 4
	title := " Sandbox "
	top := bs.Render(tl+strings.Repeat(hz, max(w-2-width(title), 0))) + st.cardTitle.Render(title) + bs.Render(tr)
	line := func(content string) string {
		return bs.Render(vt) + " " + cutTo(content, inner) + " " + bs.Render(vt)
	}
	rule := line(st.faint.Render(strings.Repeat("─", inner)))

	var marks []string
	if s.Mode == "clone" {
		marks = append(marks, "clone")
	}
	if s.Ephemeral {
		marks = append(marks, "--rm")
	}
	nameLine := spread(st.yellow.Render("■")+" "+st.name.Render(truncRight(s.Name, inner-2-width(strings.Join(marks, " "))-1)),
		st.faint.Render(strings.Join(marks, " ")), inner)

	agent := s.Agent
	if agent == "" {
		agent = "-"
	}
	agentLine := st.name.Render(agent)
	if selected {
		agentLine = spread(agentLine, st.blue.Render("↵")+st.faint.Render(" run"), inner)
	}
	ws := s.Workspace
	if ws == "" {
		ws = "-"
	}
	state := s.Status
	stateStyle := st.yellow
	if s.Running() {
		stateStyle = st.green
	}
	if verb := m.busy[s.Name]; verb != "" {
		state = verb + "…"
		stateStyle = st.blue
	}
	stateLine := stateStyle.Render(state)
	if s.Ports != "" {
		stateLine += st.faint.Render(" · " + s.Ports)
	}

	startStop := "start"
	if s.Running() {
		startStop = "stop"
	}
	third := (inner - 2) / 3
	act := func(word string, hot int, style lipgloss.Style) string {
		r := []rune(word)
		return style.Render(string(r[:hot])) + st.key.Render(string(r[hot])) + style.Render(string(r[hot+1:]))
	}
	actions := " " + padRight(act(startStop, 0, st.green), third) +
		centre(act("exec", 1, st.blue), third) +
		padLeft(act("remove", 0, st.red), inner-2-2*third) + " "

	return []string{
		top,
		line(nameLine),
		rule,
		line(agentLine),
		line(st.faint.Render(truncLeft(ws, inner))),
		line(stateLine),
		rule,
		line(actions),
		bs.Render(bl + strings.Repeat(hz, max(w-2, 0)) + br),
	}
}

func centre(s string, w int) string {
	n := width(s)
	if n >= w {
		return truncRight(s, w)
	}
	l := (w - n) / 2
	return strings.Repeat(" ", l) + s + strings.Repeat(" ", w-n-l)
}

// pane renders the right-hand side of the dashboard: the selected sandbox's network.
func (m Model) pane(st styles, w, h int) []string {
	s, ok := m.Selected()
	if !ok {
		return nil
	}
	out := []string{st.name.Render(s.Name), ""}
	out = append(out, m.tabs(st, w)...)
	out = append(out, m.tableArea(st, w, h-len(out)-2)...)
	for len(out) < h-2 {
		out = append(out, "")
	}
	out = append(out, st.sep.Render(strings.Repeat("─", max(w, 0))), m.paneFooter(st, false))
	return out
}

func (m Model) networkView(st styles, w, h int) []string {
	inner := w - 3
	out := []string{"", "   " + st.name.Render(filterLabel(m.netFilter)) + " " + st.faint.Render("▾"), ""}
	tabs := m.tabs(st, inner)
	for _, t := range tabs {
		out = append(out, "  "+t)
	}
	for _, l := range m.tableArea(st, inner, h-len(out)-2) {
		out = append(out, "  "+l)
	}
	for len(out) < h-2 {
		out = append(out, "")
	}
	out = append(out, "  "+st.sep.Render(strings.Repeat("─", max(inner, 0))), "  "+m.paneFooter(st, true))
	return out
}

func (m Model) tabs(st styles, w int) []string {
	names := []string{
		fmt.Sprintf("Network Log (%d)", len(m.scopeLog())),
		fmt.Sprintf("Network Rules (%d)", len(m.scopeRules())),
	}
	var parts []string
	for i, n := range names {
		if tabKind(i) == m.tab && m.paneFocused() {
			parts = append(parts, st.tabActive.Render(n))
		} else if tabKind(i) == m.tab {
			parts = append(parts, st.bold.Render(n))
		} else {
			parts = append(parts, st.faint.Render(n))
		}
	}
	return []string{strings.Join(parts, "    "), st.sep.Render(strings.Repeat("─", max(w, 0)))}
}

func (m Model) paneFooter(st styles, withSandboxes bool) string {
	allowed, blocked := 0, 0
	for _, e := range m.scopeLog() {
		if e.Allowed {
			allowed++
		} else {
			blocked++
		}
	}
	s := fmt.Sprintf("%d allowed entries  ·  %d blocked entries", allowed, blocked)
	if withSandboxes {
		running := 0
		for _, sb := range m.snap.Sandboxes {
			if sb.Running() {
				running++
			}
		}
		s = fmt.Sprintf("%d sandbox(es), %d running  ·  ", len(m.snap.Sandboxes), running) + s
	}
	return st.faint.Render(s)
}

// tableArea is the content under the tabs, h lines tall at most.
func (m Model) tableArea(st styles, w, h int) []string {
	if h <= 0 || w <= 0 {
		return nil
	}
	var out []string
	if m.tab == tabLog {
		out = m.logTable(st, w, h)
	} else {
		out = m.rulesTable(st, w, h)
	}
	if len(out) > h {
		out = out[:h]
	}
	return out
}

// logColumns are the network log's columns: sbx's Date, Host, Hits and Status, with a
// Sandbox column when more than one sandbox can appear.
func logColumns(withSandbox bool) []column {
	cols := []column{{title: "Date ↑", width: 18}}
	if withSandbox {
		cols = append(cols, column{title: "Sandbox", width: 14, flex: true})
	}
	return append(cols,
		column{title: "Host", width: 16, flex: true},
		column{title: "", width: 6},
		column{title: "Hits", width: 5, right: true},
		column{title: "Status", width: 7, right: true},
	)
}

func (m Model) logTable(st styles, w, h int) []string {
	all := m.scopeLog()
	allowed := 0
	for _, e := range all {
		if e.Allowed {
			allowed++
		}
	}
	summary := st.faint.Render(fmt.Sprintf("%d total", len(all))) + "  " +
		st.green.Render(fmt.Sprintf("● %d allowed", allowed)) + "  " +
		st.red.Render(fmt.Sprintf("● %d blocked", len(all)-allowed))
	summary += m.filterNote(st, "allowed", "blocked")
	out := []string{"", summary, ""}

	rows := m.LogRows()
	plain := st.r.NewStyle()
	cols := logColumns(m.showSandboxColumn())
	ws := layout(cols, w, 2)
	out = append(out, m.tableHeader(st, cols, ws, w), st.sep.Render(strings.Repeat("─", max(w, 0))))
	if len(rows) == 0 {
		msg := "No network activity recorded yet."
		if len(all) > 0 {
			msg = "Nothing matches the filter."
		}
		return append(out, st.faint.Render(msg))
	}
	space := h - len(out)
	first := visibleWindow(m.offset, m.row, len(rows), space)
	for i := first; i < len(rows) && i < first+space; i++ {
		e := rows[i]
		sel := i == m.row && m.paneFocused()
		dot := st.green.Render("●")
		status := cell{"Allowed", st.green.Bold(true)}
		if !e.Allowed {
			dot = st.red.Render("●")
			status = cell{"Blocked", st.red.Bold(true)}
		}
		if sel && m.noColor {
			dot = "▸"
		}
		hostStyle := st.faint
		action := cell{"", st.faint}
		if sel {
			hostStyle = st.name
			if e.Allowed {
				action = cell{"block", st.red}
			} else {
				action = cell{"allow", st.green}
			}
		}
		cells := []cell{{dot + " " + st.faint.Render(formatDate(e.Last)), plain}}
		if m.showSandboxColumn() {
			sb := e.Sandbox
			if sb == "" {
				sb = "-"
			}
			cells = append(cells, cell{sb, st.faint})
		}
		cells = append(cells, cell{e.Host, hostStyle}, action, cell{strconv.Itoa(e.Hits), st.faint}, status)
		out = append(out, renderCells(cells, cols, ws, w))
	}
	return out
}

// renderCells is renderRow for cells that may already carry escape sequences of their own
// (the coloured dot): those are passed through rather than measured as text.
func renderCells(cells []cell, cols []column, ws []int, w int) string {
	return renderRow(cells, cols, ws, 2, w)
}

func (m Model) filterNote(st styles, a, b string) string {
	switch m.status {
	case statusAllowed:
		return st.faint.Render("   · showing " + a + " only (o)")
	case statusBlocked:
		return st.faint.Render("   · showing " + b + " only (o)")
	}
	if q := m.search.String(); q != "" || m.searching {
		return st.faint.Render("   · search: " + q)
	}
	return ""
}

func (m Model) tableHeader(st styles, cols []column, ws []int, w int) string {
	cells := make([]cell, len(cols))
	for i, c := range cols {
		cells[i] = cell{c.title, st.faint.Bold(true)}
	}
	return renderRow(cells, cols, ws, 2, w)
}

// ruleColumns are the network rules' columns. sbx has Proto, Hosts, Values and Type; Boks
// rules have no protocol, so the first column says where a rule comes from instead, and
// an HTTP column appears when an HTTP rule is among the rows.
func ruleColumns(withScope, withHTTP bool) []column {
	title := "Source"
	if withScope {
		title = "Scope"
	}
	cols := []column{{title: title, width: 22}, {title: "Hosts", width: 16, flex: true}}
	if withScope {
		cols[0].width = 34
	}
	if withHTTP {
		cols = append(cols, column{title: "HTTP", width: 12, flex: true})
	}
	return append(cols,
		column{title: "", width: 6},
		column{title: "Type", width: 5, right: true},
	)
}

func (m Model) rulesTable(st styles, w, h int) []string {
	rows := m.RuleRows()
	note := m.filterNote(st, "allow rules", "block rules")
	out := []string{""}
	if note != "" {
		out = append(out, strings.TrimSpace(note), "")
	} else {
		out = append(out, "")
	}
	withHTTP := false
	for _, r := range rows {
		withHTTP = withHTTP || r.IsHTTP()
	}
	plain := st.r.NewStyle()
	cols := ruleColumns(m.showSandboxColumn(), withHTTP)
	ws := layout(cols, w, 2)
	out = append(out, m.tableHeader(st, cols, ws, w), st.sep.Render(strings.Repeat("─", max(w, 0))))
	if len(rows) == 0 {
		return append(out, st.faint.Render("No rules. a adds one."))
	}
	space := h - len(out)
	first := visibleWindow(m.offset, m.row, len(rows), space)
	for i := first; i < len(rows) && i < first+space; i++ {
		r := rows[i]
		sel := i == m.row && m.paneFocused()
		dot, typ := st.green.Render("●"), cell{"Allow", st.green}
		if !r.Allow {
			dot, typ = st.red.Render("●"), cell{"Block", st.red}
		}
		if sel && m.noColor {
			dot = "▸"
		}
		hostStyle := st.faint
		action := cell{"", st.faint}
		if sel {
			hostStyle = st.name
			if r.Stored {
				action = cell{"delete", st.red}
			}
		}
		http := ""
		if r.IsHTTP() {
			http = strings.TrimSpace(strings.TrimPrefix(describeRule(r), r.Resource))
		}
		cells := []cell{{dot + " " + st.faint.Render(r.Scope), plain}, {r.Resource, hostStyle}}
		if withHTTP {
			cells = append(cells, cell{http, st.faint})
		}
		cells = append(cells, action, typ)
		out = append(out, renderRow(cells, cols, ws, 2, w))
	}
	return out
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("02 Jan, 15:04:05")
}

func (m Model) statusLine(st styles, w int) string {
	if m.statusMsg == "" {
		if m.loadErr != nil && len(m.snap.Sandboxes) > 0 {
			return " " + st.errStyle.Render(truncRight("refresh failed: "+m.loadErr.Error(), w-2))
		}
		return ""
	}
	style := st.faint
	if m.statusErr {
		style = st.errStyle
	}
	return " " + style.Render(truncRight(m.statusMsg, w-2))
}

func (m Model) hints() []string {
	nav := "↑/↓/←/→/Tab navigate"
	switch {
	case m.searching:
		return []string{"type to filter", "Enter done", "Esc clear"}
	case m.view == viewNetwork && m.tab == tabLog:
		return []string{"^c quit", nav, "/ search", "s sandbox", "o status", "Enter allow/block", "Esc back"}
	case m.view == viewNetwork:
		return []string{"^c quit", nav, "/ search", "s sandbox", "o type", "a add rule", "r remove rule", "Esc back"}
	case m.focus == focusPane && m.tab == tabLog:
		return []string{"^c quit", nav, "/ search", "o status", "Enter allow/block", "n network"}
	case m.focus == focusPane:
		return []string{"^c quit", nav, "/ search", "o type", "a add rule", "r remove rule", "n network"}
	}
	return []string{"^c quit", nav, "Enter run", "c create", "n network"}
}

func (m Model) hintLine(st styles, w int) string {
	var parts []string
	for _, h := range m.hints() {
		k, rest, _ := strings.Cut(h, " ")
		parts = append(parts, st.key.Render(k)+" "+st.faint.Render(rest))
	}
	version := m.info.Version
	if version != "" && version[0] != 'v' && version[0] >= '0' && version[0] <= '9' {
		version = "v" + version
	}
	return " " + spread(strings.Join(parts, "  "), st.faint.Render(version), w-2)
}

// Modals.

// box draws a modal: a rounded border around lines, w cells wide.
func box(st styles, title string, lines []string, w int) []string {
	inner := w - 4
	out := []string{st.border.Render("╭" + strings.Repeat("─", max(w-2, 0)) + "╮")}
	row := func(s string) string {
		return st.border.Render("│") + " " + cutTo(s, inner) + " " + st.border.Render("│")
	}
	out = append(out, row(padLeft(st.faint.Render("✕"), inner)))
	if title != "" {
		out = append(out, row(" "+st.name.Render(title)), row(" "+st.sep.Render(strings.Repeat("─", max(inner-2, 0)))))
	}
	for _, l := range lines {
		out = append(out, row(" "+l))
	}
	out = append(out, row(""), st.border.Render("╰"+strings.Repeat("─", max(w-2, 0))+"╯"))
	return out
}

func modalWidth(w, want int) int {
	if want > w-4 {
		want = w - 4
	}
	return max(want, 20)
}

func (m Model) confirmBox(st styles, w int) []string {
	bw := modalWidth(w, 76)
	var lines []string
	lines = append(lines, "")
	for _, l := range m.confirm.lines {
		lines = append(lines, wrap(st.text, l, bw-6)...)
	}
	lines = append(lines, "", st.red.Bold(true).Render("y")+st.faint.Render(" yes, remove    ")+
		st.key.Render("n")+st.faint.Render("/Esc keep it"))
	return box(st, m.confirm.title, lines, bw)
}

func (m Model) ruleFormBox(st styles, w int) []string {
	bw := modalWidth(w, 76)
	f := m.ruleForm
	scope := "global (every sandbox)"
	if f.sandbox != "" {
		scope = "sandbox:" + f.sandbox
	}
	marker := func(i int) string {
		if f.field == i {
			return st.blue.Render("▸ ")
		}
		return "  "
	}
	radio := func(on bool) string {
		if on {
			return "●"
		}
		return "○"
	}
	field := func(t textField, placeholder string, focused bool) string {
		if t.String() == "" && !focused {
			return st.faint.Render(placeholder)
		}
		s := t.String()
		if focused {
			s += "█"
		}
		return st.name.Render(s)
	}
	lines := []string{
		"",
		st.faint.Render("Scope: ") + st.text.Render(scope),
		"",
		marker(0) + "Rule Type:",
		"    [1] " + radio(f.allow) + " Allow — permit connections to this host",
		"    [2] " + radio(!f.allow) + " Block — refuse connections to this host",
		"",
		marker(1) + "Host or CIDR:",
		"    " + field(f.host, "example.com, *.github.com:443 or 10.0.0.0/24", f.field == 1),
		"",
		marker(2) + "HTTP methods (optional, makes it an HTTP rule):",
		"    " + field(f.methods, "GET,HEAD or ANY", f.field == 2),
		marker(3) + "HTTP path (optional):",
		"    " + field(f.path, "/v1/**", f.field == 3),
	}
	if f.err != "" {
		lines = append(lines, "", st.errStyle.Render(f.err))
	}
	lines = append(lines, "", st.faint.Render("1-2 type  Tab next field  Enter add  Esc cancel"))
	return box(st, "Add Network Rule", lines, bw)
}

func (m Model) createBox(st styles, w int) []string {
	bw := modalWidth(w, 90)
	f := m.create
	var lines []string
	if f.step == 0 {
		lines = append(lines, st.faint.Render("1/2  Choose the agent for the sandbox."), "", st.text.Render("Available agents"))
		maxRows := max(m.height-16, 3)
		first := visibleWindow(0, f.agentIdx, len(f.agents), maxRows)
		for i := first; i < len(f.agents) && i < first+maxRows; i++ {
			a := f.agents[i]
			prefix := "  "
			name := st.text.Render(a.Name)
			if i == f.agentIdx {
				prefix = st.blue.Render("▸ ")
				name = st.name.Render(a.Name)
			}
			lines = append(lines, prefix+padRight(name, 14)+" "+st.faint.Render(truncRight(a.Summary, bw-24)))
		}
		lines = append(lines, "", padLeft(st.faint.Render("↑/↓ choose   Next ↵"), bw-6))
		return box(st, "Create Sandbox", lines, bw)
	}
	agent := f.agents[f.agentIdx].Name
	marker := func(i int) string {
		if f.field == i {
			return st.blue.Render("▸ ")
		}
		return "  "
	}
	val := func(t textField, placeholder string, focused bool) string {
		if t.String() == "" && !focused {
			return st.faint.Render(placeholder)
		}
		s := t.String()
		if focused {
			s += "█"
		}
		return st.name.Render(truncLeft(s, bw-12))
	}
	lines = append(lines,
		st.faint.Render("2/2  Where should "+agent+" work?"), "",
		marker(0)+"Workspace directory:",
		"    "+val(f.workspace, "/path/to/project (shared into the sandbox at the same path)", f.field == 0),
		"",
		marker(1)+"Name (optional):",
		"    "+val(f.name, agent+"-<directory>", f.field == 1),
	)
	if f.err != "" {
		lines = append(lines, "", st.errStyle.Render(f.err))
	}
	lines = append(lines, "", st.faint.Render("Runs 'boks create "+agent+" DIR'. Tab next field  Esc back"),
		padLeft(st.faint.Render("Create ↵"), bw-6))
	return box(st, "Create Sandbox", lines, bw)
}

func (m Model) settingsBox(st styles, w int) []string {
	bw := modalWidth(w, 72)
	row := func(k, v string) string { return spread(st.text.Render(k), st.faint.Render(truncLeft(v, bw-30)), bw-6) }
	colour := "on"
	if m.noColor {
		colour = "off (NO_COLOR)"
	}
	lines := []string{
		"",
		row("Version", m.info.Version),
		row("State directory", m.info.StateDir),
		row("containerd", m.info.Address),
		row("Refresh", m.interval.String()),
		row("Colour", colour),
		"",
		st.faint.Render("Boks has no settings of its own to change here; the"),
		st.faint.Render("environment variables in docs/usage.md configure it."),
		"",
		padLeft(st.faint.Render("Close ↵"), bw-6),
	}
	return box(st, "Settings", lines, bw)
}

func (m Model) pickerBox(st styles, w int) []string {
	longest := 20
	for _, o := range m.picker.options {
		longest = max(longest, width(filterLabel(o)))
	}
	bw := min(longest+6, w-4)
	inner := bw - 4
	maxRows := max(m.height-10, 3)
	first := visibleWindow(0, m.picker.idx, len(m.picker.options), maxRows)
	out := []string{st.border.Render("╭" + strings.Repeat("─", max(bw-2, 0)) + "╮")}
	for i := first; i < len(m.picker.options) && i < first+maxRows; i++ {
		label := filterLabel(m.picker.options[i])
		prefix := "  "
		if i == m.picker.idx {
			prefix = st.blue.Render("● ")
			label = st.name.Render(label)
		}
		out = append(out, st.border.Render("│")+" "+cutTo(prefix+label, inner)+" "+st.border.Render("│"))
	}
	out = append(out, st.border.Render("╰"+strings.Repeat("─", max(bw-2, 0))+"╯"))
	return out
}

// overlay draws box centred over lines.
func overlay(lines, boxLines []string, w int) []string {
	bw := 0
	for _, l := range boxLines {
		bw = max(bw, width(l))
	}
	x := max((w-bw)/2, 0)
	y := max((len(lines)-len(boxLines))/2, 0)
	return overlayAt(lines, boxLines, x, y, w)
}

func overlayAt(lines, boxLines []string, x, y, w int) []string {
	out := append([]string(nil), lines...)
	for i, b := range boxLines {
		row := y + i
		if row >= len(out) {
			break
		}
		base := out[row]
		left := cutTo(ansi.Truncate(base, x, ""), x)
		bw := width(b)
		right := ""
		if width(base) > x+bw {
			right = ansi.TruncateLeft(base, x+bw, "")
		}
		out[row] = cutTo(left+b+right, w)
	}
	return out
}

// wrap breaks text into lines of at most w cells at spaces.
func wrap(style lipgloss.Style, text string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			for width(word) > w {
				if line != "" {
					out = append(out, style.Render(line))
					line = ""
				}
				out = append(out, style.Render(ansi.Truncate(word, w, "")))
				word = ansi.TruncateLeft(word, w, "")
			}
			switch {
			case line == "":
				line = word
			case width(line)+1+width(word) <= w:
				line += " " + word
			default:
				out = append(out, style.Render(line))
				line = word
			}
		}
		out = append(out, style.Render(line))
	}
	return out
}
