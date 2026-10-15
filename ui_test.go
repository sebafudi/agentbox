package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestFinderFiltersAndPreferencePersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(true, "")
	m.snapshot = Snapshot{Sessions: []Session{
		{Name: "active-omp", Kind: "omp", Title: "Work on editor", Context: "/project editor"},
		{Name: "active-shell", Kind: "shell", Title: "bash"},
		{Name: "saved-omp", Kind: "omp", Title: "Old editor", Saved: true},
	}}
	if got := m.listed(); len(got) != 1 || got[0].Name != "active-omp" {
		t.Fatalf("default finder filters: %+v", got)
	}
	m.w, m.h = 42, 16
	result, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = result.(model)
	for range 3 {
		result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		m = result.(model)
	}
	if m.searchFocus != searchShells {
		t.Fatalf("inline Shells setting unreachable: %d", m.searchFocus)
	}
	result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = result.(model)
	result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = result.(model)
	result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = result.(model)
	if got := m.listed(); len(got) != 3 {
		t.Fatalf("shell and resurrect toggles: %+v", got)
	}
	data, err := os.ReadFile(prefsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "shells=1\n") || !strings.Contains(string(data), "resurrect=1\n") {
		t.Fatalf("not saved: %s", data)
	}
	for range 3 {
		result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = result.(model)
	}
	if m.mode != "search" || m.searchFocus != searchResults {
		t.Fatalf("inline settings did not return to finder results: %s %d", m.mode, m.searchFocus)
	}
	reloaded := initialModel(true, "")
	if !reloaded.prefs.Shells || !reloaded.prefs.Resurrect {
		t.Fatal("preferences not restored")
	}
	m.cursor = 2
	result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = result.(model)
	if m.action.Verb != "switch" || m.action.Name != "saved-omp" || !m.action.Saved {
		t.Fatalf("saved selection cannot be resurrected: %+v", m.action)
	}
}

func TestSearchUsesUnclippedMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.mode = "search"
	m.snapshot = Snapshot{Sessions: []Session{{Name: "agent-one", Kind: "omp", Title: "A long session title", Context: "/home/user/project nested-command tab"}}}
	m.input.SetValue("nested-command agent-one")
	if got := m.listed(); len(got) != 1 {
		t.Fatalf("full metadata not searchable: %+v", got)
	}
	m.input.SetValue("missing")
	if got := m.listed(); len(got) != 0 {
		t.Fatalf("unexpected match: %+v", got)
	}
}

func TestWorkspaceFitsAndCentersAtTerminalSizes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, size := range [][2]int{{100, 36}, {80, 28}, {42, 16}, {40, 12}, {28, 9}} {
		for _, mode := range []string{"home", "search", "settings"} {
			for _, layout := range []density{densityCompact, densityTight, densitySpacey} {
				m := initialModel(false, "")
				m.w, m.h, m.mode = size[0], size[1], mode
				m.prefs.Layout = layout
				m.prefs.Shells = true
				m.snapshot.Sessions = make([]Session, 30)
				for i := range m.snapshot.Sessions {
					m.snapshot.Sessions[i] = Session{Name: "session", Kind: "omp", Title: "Long multilingual topic 日本語 🟢 and work in progress"}
				}
				view := m.View()
				if height := lipgloss.Height(view.Content); height > m.h {
					t.Fatalf("%s %s %dx%d: rendered %d rows", mode, layout, m.w, m.h, height)
				}
				for _, row := range strings.Split(view.Content, "\n") {
					if width := lipgloss.Width(row); width > m.w {
						t.Fatalf("%s %s %dx%d: rendered %d cells in %q", mode, layout, m.w, m.h, width, ansi.Strip(row))
					}
				}
				plain := ansi.Strip(view.Content)
				if m.w == 42 && mode == "home" &&
					(!strings.Contains(plain, "Claude new") || !strings.Contains(plain, "Claude last")) {
					t.Fatalf("%s narrow launch labels clipped despite available space: %q", layout, plain)
				}
				if m.w == 42 && mode == "search" {
					for _, want := range []string{"Agents", "Shells", "Saved", "Density", "History", "RESULTS", "Long multilingual"} {
						if !strings.Contains(plain, want) {
							t.Fatalf("%s narrow finder hides %q: %q", layout, want, plain)
						}
					}
					if strings.Contains(plain, "Preferences") {
						t.Fatalf("search unexpectedly offers separate preferences: %q", plain)
					}
				}
				if mode == "settings" && m.w >= 40 && (!strings.Contains(plain, "Density") || !strings.Contains(plain, "Saved")) {
					t.Fatalf("%dx%d settings replaced by fallback: %q", m.w, m.h, plain)
				}
				if mode == "settings" && m.w >= 40 && !strings.Contains(plain, map[density]string{
					densityCompact: "Compact", densityTight: "Tight", densitySpacey: "Spacious",
				}[layout]) {
					t.Fatalf("%s %dx%d density not visible: %q", layout, m.w, m.h, plain)
				}
				if m.w == 100 {
					lines := strings.Split(plain, "\n")
					top := -1
					for y, line := range lines {
						if x := strings.Index(line, "╭"); x >= 0 {
							top = y
							right := strings.LastIndex(line, "╮")
							if right >= 0 {
								right = utf8.RuneCountInString(line[:right])
							}
							if right > x && (x < m.w-right-2 || x > m.w-right) {
								t.Fatalf("%s: frame off center: %q", mode, line)
							}
							break
						}
					}
					if top < 1 {
						t.Fatalf("%s: frame not vertically centered", mode)
					}
				}
			}
		}
	}
}

func TestHomeGridNavigation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.snapshot.Sessions = []Session{{Kind: "omp", Name: "active"}}
	for _, step := range []struct {
		key  string
		want int
	}{
		{"down", 2}, {"right", 3}, {"down", 5}, {"down", 6}, {"up", 4}, {"left", 4}, {"up", 2},
	} {
		m.moveHome(step.key)
		if m.cursor != step.want {
			t.Fatalf("%s selected %d, want %d", step.key, m.cursor, step.want)
		}
	}
}

