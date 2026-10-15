package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func fakeHistoryFD(t *testing.T, root string, pid, fd int, target string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid), "fd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, fmt.Sprint(fd))); err != nil {
		t.Fatal(err)
	}
}

func TestT3MetricsDedicatedBackendChildrenAndCPUHistory(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	// T3 was launched from a shell pane, so its children inherit that marker.
	env := "ZELLIJ_SESSION_NAME=shell\x00ZELLIJ_PANE_ID=1\x00"
	fakeProc(t, root, 10, 10, 10000, 1000, env, "t3\x00serve\x00")
	fakeProc(t, root, 11, 11, 100, 10, env, "codex\x00app-server\x00", 10)
	fakeProc(t, root, 12, 12, 200, 20, env, "helper\x00", 11)
	fakeProc(t, root, 13, 13, 50, 30, "", "tool\x00", 12)
	fakeProc(t, root, 14, 14, 500, 40, env, "other-chat\x00", 10)
	fakeHistoryFD(t, root, 11, 1, "/codex/sessions/rollout-2026-10-06-"+testCodexID+".jsonl")
	at := time.Unix(1_700_000_000, 0)
	sessions := []Session{
		{Name: "chat", Kind: "codex", T3: &t3Thread{Provider: "codex", Conversation: testCodexID}},
		{Name: "shell", Kind: "shell"},
	}
	populateSessionMetrics(root, store, sessions, func() time.Time { return at })
	if m := sessions[0].Metrics; !m.Available || m.CPUKnown || m.RAM != 60*uint64(os.Getpagesize()) || m.Samples != 1 {
		t.Fatalf("first T3 measurement: %+v", m)
	}
	if m := sessions[1].Metrics; m.RAM != 1040*uint64(os.Getpagesize()) {
		t.Fatalf("T3 workload was counted twice or shared server assigned to chat: %+v", m)
	}
	fakeProc(t, root, 11, 11, 1100, 12, env, "codex\x00app-server\x00", 10)
	populateSessionMetrics(root, store, sessions, func() time.Time { return at.Add(10 * time.Second) })
	m := sessions[0].Metrics
	if !m.CPUKnown || math.Abs(m.CPU-100) > .001 || m.RAM != 62*uint64(os.Getpagesize()) || m.Samples != 2 || m.MaxCPU != 100 {
		t.Fatalf("CPU delta/history: %+v", m)
	}
	// A restarted provider has a different process identity; discard its baseline.
	fakeProc(t, root, 11, 1000, 90000, 12, env, "codex\x00app-server\x00", 10)
	populateSessionMetrics(root, store, sessions, func() time.Time { return at.Add(20 * time.Second) })
	if m := sessions[0].Metrics; m.CPUKnown || m.Samples != 1 {
		t.Fatalf("provider restart retained old CPU/history: %+v", m)
	}
}

func TestT3MetricsRejectSharedAmbiguousAndUnrelatedProcesses(t *testing.T) {
	root := t.TempDir()
	mapping := map[string]t3ProcessSession{testCodexID: {Name: "chat", Provider: "codex"}, testClaudeID: {Name: "claude", Provider: "claudeAgent"}}
	fakeProc(t, root, 11, 11, 100, 10, "", "codex\x00app-server\x00")
	fakeHistoryFD(t, root, 11, 1, "/sessions/rollout-date-"+testCodexID+".jsonl")
	if got := t3ProcessSessionName(filepath.Join(root, "11"), mapping); got != "chat" {
		t.Fatalf("dedicated provider not matched: %q", got)
	}
	fakeHistoryFD(t, root, 11, 2, "/sessions/rollout-date-00000000-0000-4000-8000-000000000000.jsonl")
	if got := t3ProcessSessionName(filepath.Join(root, "11"), mapping); got != "" {
		t.Fatalf("shared provider attributed to one chat: %q", got)
	}
	fakeProc(t, root, 12, 12, 100, 10, "", "cat\x00history\x00")
	fakeHistoryFD(t, root, 12, 1, "/sessions/rollout-date-"+testCodexID+".jsonl")
	if got := t3ProcessSessionName(filepath.Join(root, "12"), mapping); got != "" {
		t.Fatalf("unrelated history reader attributed to chat: %q", got)
	}
	fakeProc(t, root, 13, 13, 100, 10, "", "claude\x00--output-format\x00stream-json\x00")
	fakeHistoryFD(t, root, 13, 1, "/claude/projects/project/"+testClaudeID+".jsonl")
	if got := t3ProcessSessionName(filepath.Join(root, "13"), mapping); got != "claude" {
		t.Fatalf("Claude SDK provider not matched: %q", got)
	}
	fakeProc(t, root, 14, 14, 100, 10, "", "claude\x00--output-format\x00stream-json\x00--session-id\x00"+testClaudeID+"\x00")
	if got := t3ProcessSessionName(filepath.Join(root, "14"), mapping); got != "claude" {
		t.Fatalf("idle Claude SDK session argument not matched: %q", got)
	}
}

func TestT3ResourceReadingsRenderInListDetailAndInspector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := Session{Name: "chat", Kind: "codex", Title: "Resource test", State: "RUN",
		T3:      &t3Thread{Project: "Project", Cwd: "/project", Provider: "codex", Status: "running"},
		Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 12.5, RAM: 24 << 20}}
	for _, size := range [][2]int{{100, 36}, {42, 16}, {28, 9}} {
		m := initialModel(false, "")
		m.w, m.h = size[0], size[1]
		m.snapshot = Snapshot{Sessions: []Session{s}, At: time.Now()}
		m.cursor = len(quickActions)
		for _, mode := range []string{"home", "search", "t3"} {
			m.mode = mode
			if mode == "search" {
				m.cursor, m.searchFocus = 0, searchResults
			}
			m.t3 = t3UI{session: s, messages: []t3Message{{Role: "assistant", Text: "Live message"}}}
			content := m.View().Content
			if lipgloss.Width(content) > m.w || lipgloss.Height(content) > m.h {
				t.Fatalf("%s %dx%d usage overflows", mode, m.w, m.h)
			}
			if m.h >= 16 && !strings.Contains(ansi.Strip(content), "12.5%") {
				t.Fatalf("%s resource readings missing: %s", mode, ansi.Strip(content))
			}
		}
		if totals := m.aggregateResources([]Session{s}); totals.Live != 1 || totals.CPU != 12.5 || totals.RAM != 24<<20 {
			t.Fatalf("measured chat excluded from totals: %+v", totals)
		}
	}
}
