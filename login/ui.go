package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	zone "github.com/lrstanley/bubblezone/v2"
)

type density string

const (
	densityCompact density = "compact"
	densityTight   density = "tight"
	densitySpacey  density = "spacey"
)

type preferences struct {
	Agents, Shells, Resurrect, T3 bool
	Layout                        density
	HistoryMinutes                int
}

func (p *preferences) cycleLayout() {
	switch p.Layout {
	case densityCompact:
		p.Layout = densityTight
	case densityTight:
		p.Layout = densitySpacey
	default:
		p.Layout = densityCompact
	}
}

func (p preferences) historyWindow() time.Duration {
	switch p.HistoryMinutes {
	case 5, 15, 30, 60, 360, 720, 1440:
		return time.Duration(p.HistoryMinutes) * time.Minute
	default:
		return 30 * time.Minute
	}
}

func (p preferences) historyLabel() string {
	switch p.HistoryMinutes {
	case 5:
		return "5m"
	case 15:
		return "15m"
	case 60:
		return "1h"
	case 360:
		return "6h"
	case 720:
		return "12h"
	case 1440:
		return "24h"
	default:
		return "30m"
	}
}

func (p *preferences) cycleHistory() {
	switch p.HistoryMinutes {
	case 5:
		p.HistoryMinutes = 15
	case 15:
		p.HistoryMinutes = 30
	case 30:
		p.HistoryMinutes = 60
	case 60:
		p.HistoryMinutes = 360
	case 360:
		p.HistoryMinutes = 720
	case 720:
		p.HistoryMinutes = 1440
	default:
		p.HistoryMinutes = 5
	}
}

func prefsPath() string { return filepath.Join(stateRoot(), "preferences") }
func readPrefs() preferences {
	p := preferences{Agents: true, T3: true, Layout: densityTight, HistoryMinutes: 30}
	data, _ := os.ReadFile(prefsPath())
	layoutSeen := false
	for _, line := range strings.Split(string(data), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		b := kv[1] == "1"
		switch kv[0] {
		case "agents":
			p.Agents = b
		case "shells":
			p.Shells = b
		case "resurrect":
			p.Resurrect = b
		case "t3":
			p.T3 = b
		case "compact":
			if !layoutSeen {
				if b {
					p.Layout = densityCompact
				} else {
					p.Layout = densityTight
				}
			}
		case "layout":
			switch density(kv[1]) {
			case densityCompact, densityTight, densitySpacey:
				p.Layout = density(kv[1])
				layoutSeen = true
			}
		case "history_minutes":
			if minutes, err := strconv.Atoi(kv[1]); err == nil {
				switch minutes {
				case 5, 15, 30, 60, 360, 720, 1440:
					p.HistoryMinutes = minutes
				}
			}
		}
	}
	return p
}
func savePrefs(p preferences) error {
	dir := stateRoot()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".preferences-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	for _, x := range []struct {
		k string
		v bool
	}{{"agents", p.Agents}, {"shells", p.Shells}, {"resurrect", p.Resurrect}, {"t3", p.T3}} {
		n := 0
		if x.v {
			n = 1
		}
		if _, err = fmt.Fprintf(f, "%s=%d\n", x.k, n); err != nil {
			f.Close()
			return err
		}
	}
	if _, err = fmt.Fprintf(f, "layout=%s\nhistory_minutes=%d\n", p.Layout, int(p.historyWindow()/time.Minute)); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), prefsPath())
}
func (p preferences) includes(s Session) bool {
	if s.Saved && !p.Resurrect {
		return false
	}
	if s.T3 != nil {
		return p.T3
	}
	if s.Kind == "shell" {
		return p.Shells
	}
	return p.Agents
}

type snapshotMsg Snapshot

var mouseZones = zone.New()

type tickMsg time.Time
type ageMsg time.Time

type searchFocus uint8

const (
	searchQuery searchFocus = iota
	searchAgents
	searchShells
	searchSaved
	searchDensity
	searchHistory
	searchT3
	searchResults
)

type model struct {
	snapshot       Snapshot
	prefs          preferences
	input          textinput.Model
	spinner        spinner.Model
	mode           string
	finder         bool
	w, h           int
	settingsCursor int
	searchFocus    searchFocus
	cursor         int
	action         Action
	notice         string
	refreshing     bool
	scheduled      schedulerUI
	t3             t3UI
}