func TestHomeFilterKeysKeepSelectionAndPersist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.snapshot.Sessions = []Session{
		{Name: "agent-a", Kind: "omp", Title: "Planning"},
		{Name: "shell-a", Kind: "shell", Title: "Logs"},
		{Name: "agent-b", Kind: "claude", Title: "Editing"},
		{Name: "saved-a", Kind: "omp", Saved: true, Title: "Archive"},
	}
	m.cursor = len(quickActions) + 1
	for _, step := range []struct {
		key  rune
		want string
	}{{'b', "agent-b"}, {'v', "agent-b"}, {'g', "shell-a"}} {
		next, _ := m.Update(tea.KeyPressMsg{Code: step.key, Mod: tea.ModCtrl})
		m = next.(model)
		if got := m.focusName(); got != step.want {
			t.Fatalf("ctrl-%c changed focused session to %q; expected %q", step.key, got, step.want)
		}
	}
	if got := m.listed(); len(got) != 1 || got[0].Name != "shell-a" {
		t.Fatalf("home chips did not filter sessions: %+v", got)
	}
	reloaded := initialModel(false, "")
	if reloaded.prefs.Agents || !reloaded.prefs.Shells || !reloaded.prefs.Resurrect {
		t.Fatalf("home chip changes not persisted: %+v", reloaded.prefs)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = next.(model)
	if len(m.listed()) != 0 || m.cursor != len(quickActions)-1 || m.focusName() != "" {
		t.Fatalf("hidden focused session did not clamp to action grid: cursor=%d rows=%+v", m.cursor, m.listed())
	}
}

func TestDensityMigratesAndCyclesThroughThreeModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, old := range []struct {
		data string
		want density
	}{
		{"", densityTight},
		{"compact=0\n", densityTight},
		{"compact=1\n", densityCompact},
		{"layout=spacey\ncompact=1\n", densitySpacey},
	} {
		if err := os.MkdirAll(stateRoot(), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefsPath(), []byte(old.data), 0600); err != nil {
			t.Fatal(err)
		}
		if got := readPrefs().Layout; got != old.want {
			t.Fatalf("migrating %q: got %q, want %q", old.data, got, old.want)
		}
	}
	m := initialModel(false, "")
	m.mode, m.cursor = "settings", 3
	for _, want := range []density{densityCompact, densityTight, densitySpacey} {
		next, _ := m.updateSettings("space")
		m = next.(model)
		if m.prefs.Layout != want || readPrefs().Layout != want || m.cursor != 3 {
			t.Fatalf("cycling to %s: %+v; loaded %+v", want, m.prefs, readPrefs())
		}
	}
	data, err := os.ReadFile(prefsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "compact=") || !strings.Contains(string(data), "layout=spacey\n") {
		t.Fatalf("old preference retained after migration: %s", data)
	}
}

func TestSelectedResourcesRemainUsefulAcrossSizes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.w, m.h, m.prefs.Layout = 100, 36, densitySpacey
	now := time.Now()
	m.snapshot.At = now
	m.snapshot.Sessions = []Session{
		{Name: "agent", Kind: "omp", Title: "A deliberately identifiable long project title", State: "RUN",
			Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 36, RAM: 16 * 1024 * 1024, History: []metricSample{
				{At: now.Add(-165 * time.Second), CPUKnown: true, CPU: 10, RAM: 8 * 1024 * 1024},
				{At: now, CPUKnown: true, CPU: 36, RAM: 16 * 1024 * 1024},
			}}},
		{Name: "saved", Kind: "omp", Title: "Archived", Saved: true, Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 99, RAM: 40 * 1024 * 1024}},
	}
	m.prefs.Resurrect = true
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "▌ o  OMP · new") {
		t.Fatalf("focused spacious action has no non-color marker: %q", text)
	}
	m.cursor = len(quickActions)
	plain := ansi.Strip(m.View().Content)
	for _, text := range []string{"A deliberately identifiable long project title", "CPU now/avg/max  36.0% / 23.0% / 36.0%", "RSS now/avg/max  16.0 MiB / 12.0 MiB / 16.0 MiB", "Archived", "30m ^T"} {
		if !strings.Contains(plain, text) {
			t.Fatalf("selected resource view missing %q: %q", text, plain)
		}
	}
	if !strings.Contains(plain, "2 samples · 165s") {
		t.Fatalf("home lost selected history coverage: %q", plain)
	}
	if strings.Contains(plain, "CPU 99.0%") || strings.Contains(plain, "RSS 40.0 MiB") {
		t.Fatalf("saved session shows live telemetry: %q", plain)
	}
	m.mode, m.cursor, m.w, m.h, m.searchFocus = "search", 0, 80, 28, searchResults
	m.input.SetValue("agent")
	plain = ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "A deliberately identifiable") || !strings.Contains(plain, "RSS now/avg/max") {
		t.Fatalf("finder lost result or selected telemetry: %q", plain)
	}
	m.w, m.h = 48, 16
	plain = ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "RSS 16.0 MiB") {
		t.Fatalf("Zellij-sized finder hides selected resource usage: %q", plain)
	}
	m.snapshot.Sessions[0].Metrics.CPUKnown = false
	plain = ansi.Strip(m.View().Content)
	if strings.Contains(plain, "CPU 0.0%") || !strings.Contains(plain, "CPU —") {
		t.Fatalf("unknown CPU displayed as zero: %q", plain)
	}
	m.snapshot.At = time.Now().Add(-time.Minute)
	plain = ansi.Strip(m.View().Content)
	if strings.Contains(plain, "RSS 16.0 MiB") {
		t.Fatalf("stale cached current metrics displayed: %q", plain)
	}
}

func sgrBackground(params, background string) string {
	codes := strings.Split(params, ";")
	for i := 0; i < len(codes); i++ {
		switch codes[i] {
		case "", "0", "49":
			background = "default"
		case "48":
			if i+4 < len(codes) && codes[i+1] == "2" {
				background = strings.Join(codes[i+2:i+5], ";")
				i += 4
			}
		}
	}
	return background
}

// backgroundAt returns the active SGR background at the first occurrence of text.
// It models the terminal rather than assuming a parent style inherits across resets.
func backgroundAt(screen, text string) string {
	end := strings.Index(screen, text)
	if end < 0 {
		return "missing"
	}
	bg := "default"
	for pos := 0; pos < end; {
		if screen[pos] != '\x1b' || pos+2 >= end || screen[pos+1] != '[' {
			pos++
			continue
		}
		finish := strings.IndexByte(screen[pos+2:end], 'm')
		if finish < 0 {
			pos++
			continue
		}
		bg = sgrBackground(screen[pos+2:pos+2+finish], bg)
		pos += finish + 3
	}
	return bg
}

func TestFooterShowsRelativeSnapshotAge(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{-time.Second, "0s ago"},
		{0, "0s ago"},
		{time.Second, "1s ago"},
		{59 * time.Second, "59s ago"},
		{time.Minute, "1m ago"},
		{59 * time.Minute, "59m ago"},
		{time.Hour, "1h ago"},
		{24 * time.Hour, "1d ago"},
	} {
		if got := relativeAge(tc.age); got != tc.want {
			t.Errorf("snapshot age %s rendered %q, want %q", tc.age, got, tc.want)
		}
	}
	t.Setenv("HOME", t.TempDir())
	m := initialModel(true, "")
	m.w, m.h, m.refreshing = 42, 16, false
	m.snapshot.At = time.Now().Add(-90 * time.Second)
	if screen := ansi.Strip(m.View().Content); !strings.Contains(screen, "1m ago") ||
		strings.Contains(screen, "cached ") {
		t.Fatalf("narrow finder did not show relative snapshot age: %q", screen)
	}
}

