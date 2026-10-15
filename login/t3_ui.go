package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type t3UI struct {
	session    Session
	messages   []t3Message
	returnMode string
	err        string
	offset     int
	loading    bool
	generation uint64
}

type t3DetailMsg struct {
	id         string
	generation uint64
	session    Session
	messages   []t3Message
	err        error
	tail       bool
}

func (m model) openSession(s Session) (tea.Model, tea.Cmd) {
	if s.T3 != nil {
		m.t3 = t3UI{session: s, returnMode: m.mode, generation: m.t3.generation + 1}
		m.mode = "t3"
		markUsed(s.Name)
		return m, m.loadT3Detail(true)
	}
	verb := "attach"
	if m.finder {
		verb = "switch"
	}
	m.action = Action{Verb: verb, Name: s.Name, PaneID: s.PaneID, Saved: s.Saved}
	return m, tea.Quit
}

func (m *model) loadT3Detail(tail bool) tea.Cmd {
	m.t3.loading = true
	id, generation := m.t3.session.T3.ID, m.t3.generation
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := findT3(ctx, id)
		var messages []t3Message
		if err == nil {
			messages, err = loadT3Messages(ctx, id)
		}
		return t3DetailMsg{id, generation, s, messages, err, tail}
	}
}

func (m model) updateT3Detail(msg t3DetailMsg) (tea.Model, tea.Cmd) {
	if m.mode != "t3" || m.t3.session.T3 == nil || msg.id != m.t3.session.T3.ID || msg.generation != m.t3.generation {
		return m, nil
	}
	atTail := m.t3.offset >= m.t3MaxOffset()
	m.t3.loading = false
	m.t3.err = ""
	if msg.err != nil {
		m.t3.err = clean(msg.err.Error())
		return m, nil
	}
	for _, s := range m.snapshot.Sessions {
		if s.Name == msg.session.Name {
			msg.session.Metrics = s.Metrics
			break
		}
	}
	m.t3.session, m.t3.messages = msg.session, msg.messages
	if msg.tail || atTail {
		m.t3.offset = m.t3MaxOffset()
	} else {
		m.t3.offset = min(m.t3.offset, m.t3MaxOffset())
	}
	return m, nil
}

func (m model) updateT3Key(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "ctrl+c":
		m.action = Action{Verb: "logout"}
		return m, tea.Quit
	case "esc":
		m.mode = m.t3.returnMode
		return m, nil
	case "ctrl+r":
		if !m.t3.loading {
			return m, m.loadT3Detail(false)
		}
	case "r":
		if m.t3.loading || m.t3.err != "" {
			return m, nil
		}
		if reason := m.t3.session.T3.resumeBlocked(); reason != "" {
			m.t3.err = reason
			return m, nil
		}
		m.action = Action{Verb: "t3-resume", Name: m.t3.session.T3.ID}
		return m, tea.Quit
	case "up", "k":
		m.t3.offset = max(0, m.t3.offset-1)
	case "down", "j":
		m.t3.offset = min(m.t3MaxOffset(), m.t3.offset+1)
	case "pgup":
		m.t3.offset = max(0, m.t3.offset-m.t3BodyRows())
	case "pgdown":
		m.t3.offset = min(m.t3MaxOffset(), m.t3.offset+m.t3BodyRows())
	case "home":
		m.t3.offset = 0
	case "end":
		m.t3.offset = m.t3MaxOffset()
	}
	return m, nil
}

func (m model) t3Lines(width int) []string {
	var lines []string
	for _, msg := range m.t3.messages {
		heading := strings.ToUpper(msg.Role) + " · " + scheduledTime(msg.Created)
		if msg.Streaming != 0 {
			heading += " · streaming"
		}
		lines = append(lines, schedulerWrap(heading+"\n"+msg.Text, width)...)
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		lines = []string{"No messages yet."}
	}
	return lines
}

func (m model) t3BodyRows() int { return max(1, m.h-13) }
func (m model) t3MaxOffset() int {
	return max(0, len(m.t3Lines(m.contentWidth()))-m.t3BodyRows())
}

func (m model) t3View(width int) string {
	t := m.t3.session.T3
	heading := "T3 · " + sessionTitle(m.t3.session)
	status := t.Project + " · " + t.Provider + " · " + t.Status
	if t.PendingApprovals+t.PendingInputs > 0 {
		status += fmt.Sprintf(" · %d approvals / %d inputs", t.PendingApprovals, t.PendingInputs)
	}
	info := t.Cwd
	if m.h >= 18 {
		info += "\nT3 " + t.ID + "\nConversation " + t.Conversation
	}
	if t.LastError != "" {
		info += "\n" + t.LastError
	}
	infoLines := schedulerWrap(info, width)
	infoLines = infoLines[:min(len(infoLines), min(4, max(1, m.h-15)))]
	help := "↑↓ PgUp/PgDn · Home/End · Ctrl-R refresh · r resume · Esc back"
	if m.h < 18 {
		help = "↑↓ scroll · Ctrl-R refresh · r resume · Esc back"
	}
	notice := m.t3.err
	if notice == "" {
		notice = "Terminal continuation uses the provider's history; new CLI turns appear in the terminal."
		if reason := t.resumeBlocked(); reason != "" {
			notice = reason
		}
	}
	if m.t3.loading {
		notice = "Refreshing conversation…"
	}
	// Use fixed single-line status/help rows so long titles/errors fit small terminals.
	parts := []string{fit(heading, width), fit(status, width)}
	if m.h >= 12 {
		parts = append(parts, fit(m.listUsage(m.t3.session, width), width))
	}
	parts = append(parts, infoLines...)
	parts = append(parts, fit(notice, width))
	lines := m.t3Lines(width)
	rows := m.t3BodyRows()
	start := min(m.t3.offset, max(0, len(lines)-rows))
	parts = append(parts, lines[start:min(len(lines), start+rows)]...)
	parts = append(parts, fit(help, width))
	return strings.Join(parts, "\n")
}