func initialModel(finder bool, notice string) model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "session, topic, command, directory…"
	input.CharLimit = 180
	input.SetVirtualCursor(true)
	loader := spinner.New()
	loader.Spinner = spinner.Dot
	loader.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#ACF1C6"))
	m := model{snapshot: loadCache(), prefs: readPrefs(), input: input, spinner: loader, mode: "home", finder: finder, notice: notice, refreshing: true, scheduled: newSchedulerUI()}
	if finder {
		m.mode = "search"
		m.input.Focus()
	}
	return m
}
func refresh() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s := collect(ctx)
	if s.Err == "" {
		if e := saveCache(s); e != nil {
			s.Err = e.Error()
		}
	}
	return snapshotMsg(s)
}
func (m model) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			if m.mode == "scheduled" {
				return schedulerLoad(m.scheduled.generation)()
			}
			return nil
		},
		refresh,
		m.spinner.Tick,
		tea.Tick(10*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }),
		tea.Tick(time.Second, func(t time.Time) tea.Msg { return ageMsg(t) }),
	)
}
func (m model) listed() []Session {
	query := strings.ToLower(strings.TrimSpace(m.input.Value()))
	var rows []Session
	for _, s := range m.snapshot.Sessions {
		if !m.prefs.includes(s) {
			continue
		}
		if query != "" && m.mode == "search" && !matches(strings.ToLower(s.Name+" "+s.Title+" "+s.Context+" "+s.RunningCommand+" "+s.LastCommand), query) {
			continue
		}
		rows = append(rows, s)
	}
	return rows
}