func TestWorkspacePaintsBackgroundBehindText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.w, m.h, m.prefs.Layout = 100, 36, densitySpacey
	m.snapshot.Sessions = []Session{
		{Name: "first", Kind: "omp", Title: "Selected session"},
		{Name: "second", Kind: "claude", Title: "Unselected session"},
	}
	m.cursor = len(quickActions)
	screen := m.View().Content
	for _, check := range []struct{ text, bg string }{
		{" A G E N T B O X", "12;23;32"},
		{"OMP · new", "22;40;50"},
		{"Selected session", "32;64;74"},
		{"Unselected session", "12;23;32"},
		{"RECENT SESSIONS", "12;23;32"},
		{"↑↓ move", "22;40;50"},
	} {
		if got := backgroundAt(screen, check.text); got != check.bg {
			t.Errorf("%q background = %q, want %q", check.text, got, check.bg)
		}
	}
	m.mode, m.cursor = "search", 0
	m.w, m.h = 80, 28
	m.input.SetValue("Selected")
	screen = m.View().Content
	for _, check := range []struct{ text, bg string }{
		{"FIND A SESSION", "12;23;32"},
		{"Selected", "32;64;74"},
		{"Selected session", "12;23;32"},
	} {
		if got := backgroundAt(screen, check.text); got != check.bg {
			t.Errorf("search %q background = %q, want %q", check.text, got, check.bg)
		}
	}
	m.mode = "settings"
	screen = m.View().Content
	for _, check := range []struct{ text, bg string }{
		{"Agents", "32;64;74"},
		{"Density", "22;40;50"},
		{"PREFERENCES", "12;23;32"},
	} {
		if got := backgroundAt(screen, check.text); got != check.bg {
			t.Errorf("settings %q background = %q, want %q", check.text, got, check.bg)
		}
	}
	m.mode, m.cursor, m.w, m.h = "home", 0, 42, 16
	if got := backgroundAt(m.View().Content, "OMP new"); got != "32;64;74" {
		t.Errorf("compact selected action background = %q, want raised color", got)
	}
}

// ANSI styles may color border cells even when the card text has the right
// background. Check the actual visible cell colors on both preference rows.
func visibleBackgrounds(screen string) [][]string {
	var rows [][]string
	var row []string
	background := "default"
	for i := 0; i < len(screen); {
		if screen[i] == '\x1b' && i+1 < len(screen) && screen[i+1] == '[' {
			if end := strings.IndexByte(screen[i+2:], 'm'); end >= 0 {
				background = sgrBackground(screen[i+2:i+2+end], background)
				i += end + 3
				continue
			}
		}
		if screen[i] == '\n' {
			rows = append(rows, row)
			row = nil
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(screen[i:])
		row = append(row, background)
		i += size
	}
	return append(rows, row)
}

func TestFocusedQueryFillDoesNotPaintAdjacentRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(true, "")
	m.w, m.h = 80, 28
	m.input.SetValue("Project")
	m.snapshot.Sessions = []Session{{Name: "work", Kind: "omp", Title: "Project"}}
	screen := m.View().Content
	lines := strings.Split(ansi.Strip(screen), "\n")
	colors := visibleBackgrounds(screen)
	for y, line := range lines {
		xBytes := strings.Index(line, "▌  Project")
		if xBytes < 0 {
			continue
		}
		x := utf8.RuneCountInString(line[:xBytes])
		if got := colors[y][x+3]; got != "32;64;74" {
			t.Fatalf("focused query text background = %q, want raised", got)
		}
		for _, point := range [][2]int{{y, x - 1}, {y - 1, x + 3}, {y + 1, x + 3}, {y + 2, x + 3}} {
			if got := colors[point[0]][point[1]]; got != "12;23;32" {
				t.Errorf("query focus paints adjacent cell at row %d column %d: %q", point[0], point[1], got)
			}
		}
		return
	}
	t.Fatalf("focused query not visible: %q", strings.Join(lines, "\n"))
}

func TestQueryDetailsSumOnlyFilteredLiveSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	m := initialModel(true, "")
	m.w, m.h = 100, 36
	m.prefs.Shells, m.prefs.Resurrect = true, true
	sample := func(cpu float64, ram uint64) SessionMetrics {
		return SessionMetrics{Available: true, CPUKnown: true, CPU: cpu, RAM: ram,
			History: []metricSample{{At: now.Add(-time.Minute), CPUKnown: true, CPU: cpu, RAM: ram}}}
	}
	m.snapshot = Snapshot{At: now, Sessions: []Session{
		{Name: "alpha-agent", Kind: "omp", Metrics: sample(12, 8<<20)},
		{Name: "alpha-shell", Kind: "shell", Metrics: sample(7, 4<<20)},
		{Name: "alpha-unknown", Kind: "omp", Metrics: SessionMetrics{Available: true, RAM: 2 << 20}},
		{Name: "alpha-saved", Kind: "omp", Saved: true, Metrics: sample(100, 100<<20)},
		{Name: "beta-agent", Kind: "omp", Metrics: sample(40, 32<<20)},
	}}
	m.input.SetValue("alpha")
	screen := ansi.Strip(m.View().Content)
	for _, want := range []string{"4 matches", "Σ CPU now/avg/max  19.0% / 19.0% / 19.0% (2/3 live)", "RSS now/avg/max  14.0 MiB / 12.0 MiB / 12.0 MiB (3/3 live)", "3 live · 1 saved excluded", "ΣCPU 30m", "ΣRSS 30m"} {
		if !strings.Contains(screen, want) {
			t.Errorf("filtered total detail missing %q: %q", want, screen)
		}
	}
	if strings.Contains(screen, "Σ CPU now/avg/max  159.0%") || strings.Contains(screen, "RSS now/avg/max  146.0 MiB") {
		t.Errorf("saved or unlisted sessions contaminated total: %q", screen)
	}
	m.searchFocus = searchHistory
	screen = ansi.Strip(m.View().Content)
	if strings.Contains(screen, "CPU now/avg/max") || strings.Contains(screen, "ΣCPU 30m") {
		t.Fatalf("control focus displayed a graph for an unfocused result: %q", screen)
	}
	m.searchFocus = searchQuery
	m.w, m.h = 42, 16
	screen = ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "CPU now/avg/max 19%/19%/19% 2/3") ||
		!strings.Contains(screen, "RSS now/avg/max 14M/12M/12M 3/3") ||
		!strings.Contains(screen, "Agents") || !strings.Contains(screen, "History") ||
		!strings.Contains(screen, "RESULTS") || !strings.Contains(screen, "alpha-agent") {
		t.Fatalf("compact query hid settings, query totals, coverage or results: %q", screen)
	}
	m.searchFocus = searchResults
	screen = ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "4 matches · ΣC19.0% R14M") || strings.Contains(screen, "ΣC19% 2/3") {
		t.Fatalf("narrow results view did not retain filtered sum: %q", screen)
	}
	m.searchFocus = searchQuery
	m.w, m.h = 100, 36
	next, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = next.(model)
	screen = ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "Σ CPU now/avg/max  12.0% / 12.0% / 12.0% (1/2 live)") || m.input.Value() != "alpha" {
		t.Fatalf("filter toggle did not update query totals: %q", screen)
	}
	m.input.SetValue("beta")
	screen = ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "Σ CPU now/avg/max  40.0% / 40.0% / 40.0% (1/1 live)") || strings.Contains(screen, "Σ CPU now/avg/max  12.0%") {
		t.Fatalf("query did not update total scope: %q", screen)
	}
	m.snapshot.At = now.Add(-time.Minute)
	screen = ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "Σ CPU now/avg/max  — / — / — (0/1 live)") || strings.Contains(screen, "Σ CPU now/avg/max  0.0%") {
		t.Fatalf("stale totals treated as zero or available: %q", screen)
	}
}

func TestPreferenceHighlightStaysWithinSelectedCard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.mode, m.w, m.h, m.prefs.Layout = "settings", 100, 36, densitySpacey
	for selected := range 6 {
		m.cursor = selected
		screen := m.View().Content
		colors := visibleBackgrounds(screen)
		var topRows []int
		for y, line := range strings.Split(ansi.Strip(screen), "\n") {
			if strings.Count(line, "╭") == 2 {
				topRows = append(topRows, y)
			}
		}
		if len(topRows) != 3 {
			t.Fatalf("expected three preference rows, found %v", topRows)
		}
		for row, top := range topRows {
			plain := strings.Split(ansi.Strip(screen), "\n")[top]
			first := strings.Index(plain, "╭")
			second := strings.LastIndex(plain, "╭")
			left := utf8.RuneCountInString(plain[:first])
			right := utf8.RuneCountInString(plain[:second])
			for col, x := range []int{left, right} {
				want := "22;40;50"
				if row*2+col == selected {
					want = "32;64;74"
				}
				if got := colors[top+1][x+2]; got != want {
					t.Errorf("selected %d, card %d body background = %q, want %q", selected, row*2+col, got, want)
				}
				for _, y := range []int{top, top + 2} {
					if got := colors[y][x+2]; got != "12;23;32" {
						t.Errorf("selected %d, card %d border row %d tinted %q", selected, row*2+col, y, got)
					}
				}
			}
			for y := top; y <= top+2; y++ {
				if got := colors[y][right-1]; got != "12;23;32" {
					t.Errorf("selected %d, gap at row %d tinted %q", selected, y, got)
				}
			}
		}
		if got := backgroundAt(screen, "History span"); got != map[bool]string{false: "22;40;50", true: "32;64;74"}[selected == 4] {
			t.Errorf("selected %d, history card body background = %q", selected, got)
		}
	}
}

func mousePoint(t *testing.T, id string, screen string, text string, offset ...int) (int, int) {
	t.Helper()
	wantY := -1
	for y, line := range strings.Split(ansi.Strip(screen), "\n") {
		if strings.Contains(line, text) {
			wantY = y
			break
		}
	}
	if wantY == -1 {
		t.Fatalf("mouse target text %q not rendered", text)
	}
	if len(offset) != 0 {
		wantY += offset[0]
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if z := mouseZones.Get(id); !z.IsZero() && z.StartY == wantY {
			return z.StartX + 2, z.StartY
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mouse target %q not registered at visible row %d", id, wantY)
	return 0, 0
}

func TestMouseSelectsSessionsAndTogglesFilters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.w, m.h = 100, 36
	m.snapshot = Snapshot{At: time.Now(), Sessions: []Session{
		{Name: "first", Kind: "omp", Title: "One"},
		{Name: "second", Kind: "omp", Title: "Two"},
	}}
	screen := m.View().Content
	x, y := mousePoint(t, "session-1", screen, "Two")
	next, _ := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if m.cursor != len(quickActions)+1 || m.action.Verb != "" {
		t.Fatalf("first click must focus without attaching: cursor=%d action=%+v", m.cursor, m.action)
	}
	screen = m.View().Content
	x, y = mousePoint(t, "session-1", screen, "Two")
	next, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if m.action.Verb != "attach" || m.action.Name != "second" {
		t.Fatalf("second click did not open selected session: %+v", m.action)
	}
	m.action = Action{}
	screen = m.View().Content
	x, y = mousePoint(t, "filter-0", screen, "Agents ^G")
	next, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if m.prefs.Agents || len(m.listed()) != 0 {
		t.Fatalf("filter click did not hide agent sessions: %+v", m.prefs)
	}
}

func TestMouseSettingsAndSessionWheel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.w, m.h, m.mode = 100, 36, "settings"
	screen := m.View().Content
	x, y := mousePoint(t, "setting-1", screen, "Shells", -1)
	next, _ := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if !m.prefs.Shells || m.cursor != 1 {
		t.Fatalf("settings card click did not toggle shell preference: %+v cursor %d", m.prefs, m.cursor)
	}
	screen = m.View().Content
	x, y = mousePoint(t, "setting-4", screen, "History span", -1)
	next, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if m.prefs.HistoryMinutes != 60 || readPrefs().HistoryMinutes != 60 {
		t.Fatalf("history card click did not persist selected span: %+v", m.prefs)
	}
	m.mode, m.cursor = "home", len(quickActions)
	m.snapshot.Sessions = []Session{{Name: "one", Kind: "shell"}, {Name: "two", Kind: "shell"}}
	screen = m.View().Content
	x, y = mousePoint(t, "session-0", screen, "one")
	for until := time.Now().Add(time.Second); time.Now().Before(until) &&
		!mouseZones.Get("sessions").InBounds(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown}); {
		time.Sleep(time.Millisecond)
	}
	next, _ = m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	m = next.(model)
	if m.cursor != len(quickActions)+1 {
		t.Fatalf("wheel did not move to second session: %d", m.cursor)
	}
}

func TestShellCommandStatesAndUnavailableUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.w, m.h = 80, 32
	m.prefs.Shells = true
	m.snapshot = Snapshot{At: time.Now(), Sessions: []Session{
		{Name: "running", Kind: "shell", Title: "user@host", RunningCommand: "btop", LastCommand: "vim",
			Metrics: SessionMetrics{Available: true, RAM: 24 * 1024 * 1024}},
		{Name: "idle", Kind: "shell", Title: "user@host", LastCommand: "htop"},
	}}
	m.cursor = len(quickActions)
	plain := ansi.Strip(m.View().Content)
	for _, text := range []string{"▶ btop", "↶ last: htop", "RUNNING  btop", "CPU — · RSS 24.0 MiB", "CPU — · RSS —"} {
		if !strings.Contains(plain, text) {
			t.Fatalf("missing %q from shell view: %q", text, plain)
		}
	}
	if strings.Contains(plain, "↶ last: vim") {
		t.Fatal("running command mislabeled as last command")
	}
	m.h = 24
	plain = ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "▶ btop") || !strings.Contains(plain, "CPU — · RSS 24.0 MiB") {
		t.Fatalf("compact home lost selected command or usage: %q", plain)
	}
}

func TestSessionNavigationKeepsFrameAndListFixed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	for _, size := range [][2]int{{100, 44}, {100, 36}, {66, 32}, {80, 28}, {42, 16}} {
		for _, mode := range []string{"home", "search"} {
			m := initialModel(false, "")
			m.w, m.h, m.mode = size[0], size[1], mode
			m.prefs.Layout, m.prefs.Shells, m.prefs.Resurrect = densitySpacey, true, true
			m.snapshot = Snapshot{At: now, Sessions: []Session{
				{Name: "agent", Kind: "omp", Title: "Work", State: "RUN",
					Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 9, RAM: 8 * 1024 * 1024,
						Samples: 3, AvgRAM: 8 * 1024 * 1024, MaxRAM: 9 * 1024 * 1024,
						History: []metricSample{{At: now, CPUKnown: true, CPU: 9, RAM: 8 * 1024 * 1024}}}},
				{Name: "shell", Kind: "shell", Title: "prompt", RunningCommand: "btop",
					Metrics: SessionMetrics{Available: true, RAM: 6 * 1024 * 1024}},
				{Name: "idle", Kind: "shell", Title: "prompt", LastCommand: "htop"},
				{Name: "saved", Kind: "omp", Title: "Archived", Saved: true},
			}}
			start := 0
			if mode == "home" {
				start = len(quickActions)
			}
			if mode == "search" {
				m.searchFocus = searchResults
			}
			positions := func() [3]int {
				t.Helper()
				lines := strings.Split(ansi.Strip(m.View().Content), "\n")
				p := [3]int{-1, -1, -1}
				for y, line := range lines {
					if strings.Contains(line, "A G E N T B O X") {
						p[0] = y
					}
					if strings.Contains(line, "RECENT SESSIONS") || strings.Contains(line, "RESULTS") {
						p[1] = y
					}
					if strings.Contains(line, "│") {
						p[2] = y
					}
				}
				if p[0] < 0 || p[1] < 0 || p[2] < 0 {
					t.Fatalf("%s %dx%d lost dashboard structure: %v", mode, m.w, m.h, p)
				}
				return p
			}
			m.cursor = start
			baseline := positions()
			if mode == "home" {
				m.cursor = 0
				if got := positions(); got != baseline {
					t.Fatalf("%s %dx%d launch card shifted session list %v → %v", mode, m.w, m.h, baseline, got)
				}
			}
			for i := range m.snapshot.Sessions {
				m.cursor = start + i
				if got := positions(); got != baseline {
					t.Fatalf("%s %dx%d selection %d shifted header/list/footer %v → %v", mode, m.w, m.h, i, baseline, got)
				}
				detail := "CPU now/avg/max"
				if m.h < 20 {
					detail = "●  NOW  CPU"
				}
				if i < 2 && !strings.Contains(ansi.Strip(m.View().Content), detail) {
					t.Fatalf("%s %dx%d selection %d lost selected CPU usage", mode, m.w, m.h, i)
				}
			}
			m.cursor = start
			m.snapshot.Sessions[0].Metrics.History = nil
			if got := positions(); got != baseline {
				t.Fatalf("%s %dx%d missing chart shifted layout: %v → %v", mode, m.w, m.h, baseline, got)
			}
		}
	}
}

func TestTallerFinderGraphsKeepResultsAndFooterVisible(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	m := initialModel(true, "")
	m.prefs.Layout = densitySpacey
	m.refreshing = false
	m.snapshot.At = now
	for _, title := range []string{"First result", "Second result", "Third result"} {
		m.snapshot.Sessions = append(m.snapshot.Sessions, Session{
			Name: title, Title: title, Kind: "omp",
			Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 10, RAM: 8 << 20,
				History: []metricSample{{At: now, CPUKnown: true, CPU: 10, RAM: 8 << 20}}},
		})
	}
	for _, size := range [][2]int{{100, 36}, {100, 44}, {66, 32}} {
		m.w, m.h = size[0], size[1]
		for _, focus := range []searchFocus{searchQuery, searchResults} {
			m.searchFocus, m.cursor = focus, 0
			screen := ansi.Strip(m.View().Content)
			for _, title := range []string{"First result", "Second result", "Third result"} {
				if !strings.Contains(screen, title) {
					t.Fatalf("%dx%d focus %d: tall graphs hid %s: %q", m.w, m.h, focus, title, screen)
				}
			}
			rows := strings.Split(screen, "\n")
			cpuLabel, rssLabel := "ΣCPU ", "ΣRSS "
			if focus == searchResults {
				cpuLabel, rssLabel = "CPU ", "RSS "
			}
			cpu, rss, footer := -1, -1, -1
			for y, row := range rows {
				if strings.Contains(row, cpuLabel+m.prefs.historyLabel()) {
					cpu = y
				}
				if strings.Contains(row, rssLabel+m.prefs.historyLabel()) {
					rss = y
				}
				if strings.Contains(row, "ago") && strings.Contains(row, "↑") {
					footer = y
				}
			}
			if cpu < 0 || rss-cpu != m.chartPlotRows() || footer-rss != m.chartPlotRows() ||
				len(rows) != m.h {
				t.Fatalf("%dx%d focus %d: plots or footer clipped: CPU=%d RSS=%d footer=%d lines=%d: %q",
					m.w, m.h, focus, cpu, rss, footer, len(rows), screen)
			}
		}
	}
}