// Search terms may match anywhere in the full (unclipped) metadata, in any order.
func matches(haystack, query string) bool {
	for _, word := range strings.Fields(query) {
		if !strings.Contains(haystack, word) {
			return false
		}
	}
	return true
}
func (m *model) focusName() string {
	list := m.listed()
	idx := m.cursor
	if m.mode == "home" {
		idx -= len(quickActions)
	}
	if idx >= 0 && idx < len(list) {
		return list[idx].Name
	}
	return ""
}
func (m *model) keepName(name string) {
	if name == "" {
		return
	}
	for i, s := range m.listed() {
		if s.Name == name {
			m.cursor = i
			if m.mode == "home" {
				m.cursor += len(quickActions)
			}
			return
		}
	}
}
func (m *model) clamp() {
	if m.mode == "scheduled" || m.mode == "t3" {
		return
	}
	count := len(m.listed())
	if m.mode == "home" {
		count += len(quickActions)
	}
	if m.mode == "settings" {
		count = 6
	}
	if count == 0 {
		m.cursor = 0
		if m.mode == "search" && m.searchFocus == searchResults {
			m.searchFocus = searchQuery
			m.input.Focus()
		}
		return
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}
func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := message.(t3DetailMsg); ok {
		return m.updateT3Detail(msg)
	}
	if m.mode == "t3" {
		switch msg := message.(type) {
		case tea.KeyPressMsg:
			return m.updateT3Key(msg.String())
		case tea.MouseWheelMsg:
			if msg.Button == tea.MouseWheelUp {
				m.t3.offset = max(0, m.t3.offset-3)
			} else if msg.Button == tea.MouseWheelDown {
				m.t3.offset = min(m.t3MaxOffset(), m.t3.offset+3)
			}
			return m, nil
		case tea.MouseClickMsg:
			return m, nil
		}
	}
	switch message.(type) {
	case schedulerSnapshotMsg, schedulerMutationMsg, schedulerLogMsg, schedulerModelCatalogMsg:
		return m.updateScheduled(message)
	}
	if m.mode == "scheduled" {
		switch msg := message.(type) {
		case tea.WindowSizeMsg, ageMsg, tickMsg, snapshotMsg, spinner.TickMsg:
			// Shared workspace timers and dimensions continue behind this view.
		case tea.KeyPressMsg:
			if msg.String() == "ctrl+c" {
				m.action = Action{Verb: "logout"}
				return m, tea.Quit
			}
			return m.updateScheduled(message)
		default:
			return m.updateScheduled(message)
		}
	}
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.w = msg.Width
		m.h = msg.Height
		m.input.SetWidth(max(8, min(60, m.w-14)))
		m.scheduled.query.SetWidth(max(1, m.w-14))
		if m.w < 60 {
			m.input.Placeholder = "Find sessions…"
		} else {
			m.input.Placeholder = "session, topic, command, directory…"
		}
	case ageMsg:
		// Update the relative snapshot age without collecting metrics again.
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return ageMsg(t) })
	case tickMsg:
		cmd := tea.Tick(10*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
		if m.mode == "t3" && !m.t3.loading {
			cmd = tea.Batch(cmd, m.loadT3Detail(false))
		}
		if m.mode == "scheduled" {
			cmd = tea.Batch(cmd, m.scheduled.refresh())
			if m.scheduled.page == "log" {
				cmd = tea.Batch(cmd, m.scheduled.fetchLog())
			}
		}
		if !m.refreshing {
			m.refreshing = true
			return m, tea.Batch(cmd, refresh, m.spinner.Tick)
		}
		return m, cmd
	case snapshotMsg:
		mode := m.mode
		if mode == "scheduled" || mode == "t3" {
			if mode == "t3" {
				m.mode = m.t3.returnMode
			} else {
				m.mode = m.scheduled.returnMode
			}
			if m.mode != "search" {
				m.mode = "home"
			}
		}
		name := m.focusName()
		m.refreshing = false
		if msg.Err == "" {
			if msg.Warning != "" {
				m.notice = msg.Warning
			} else if m.snapshot.Warning != "" {
				m.notice = ""
			}
			m.snapshot = Snapshot(msg)
			if mode == "t3" {
				for _, s := range m.snapshot.Sessions {
					if s.Name == m.t3.session.Name {
						m.t3.session.Metrics = s.Metrics
						break
					}
				}
			}
			m.keepName(name)
		} else {
			m.notice = msg.Err
		}
		m.clamp()
		m.mode = mode
	case spinner.TickMsg:
		if m.refreshing {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return m.mouseClick(msg)
		}
	case tea.MouseWheelMsg:
		if m.mode != "settings" && mouseZones.Get("sessions").InBounds(msg) {
			if m.mode == "home" && m.cursor < len(quickActions) {
				m.cursor = len(quickActions)
			}
			if m.mode == "search" && len(m.listed()) > 0 {
				m.searchFocus = searchResults
				m.input.Blur()
			}
			if msg.Button == tea.MouseWheelUp {
				m.cursor--
			} else if msg.Button == tea.MouseWheelDown {
				m.cursor++
			}
			m.clamp()
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			m.action = Action{Verb: "logout"}
			return m, tea.Quit
		}
		if (key == "ctrl+j" && (m.mode == "home" || m.mode == "search")) || (key == "a" && m.mode == "home") {
			return m.openScheduledJobs()
		}
		if key == "ctrl+t" && (m.mode == "home" || m.mode == "search") {
			m.cycleHistory()
			return m, nil
		}
		if m.mode == "settings" {
			return m.updateSettings(key)
		}
		if m.mode == "search" {
			return m.updateSearch(msg)
		}
		switch key {
		case "ctrl+g", "ctrl+b", "ctrl+v", "ctrl+y":
			m.toggleFilter(key)
		case "esc":
			m.action = Action{Verb: "logout"}
			return m, tea.Quit
		case "up", "k", "down", "j", "left", "right":
			m.moveHome(key)
		case "enter":
			return m.activate()
		case "/", "f":
			m.mode = "search"
			m.cursor = 0
			m.searchFocus = searchQuery
			m.input.Focus()
		case "p":
			m.openSettings()
		case "o", "r", "c", "l", "s", "h":
			for _, a := range quickActions {
				if a.key == key {
					m.action = Action{Verb: a.verb}
					return m, tea.Quit
				}
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "0":
			i, _ := strconv.Atoi(key)
			if i == 0 {
				i = 10
			}
			rows := m.listed()
			if i-1 < len(rows) {
				return m.openSession(rows[i-1])
			}
		}
	}
	return m, nil
}

func (m *model) cycleHistory() {
	m.prefs.cycleHistory()
	if err := savePrefs(m.prefs); err != nil {
		m.notice = err.Error()
	}
}