func TestSessionRowsShowCurrentResourcesInBothLists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	m := initialModel(false, "")
	m.snapshot.At = now
	m.prefs.Layout = densitySpacey
	s := Session{Name: "live", Kind: "omp", State: "RUN", Title: strings.Repeat("Project work ", 10),
		Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 12.5, RAM: 8 * 1024 * 1024,
			AvgCPU: 100, MaxCPU: 200, AvgRAM: 40 * 1024 * 1024, MaxRAM: 50 * 1024 * 1024}}
	for _, size := range [][2]int{{100, 36}, {80, 28}, {42, 16}, {28, 16}} {
		for _, mode := range []string{"home", "search"} {
			m.w, m.h, m.mode = size[0], size[1], mode
			width := m.contentWidth() - 2
			row := ansi.Strip(m.sessionRow(s, 0, width, false))
			usage := "CPU 12.5% · RSS 8.0 MiB"
			if width < 38 {
				usage = "C12.5% R8M"
			} else if width < 64 {
				usage = "C:12.5% R:8M"
			}
			if !strings.Contains(row, usage) {
				t.Errorf("%s %dx%d hid current resources: %q", mode, m.w, m.h, row)
			}
			if lipgloss.Width(row) > width || strings.Contains(row, "100") || strings.Contains(row, "50M") {
				t.Errorf("%s %dx%d row overflowed or included historical metrics: %q", mode, m.w, m.h, row)
			}
		}
	}
	s.Metrics.CPU, s.Metrics.RAM = 200.4, 3<<30
	row := ansi.Strip(m.sessionRow(s, 0, 20, false))
	if !strings.Contains(row, "C200% R3.0G") || lipgloss.Width(row) > 20 {
		t.Errorf("large current readings overflowed narrow row: %q", row)
	}
	s.Saved = true
	if row := ansi.Strip(m.sessionRow(s, 0, 76, false)); strings.Contains(row, "CPU ") || strings.Contains(row, "R:8M") {
		t.Errorf("saved row showed live metrics: %q", row)
	}
	m.snapshot.At = now.Add(-time.Minute)
	s.Saved = false
	if row := ansi.Strip(m.sessionRow(s, 0, 74, false)); strings.Contains(row, "12.5%") || !strings.Contains(row, "CPU — · RSS —") {
		t.Errorf("stale row showed a live measurement: %q", row)
	}
}

func TestHistorySpanChangesStatisticsAndSearchGraphs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	m := initialModel(false, "")
	m.w, m.h, m.mode, m.prefs.Layout = 100, 36, "search", densitySpacey
	m.snapshot = Snapshot{At: now, Sessions: []Session{{
		Name: "work", Kind: "omp", Title: "Work", Metrics: SessionMetrics{
			Available: true, CPUKnown: true, CPU: 20, RAM: 12 * 1024 * 1024,
			History: []metricSample{
				{At: now.Add(-25 * time.Minute), CPUKnown: true, CPU: 90, RAM: 32 * 1024 * 1024},
				{At: now.Add(-12 * time.Minute), CPUKnown: true, CPU: 30, RAM: 16 * 1024 * 1024},
				{At: now.Add(-5 * time.Minute), CPUKnown: true, CPU: 10, RAM: 8 * 1024 * 1024},
				{At: now.Add(-time.Minute), CPUKnown: true, CPU: 20, RAM: 12 * 1024 * 1024},
			},
		},
	}}}
	for _, span := range []struct {
		minutes int
		cpu     string
		ram     string
		samples string
	}{
		{30, "20.0% / 37.5% / 90.0%", "12.0 MiB / 17.0 MiB / 32.0 MiB", "4 samples"},
		{15, "20.0% / 20.0% / 30.0%", "12.0 MiB / 12.0 MiB / 16.0 MiB", "3 samples"},
		{5, "20.0% / 15.0% / 20.0%", "12.0 MiB / 10.0 MiB / 12.0 MiB", "2 samples"},
	} {
		m.prefs.HistoryMinutes = span.minutes
		screen := ansi.Strip(m.View().Content)
		if !strings.Contains(screen, "Σ CPU now/avg/max  "+span.cpu+" (1/1 live)") ||
			!strings.Contains(screen, "ΣCPU "+fmt.Sprintf("%3s", m.prefs.historyLabel())) ||
			!strings.Contains(screen, "ΣRSS "+fmt.Sprintf("%3s", m.prefs.historyLabel())) {
			t.Errorf("%dm span did not update aggregate graph: %q", span.minutes, screen)
		}
		m.searchFocus = searchResults
		screen = ansi.Strip(m.View().Content)
		if !strings.Contains(screen, "CPU now/avg/max  "+span.cpu) ||
			!strings.Contains(screen, "RSS now/avg/max  "+span.ram) ||
			!strings.Contains(screen, span.samples) ||
			!strings.Contains(screen, "CPU "+fmt.Sprintf("%3s", m.prefs.historyLabel())) ||
			!strings.Contains(screen, "RSS "+fmt.Sprintf("%3s", m.prefs.historyLabel())) {
			t.Errorf("%dm span did not update selected stats and graph: %q", span.minutes, screen)
		}
		m.searchFocus = searchQuery
	}
	m.snapshot.Sessions[0].Metrics.History = m.snapshot.Sessions[0].Metrics.History[:2]
	m.prefs.HistoryMinutes = 5
	if chart := m.aggregateChart(m.listed(), m.contentWidth()); chart != "" {
		t.Errorf("out-of-window samples generated graph: %q", chart)
	}
	if summary := m.historySummary(m.snapshot.Sessions[0]); summary.Samples != 0 {
		t.Errorf("out-of-window samples generated stats: %+v", summary)
	}
}

func TestHistoryPreferenceCyclesPersistsAndDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	if m.prefs.HistoryMinutes != 30 {
		t.Fatalf("default history window = %d", m.prefs.HistoryMinutes)
	}
	m.w, m.h = 42, 16
	m.openSettings()
	for _, key := range []string{"down", "down"} {
		next, _ := m.updateSettings(key)
		m = next.(model)
	}
	if m.cursor != 4 || !strings.Contains(ansi.Strip(m.View().Content), "History 30m") {
		t.Fatalf("compact preferences cannot reach history span: cursor=%d screen=%q", m.cursor, ansi.Strip(m.View().Content))
	}
	for _, span := range []struct {
		minutes int
		label   string
	}{
		{60, "1h"}, {360, "6h"}, {720, "12h"}, {1440, "24h"},
		{5, "5m"}, {15, "15m"}, {30, "30m"},
	} {
		next, _ := m.updateSettings("space")
		m = next.(model)
		if m.prefs.HistoryMinutes != span.minutes || readPrefs().HistoryMinutes != span.minutes ||
			m.prefs.historyLabel() != span.label || m.prefs.historyWindow() != time.Duration(span.minutes)*time.Minute ||
			!strings.Contains(ansi.Strip(m.View().Content), "History "+span.label) {
			t.Fatalf("cycling history span to %s: in-memory=%+v persisted=%+v", span.label, m.prefs, readPrefs())
		}
	}
	if err := os.WriteFile(prefsPath(), []byte("layout=tight\nhistory_minutes=90\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readPrefs().HistoryMinutes; got != 30 {
		t.Fatalf("unsupported saved window accepted: %d", got)
	}
}

func TestLongHistoryWindowsIncludeOlderSamples(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	m := initialModel(false, "")
	m.snapshot = Snapshot{At: now, Sessions: []Session{{
		Name: "work", Kind: "omp", Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: now.Add(-23 * time.Hour), CPUKnown: true, CPU: 100, RAM: 100 * 1024 * 1024},
			{At: now.Add(-10 * time.Hour), CPUKnown: true, CPU: 50, RAM: 50 * 1024 * 1024},
			{At: now.Add(-4 * time.Hour), CPUKnown: true, CPU: 25, RAM: 25 * 1024 * 1024},
			{At: now.Add(-45 * time.Minute), CPUKnown: true, CPU: 10, RAM: 10 * 1024 * 1024},
			{At: now.Add(-2 * time.Minute), CPUKnown: true, CPU: 5, RAM: 5 * 1024 * 1024},
		}},
	}}}
	for _, tc := range []struct {
		minutes, samples int
		maxCPU           float64
	}{
		{30, 1, 5}, {60, 2, 10}, {360, 3, 25}, {720, 4, 50}, {1440, 5, 100},
	} {
		m.prefs.HistoryMinutes = tc.minutes
		summary := m.historySummary(m.snapshot.Sessions[0])
		if summary.Samples != tc.samples || summary.MaxCPU != tc.maxCPU ||
			summary.MaxRAM != uint64(tc.maxCPU)*1024*1024 {
			t.Errorf("%s stats lost older history: %+v", m.prefs.historyLabel(), summary)
		}
	}
}