func (m model) mouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if (m.mode == "home" || m.mode == "search") && mouseZones.Get("scheduled-entry").InBounds(msg) {
		return m.openScheduledJobs()
	}
	if m.mode == "settings" {
		for i := 0; i < 6; i++ {
			if mouseZones.Get(fmt.Sprintf("setting-%d", i)).InBounds(msg) {
				m.cursor = i
				return m.updateSettings("enter")
			}
		}
		return m, nil
	}
	if m.mode == "search" {
		for i := searchAgents; i <= searchT3; i++ {
			if mouseZones.Get(fmt.Sprintf("search-control-%d", i)).InBounds(msg) {
				m.searchFocus = i
				m.input.Blur()
				m.activateSearchControl()
				return m, nil
			}
		}
		if mouseZones.Get("search-query").InBounds(msg) {
			m.searchFocus = searchQuery
			m.input.Focus()
			return m, nil
		}
	}
	if m.mode == "home" {
		for i := range quickActions {
			if mouseZones.Get(fmt.Sprintf("action-%d", i)).InBounds(msg) {
				m.cursor = i
				return m.activate()
			}
		}
	}
	keys := []string{"ctrl+g", "ctrl+b", "ctrl+v", "ctrl+y"}
	for i, key := range keys {
		if mouseZones.Get(fmt.Sprintf("filter-%d", i)).InBounds(msg) {
			m.toggleFilter(key)
			return m, nil
		}
	}
	for i := range m.listed() {
		if mouseZones.Get(fmt.Sprintf("session-%d", i)).InBounds(msg) {
			if m.mode == "search" && m.searchFocus != searchResults {
				m.searchFocus = searchResults
				m.input.Blur()
				m.cursor = i
				return m, nil
			}
			cursor := i
			if m.mode == "home" {
				cursor += len(quickActions)
			}
			if m.cursor == cursor {
				return m.activate()
			}
			m.cursor = cursor
			return m, nil
		}
	}
	return m, nil
}

// The first six choices form a two-column grid; sessions below remain a list.
func (m *model) moveHome(key string) {
	switch key {
	case "left":
		if m.cursor < len(quickActions) && m.cursor%2 == 1 {
			m.cursor--
		}
	case "right":
		if m.cursor < len(quickActions) && m.cursor%2 == 0 {
			m.cursor++
		}
	case "up", "k":
		if m.cursor >= len(quickActions) {
			if m.cursor == len(quickActions) {
				m.cursor = len(quickActions) - 2
			} else {
				m.cursor--
			}
		} else if m.cursor >= 2 {
			m.cursor -= 2
		}
	case "down", "j":
		if m.cursor < len(quickActions)-2 {
			m.cursor += 2
		} else if m.cursor < len(quickActions) {
			if len(m.listed()) > 0 {
				m.cursor = len(quickActions)
			}
		} else {
			m.cursor++
		}
	}
	m.clamp()
}
func (m *model) activateSearchControl() {
	switch m.searchFocus {
	case searchAgents:
		m.toggleFilter("ctrl+g")
	case searchShells:
		m.toggleFilter("ctrl+b")
	case searchSaved:
		m.toggleFilter("ctrl+v")
	case searchDensity:
		m.prefs.cycleLayout()
		if err := savePrefs(m.prefs); err != nil {
			m.notice = err.Error()
		}
	case searchHistory:
		m.cycleHistory()
	case searchT3:
		m.toggleFilter("ctrl+y")
	}
}

func (m model) updateSearch(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	controls := m.searchFocus >= searchAgents && m.searchFocus <= searchT3
	switch key {
	case "esc":
		if m.finder {
			m.action = Action{Verb: "logout"}
			return m, tea.Quit
		}
		m.mode = "home"
		m.input.SetValue("")
		m.input.Blur()
		m.searchFocus = searchQuery
		m.cursor = 0
		return m, nil
	case "tab":
		m.searchFocus = searchHistory
		m.input.Blur()
		return m, nil
	case "up", "ctrl+p":
		switch {
		case m.searchFocus == searchQuery:
			m.searchFocus = searchHistory
			m.input.Blur()
		case m.searchFocus == searchResults:
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.searchFocus = searchQuery
				m.input.Focus()
			}
		case controls && m.contentWidth() < 90 && m.searchFocus >= searchDensity:
			m.searchFocus -= 2
		}
		return m, nil
	case "down", "ctrl+n":
		switch {
		case controls:
			if m.contentWidth() < 90 && m.searchFocus <= searchSaved {
				m.searchFocus = min(m.searchFocus+3, searchT3)
			} else {
				m.searchFocus = searchQuery
				m.input.Focus()
			}
		case m.searchFocus == searchQuery:
			if len(m.listed()) > 0 {
				m.searchFocus = searchResults
				m.input.Blur()
				m.clamp()
			}
		case m.searchFocus == searchResults:
			m.cursor++
			m.clamp()
		}
		return m, nil
	case "left", "right":
		if controls {
			if key == "left" && m.searchFocus > searchAgents {
				m.searchFocus--
			}
			if key == "right" && m.searchFocus < searchT3 {
				m.searchFocus++
			}
			return m, nil
		}
	case "enter", "space":
		if controls {
			m.activateSearchControl()
			return m, nil
		}
		if key == "enter" {
			return m.activate()
		}
	case "ctrl+g", "ctrl+b", "ctrl+v", "ctrl+y":
		m.toggleFilter(key)
		return m, nil
	}
	if m.searchFocus != searchQuery {
		return m, nil
	}
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != before {
		m.cursor = 0
	}
	return m, cmd
}
func (m *model) toggleFilter(key string) {
	name := ""
	if m.mode != "settings" {
		name = m.focusName()
	}
	switch key {
	case "ctrl+g":
		m.prefs.Agents = !m.prefs.Agents
	case "ctrl+b":
		m.prefs.Shells = !m.prefs.Shells
	case "ctrl+v":
		m.prefs.Resurrect = !m.prefs.Resurrect
	case "ctrl+y":
		m.prefs.T3 = !m.prefs.T3
	}
	if err := savePrefs(m.prefs); err != nil {
		m.notice = err.Error()
	}
	if m.mode != "settings" {
		m.keepName(name)
		m.clamp()
	}
}

func (m *model) openSettings() {
	m.settingsCursor = m.cursor
	m.mode, m.cursor = "settings", 0
}

func (m model) updateSettings(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "p", "tab":
		m.mode = "home"
		m.cursor = m.settingsCursor
		m.clamp()
	case "up", "k":
		if m.cursor >= 2 {
			m.cursor -= 2
		}
	case "down", "j":
		if m.cursor < 4 {
			m.cursor += 2
		}
	case "left":
		if m.cursor%2 == 1 {
			m.cursor--
		}
	case "right":
		if m.cursor%2 == 0 && m.cursor < 5 {
			m.cursor++
		}
	case "space", "enter":
		switch m.cursor {
		case 3:
			m.prefs.cycleLayout()
		case 4:
			m.prefs.cycleHistory()
		case 5:
			m.toggleFilter("ctrl+y")
			return m, nil
		default:
			keys := []string{"ctrl+g", "ctrl+b", "ctrl+v"}
			m.toggleFilter(keys[m.cursor])
			return m, nil
		}
		if err := savePrefs(m.prefs); err != nil {
			m.notice = err.Error()
		}
	}
	return m, nil
}
func (m model) activate() (tea.Model, tea.Cmd) {
	if m.mode == "home" && m.cursor < len(quickActions) {
		m.action = Action{Verb: quickActions[m.cursor].verb}
		return m, tea.Quit
	}
	idx := m.cursor
	if m.mode == "home" {
		idx -= len(quickActions)
	}
	rows := m.listed()
	if idx >= 0 && idx < len(rows) {
		return m.openSession(rows[idx])
	}
	return m, nil
}

type quickAction struct{ key, verb, label, short string }

var quickActions = []quickAction{
	{"o", "omp-new", "OMP · new", "OMP new"},
	{"r", "omp-last", "OMP · resume", "OMP last"},
	{"c", "claude-new", "Claude · new", "Claude new"},
	{"l", "claude-last", "Claude · resume", "Claude last"},
	{"s", "shell-new", "Zellij shell", "Zellij"},
	{"h", "plain-shell", "Plain shell", "Shell"},
}