func TestSearchControlsKeyboardPreservesQueryAndResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(true, "")
	m.w, m.h = 42, 16
	m.snapshot.Sessions = []Session{{Name: "shell", Kind: "shell", RunningCommand: "btop"}}
	m.prefs.Shells = true
	key := func(msg tea.KeyPressMsg) {
		t.Helper()
		next, _ := m.Update(msg)
		m = next.(model)
	}
	for _, r := range "btop" {
		key(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	plain := ansi.Strip(m.View().Content)
	for _, text := range []string{"Agents", "Shells", "Saved", "Density", "History 30m", "btop", "RESULTS", "▶ btop"} {
		if !strings.Contains(plain, text) {
			t.Fatalf("narrow finder lost %q: %q", text, plain)
		}
	}
	if strings.Contains(plain, "Preferences") || strings.Contains(plain, "Tab settings") {
		t.Fatalf("separate search settings remain: %q", plain)
	}
	positions := func() [3]int {
		found := [3]int{-1, -1, -1}
		for y, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			for i, text := range []string{"A G E N T B O X", "RESULTS", "▶ btop"} {
				if strings.Contains(line, text) {
					found[i] = y
				}
			}
		}
		return found
	}
	baseline := positions()
	key(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.searchFocus != searchHistory || m.input.Focused() {
		t.Fatalf("Up did not focus history: %d", m.searchFocus)
	}
	if got := backgroundAt(m.View().Content, "History 30m"); got != "32;64;74" {
		t.Fatalf("history control has no focus highlight: %q", got)
	}
	key(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.prefs.HistoryMinutes != 60 || readPrefs().HistoryMinutes != 60 || m.input.Value() != "btop" {
		t.Fatalf("history change lost query or persistence: %+v %q", m.prefs, m.input.Value())
	}
	for _, want := range []searchFocus{searchDensity, searchSaved, searchShells, searchAgents} {
		key(tea.KeyPressMsg{Code: tea.KeyLeft})
		if m.searchFocus != want {
			t.Fatalf("Left focus = %d, want %d", m.searchFocus, want)
		}
	}
	key(tea.KeyPressMsg{Code: tea.KeySpace})
	if m.prefs.Agents || readPrefs().Agents || m.input.Value() != "btop" || m.cursor != 0 {
		t.Fatalf("space did not toggle agents preserving query/selection: %+v %q cursor %d", m.prefs, m.input.Value(), m.cursor)
	}
	key(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.searchFocus != searchDensity {
		t.Fatalf("Down did not move between narrow control rows: %d", m.searchFocus)
	}
	key(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.prefs.Layout != densitySpacey || readPrefs().Layout != densitySpacey {
		t.Fatalf("density control did not persist: %+v", m.prefs)
	}
	key(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.searchFocus != searchShells {
		t.Fatalf("Up did not move to previous narrow control row: %d", m.searchFocus)
	}
	key(tea.KeyPressMsg{Code: tea.KeyRight})
	key(tea.KeyPressMsg{Code: tea.KeySpace})
	if !m.prefs.Resurrect || !readPrefs().Resurrect || m.searchFocus != searchSaved {
		t.Fatalf("saved setting not toggled from keyboard: %+v focus %d", m.prefs, m.searchFocus)
	}
	key(tea.KeyPressMsg{Code: tea.KeyRight})
	key(tea.KeyPressMsg{Code: tea.KeyRight})
	key(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.searchFocus != searchQuery || !m.input.Focused() {
		t.Fatalf("Down did not return to query: %d", m.searchFocus)
	}
	key(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.searchFocus != searchResults || m.cursor != 0 || m.input.Value() != "btop" {
		t.Fatalf("Down did not focus preserved result: focus=%d cursor=%d query=%q", m.searchFocus, m.cursor, m.input.Value())
	}
	if got := backgroundAt(m.View().Content, "▶ btop"); got != "32;64;74" {
		t.Fatalf("focused result has no highlight: %q", got)
	}
	key(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.searchFocus != searchQuery || m.input.Value() != "btop" {
		t.Fatalf("Up from first result lost query: focus=%d query=%q", m.searchFocus, m.input.Value())
	}
	key(tea.KeyPressMsg{Code: '\t'})
	if m.searchFocus != searchHistory || m.mode != "search" {
		t.Fatalf("Tab opened settings instead of inline controls: mode=%s focus=%d", m.mode, m.searchFocus)
	}
	if got := positions(); got != baseline {
		t.Fatalf("inline control navigation shifted narrow finder: %v → %v", baseline, got)
	}
}

func TestHistoryShortcutOnHomeAndSearch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.w, m.h = 100, 36
	now := time.Now()
	m.snapshot = Snapshot{At: now, Sessions: []Session{{Name: "btop", Kind: "omp", Metrics: SessionMetrics{
		Available: true, CPUKnown: true, CPU: 10, RAM: 10 << 20,
		History: []metricSample{
			{At: now.Add(-45 * time.Minute), CPUKnown: true, CPU: 100, RAM: 50 << 20},
			{At: now.Add(-2 * time.Minute), CPUKnown: true, CPU: 10, RAM: 10 << 20},
		},
	}}}}
	m.cursor = len(quickActions)
	if screen := ansi.Strip(m.View().Content); !strings.Contains(screen, "CPU now/avg/max  10.0% / 10.0% / 10.0%") {
		t.Fatalf("home 30m history did not exclude older samples: %q", screen)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	m = next.(model)
	if m.prefs.HistoryMinutes != 60 || readPrefs().HistoryMinutes != 60 {
		t.Fatalf("home Ctrl+T did not persist history span: %+v", m.prefs)
	}
	if screen := ansi.Strip(m.View().Content); !strings.Contains(screen, "1h ^T history") ||
		!strings.Contains(screen, "CPU now/avg/max  10.0% / 55.0% / 100.0%") ||
		!strings.Contains(screen, "RSS now/avg/max  10.0 MiB / 30.0 MiB / 50.0 MiB") {
		t.Fatalf("home span did not update selected history stats: %q", screen)
	}
	m.w, m.h = 42, 16
	if screen := ansi.Strip(m.View().Content); !strings.Contains(screen, "1h ^T") {
		t.Fatalf("compact home did not advertise selected history span: %q", screen)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: '/'})
	m = next.(model)
	for _, r := range "btop" {
		next, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = next.(model)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	m = next.(model)
	if m.prefs.HistoryMinutes != 360 || readPrefs().HistoryMinutes != 360 || m.input.Value() != "btop" || m.searchFocus != searchQuery {
		t.Fatalf("search Ctrl+T changed input or failed save: prefs=%+v query=%q", m.prefs, m.input.Value())
	}
	if got := initialModel(true, "").prefs.historyLabel(); got != "6h" {
		t.Fatalf("search history shortcut not restored: %s", got)
	}
}

func TestSearchControlsMouseToggleAndQueryFocus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(true, "")
	m.w, m.h = 100, 36
	m.snapshot.Sessions = []Session{{Name: "work", Kind: "omp", Title: "Project"}, {Name: "shell", Kind: "shell", Title: "Project shell"}}
	m.prefs.Shells = true
	m.input.SetValue("Project")
	m.cursor = 1
	for _, tc := range []struct {
		focus searchFocus
		text  string
	}{
		{searchHistory, "History 30m"}, {searchDensity, "Density Tight"},
		{searchSaved, "Saved ○"}, {searchShells, "Shells ●"}, {searchAgents, "Agents ●"},
	} {
		screen := m.View().Content
		x, y := mousePoint(t, fmt.Sprintf("search-control-%d", tc.focus), screen, tc.text)
		next, _ := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		m = next.(model)
		if m.mode != "search" || m.searchFocus != tc.focus || m.input.Value() != "Project" || m.input.Focused() {
			t.Fatalf("control %d lost search context: mode=%s focus=%d query=%q", tc.focus, m.mode, m.searchFocus, m.input.Value())
		}
		if tc.focus >= searchSaved && m.cursor != 1 {
			t.Fatalf("unrelated control changed selected result: %d", m.cursor)
		}
	}
	if m.prefs.HistoryMinutes != 60 || m.prefs.Layout != densitySpacey || !m.prefs.Resurrect || m.prefs.Shells || m.prefs.Agents {
		t.Fatalf("mouse control clicks did not apply: %+v", m.prefs)
	}
	if saved := readPrefs(); saved != m.prefs {
		t.Fatalf("mouse changes not persisted: %+v versus %+v", saved, m.prefs)
	}
	screen := m.View().Content
	x, y := mousePoint(t, "search-query", screen, "Project", -1)
	next, _ := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if m.searchFocus != searchQuery || !m.input.Focused() || m.input.Value() != "Project" {
		t.Fatalf("mouse query did not restore typing focus: focus=%d query=%q", m.searchFocus, m.input.Value())
	}
}

func TestHomePreferencesReturnAndSearchTabStaysInline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.snapshot.Sessions = []Session{
		{Name: "first", Kind: "omp", Title: "Project alpha"},
		{Name: "second", Kind: "omp", Title: "Project beta"},
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: '/'})
	m = next.(model)
	m.input.SetValue("Project")
	m.cursor = 1
	next, _ = m.Update(tea.KeyPressMsg{Code: '\t'})
	m = next.(model)
	if m.mode != "search" || m.searchFocus != searchHistory || m.input.Value() != "Project" || m.cursor != 1 {
		t.Fatalf("search Tab left inline settings: mode=%s focus=%d query=%q cursor=%d", m.mode, m.searchFocus, m.input.Value(), m.cursor)
	}
	m.mode, m.cursor = "home", len(quickActions)+1
	next, _ = m.Update(tea.KeyPressMsg{Code: 'p'})
	m = next.(model)
	if m.mode != "settings" {
		t.Fatalf("home p did not open preferences: %s", m.mode)
	}
	next, _ = m.updateSettings("esc")
	m = next.(model)
	if m.mode != "home" || m.cursor != len(quickActions)+1 {
		t.Fatalf("return from home preferences lost context: mode=%s cursor=%d", m.mode, m.cursor)
	}
}

func TestSearchRowsAlignLiveReadingsAtRightEdge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := initialModel(true, "")
	m.snapshot.At = time.Now()
	for _, size := range [][2]int{{100, 36}, {80, 28}, {42, 16}, {28, 16}} {
		m.w, m.h = size[0], size[1]
		for _, layout := range []density{densityTight, densitySpacey} {
			m.prefs.Layout = layout
			width := m.contentWidth() - 2
			var rightEdge int
			for i, tc := range []struct {
				title string
				cpu   bool
			}{
				{"Short", true},
				{strings.Repeat("A very long project title ", 8), true},
				{"No CPU", false},
			} {
				s := Session{Name: "work", Kind: "omp", State: "RUN", Title: tc.title,
					Metrics: SessionMetrics{Available: true, CPUKnown: tc.cpu, CPU: 12.5, RAM: 8 << 20}}
				row := ansi.Strip(m.sessionRow(s, i, width, i == 1))
				reading := m.currentUsage(s)
				if !m.spaciousView() {
					reading = m.listUsage(s, width)
				}
				x := strings.LastIndex(row, reading)
				if x < 0 {
					t.Fatalf("%s %dx%d row lost reading %q: %q", layout, m.w, m.h, reading, row)
				}
				end := lipgloss.Width(row[:x]) + lipgloss.Width(reading)
				if end != width || lipgloss.Width(row) > width {
					t.Fatalf("%s %dx%d title %q: reading ends at %d, want %d: %q", layout, m.w, m.h, tc.title, end, width, row)
				}
				if i > 0 && end != rightEdge {
					t.Fatalf("search readings do not share a column: %d != %d", end, rightEdge)
				}
				rightEdge = end
				if i == 1 && !strings.Contains(row, "…") {
					t.Fatalf("%s %dx%d long title was not truncated before reading: %q", layout, m.w, m.h, row)
				}
			}
		}
	}
}
