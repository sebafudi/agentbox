package main

import (
	"fmt"
	"image/color"
	"math"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	ink     = lipgloss.Color("#0C1720")
	surface = lipgloss.Color("#162832")
	raised  = lipgloss.Color("#20404A")
	outline = lipgloss.Color("#42606B")
	mint    = lipgloss.Color("#ACF1C6")
	peach   = lipgloss.Color("#F9C69D")
	pale    = lipgloss.Color("#F1F7F1")
	muted   = lipgloss.Color("#A1B9B8")
)

var (
	inkBG     = ansi.NewStyle().BackgroundColor(ink).String()
	surfaceBG = ansi.NewStyle().BackgroundColor(surface).String()
	raisedBG  = ansi.NewStyle().BackgroundColor(raised).String()
)

// Lip Gloss nested styles reset the background too. Reapply it after the
// nested text resets, without painting neighboring border or separator cells.
func backgroundText(text, background string) string {
	return strings.ReplaceAll(text, ansi.ResetStyle, ansi.ResetStyle+background)
}

func restoreBackground(text, background string) string {
	return backgroundText(text, background) + ansi.ResetStyle
}

func panelBackground(active bool) string {
	if active {
		return raisedBG
	}
	return surfaceBG
}

func rowBackground(active bool) string {
	if active {
		return raisedBG
	}
	return inkBG
}

func fit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(text, width, "…")
}

func label(text string, tone color.Color) string {
	return lipgloss.NewStyle().Bold(true).Foreground(tone).Render(text)
}

func rightAligned(left, right string, width int) string {
	available := max(0, width-lipgloss.Width(left)-1)
	right = ansi.Truncate(right, available, "")
	return left + strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(right))) + right
}

func (m model) compactView() bool  { return m.prefs.Layout == densityCompact || m.h < 20 || m.w < 60 }
func (m model) spaciousView() bool { return m.prefs.Layout == densitySpacey && m.w >= 70 && m.h >= 24 }
func (m model) contentWidth() int {
	cap := 76
	if m.spaciousView() {
		cap = 92
	}
	return max(20, min(cap, m.w-6))
}

func (m model) header(width int) string {
	mark := lipgloss.NewStyle().Foreground(ink).Background(mint).Bold(true).Padding(0, 1).Render("◇")
	brand := label(" A G E N T B O X", mint)
	context := "YOUR WORKSPACE"
	if m.finder {
		context = "SWITCH SESSION"
	}
	if m.mode == "search" && !m.finder {
		context = "SESSION FINDER"
	}
	if m.mode == "settings" {
		context = "PREFERENCES"
	}
	top := rightAligned(mark+brand, lipgloss.NewStyle().Foreground(muted).Render(context), width)
	if m.compactView() {
		return top
	}
	sub := lipgloss.NewStyle().Foreground(muted).Render("  Pick up where you left off, or start somewhere new.")
	if m.mode == "search" {
		sub = lipgloss.NewStyle().Foreground(muted).Render("  Find a session by topic, path, command, or tab.")
	}
	if m.mode == "settings" {
		sub = lipgloss.NewStyle().Foreground(muted).Render("  Make the workspace yours. Changes are saved instantly.")
	}
	return top + "\n" + fit(sub, width)
}

func (m model) section(title, detail string, width int) string {
	name := lipgloss.NewStyle().Bold(true).Foreground(peach).Render(title)
	caption := lipgloss.NewStyle().Foreground(muted).Render(detail)
	return rightAligned(name, caption, width)
}

func (m model) actionCard(index, width int, small bool) string {
	a := quickActions[index]
	active := m.cursor == index
	key := lipgloss.NewStyle().Bold(true).Foreground(ink).Background(mint).Padding(0, 1).Render(a.key)
	fg := pale
	bg := surface
	border := outline
	if active {
		fg = mint
		bg = raised
		border = mint
	}
	textLabel := a.label
	labelWidth := width - 8
	if small {
		textLabel = a.short
		labelWidth = width - 6
	}
	text := key + " " + lipgloss.NewStyle().Foreground(fg).Bold(active).Render(fit(textLabel, labelWidth))
	if small {
		return restoreBackground(lipgloss.NewStyle().Width(width).Background(bg).Foreground(fg).Render(" "+text), panelBackground(active))
	}
	if m.spaciousView() {
		marker := " "
		if active {
			marker = "▌"
		}
		return restoreBackground(lipgloss.NewStyle().Width(width-4).Padding(0, 2).Background(bg).Render(marker+text), panelBackground(active))
	}
	style := lipgloss.NewStyle().Width(width).Padding(0, 1).Border(lipgloss.RoundedBorder()).BorderForeground(border).Background(bg)
	return restoreBackground(style.Render(text), panelBackground(active))
}

func (m model) actionGrid(width int) string {
	small := m.compactView()
	gap := " "
	if m.spaciousView() && width >= 72 {
		gap = "   "
	}
	col := (width - len(gap)) / 2
	var rows []string
	for i := 0; i < len(quickActions); i += 2 {
		left := mouseZones.Mark(fmt.Sprintf("action-%d", i), m.actionCard(i, col, small))
		right := mouseZones.Mark(fmt.Sprintf("action-%d", i+1), m.actionCard(i+1, width-col-len(gap), small))
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, left, gap, right))
	}
	separator := "\n"
	if m.spaciousView() && m.h >= 32 {
		separator = "\n\n"
	}
	return strings.Join(rows, separator)
}

func (m model) filterBar(width int) string {
	if width < 34 {
		chip := func(key string, enabled bool) string {
			mark := "○"
			tone := muted
			if enabled {
				mark = "●"
				tone = mint
			}
			return lipgloss.NewStyle().Foreground(tone).Render("^" + key + mark)
		}
		return mouseZones.Mark("filter-0", chip("G", m.prefs.Agents)) + " " + mouseZones.Mark("filter-1", chip("B", m.prefs.Shells)) + " " + mouseZones.Mark("filter-2", chip("V", m.prefs.Resurrect)) + " " + mouseZones.Mark("filter-3", chip("Y", m.prefs.T3))
	}
	filter := func(on bool, title, hotkey string) string {
		check := "○"
		fg := muted
		bg := surface
		if on {
			check = "●"
			fg = mint
			bg = raised
		}
		return restoreBackground(lipgloss.NewStyle().Foreground(fg).Background(bg).Padding(0, 1).Render(check+" "+title+" "+hotkey), panelBackground(on))
	}
	names := []string{"Agents", "Shells", "Saved"}
	if width < 60 {
		names = []string{"A", "S", "R"}
	}
	bar := mouseZones.Mark("filter-0", filter(m.prefs.Agents, names[0], "^G")) + " " + mouseZones.Mark("filter-1", filter(m.prefs.Shells, names[1], "^B")) + " " + mouseZones.Mark("filter-2", filter(m.prefs.Resurrect, names[2], "^V")) + " " + mouseZones.Mark("filter-3", filter(m.prefs.T3, "T3", "^Y"))
	return bar
}

func rss(bytes uint64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		if value < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return ""
}

func (m model) currentUsage(s Session) string {
	if s.Saved {
		return ""
	}
	if !s.Metrics.Available || m.snapshot.At.IsZero() || time.Since(m.snapshot.At) > 30*time.Second {
		return "CPU — · RSS —"
	}
	if !s.Metrics.CPUKnown {
		return "CPU — · RSS " + rss(s.Metrics.RAM)
	}
	return fmt.Sprintf("CPU %.1f%% · RSS %s", s.Metrics.CPU, rss(s.Metrics.RAM))
}
func briefRSS(bytes uint64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

func (m model) listUsage(s Session, width int) string {
	if s.Saved {
		return ""
	}
	if width >= 64 {
		return m.currentUsage(s)
	}
	if !s.Metrics.Available || m.snapshot.At.IsZero() || time.Since(m.snapshot.At) > 30*time.Second {
		if width < 38 {
			return "C— R—"
		}
		return "C:— R:—"
	}
	if width < 38 {
		cpu := "—"
		if s.Metrics.CPUKnown {
			cpu = fmt.Sprintf("%.1f%%", s.Metrics.CPU)
		}
		ram := briefRSS(s.Metrics.RAM)
		usage := "C" + cpu + " R" + ram
		if width < 26 && lipgloss.Width(usage) > width-8 && s.Metrics.CPUKnown {
			cpu = fmt.Sprintf("%.0f%%", math.Round(s.Metrics.CPU))
			usage = "C" + cpu + " R" + ram
		}
		if width < 26 && lipgloss.Width(usage) > width-8 && strings.HasSuffix(ram, ".0G") {
			usage = "C" + cpu + " R" + strings.TrimSuffix(ram, ".0G") + "G"
		}
		return usage
	}
	cpu := "—"
	if s.Metrics.CPUKnown {
		cpu = fmt.Sprintf("%.1f%%", s.Metrics.CPU)
	}
	return "C:" + cpu + " R:" + briefRSS(s.Metrics.RAM)
}

func (m model) historySummary(s Session) SessionMetrics {
	var summary SessionMetrics
	if s.Saved || !s.Metrics.Available || m.snapshot.At.IsZero() ||
		time.Since(m.snapshot.At) > 30*time.Second {
		return summary
	}
	start := m.snapshot.At.Add(-m.prefs.historyWindow())
	samples := s.Metrics.History
	// The collector appends samples in time order; skip older history without
	// scanning the entire day for a 5-minute selection on every render.
	first := sort.Search(len(samples), func(i int) bool {
		return !samples[i].At.Before(start)
	})
	last := first
	for last < len(samples) && !samples[last].At.After(m.snapshot.At) {
		last++
	}
	metricSummary(&summary, samples[first:last])
	return summary
}

func sessionTitle(s Session) string {
	if s.Kind == "shell" && !s.Saved {
		if s.RunningCommand != "" {
			return "▶ " + clean(s.RunningCommand)
		}
		if s.LastCommand != "" {
			return "↶ last: " + clean(s.LastCommand)
		}
	}
	if s.Title != "" {
		if s.T3 != nil && s.T3.Project != "" {
			return clean(s.Title) + " · " + clean(s.T3.Project)
		}
		return clean(s.Title)
	}
	return s.Name
}

func (m model) selectedSession(rows []Session) (Session, bool) {
	index := m.cursor
	if m.mode == "home" {
		index -= len(quickActions)
	}
	if index < 0 || index >= len(rows) {
		return Session{}, false
	}
	return rows[index], true
}

func (m model) sessionDetail(width int, rows []Session, totals resourceTotals) string {
	if width < 22 || m.h < 12 {
		return ""
	}
	s, selected := m.selectedSession(rows)
	if m.mode == "search" && m.searchFocus != searchQuery && m.searchFocus != searchResults {
		selected = false
	}
	if m.mode != "search" && !selected && len(rows) == 0 {
		return ""
	}
	// Reserve the same detail rows for every selection. Otherwise selecting a
	// shell command, a saved session, or a session with chart history recenters
	// the frame and moves the session list under the cursor.
	lines := []string{" "}
	var history SessionMetrics
	if m.mode == "search" && m.searchFocus == searchQuery {
		cpu, ram := "—", "—"
		if totals.CPUKnown > 0 {
			cpu = fmt.Sprintf("%.1f%%", totals.CPU)
		}
		if totals.RSSKnown > 0 {
			ram = rss(totals.RAM)
		}
		switch {
		case m.h < 16:
			if totals.RSSKnown > 0 {
				ram = briefRSS(totals.RAM)
			}
			lines[0] = fmt.Sprintf("ΣCPU %s %d/%d · RSS %s %d/%d", cpu, totals.CPUKnown, totals.Live, ram, totals.RSSKnown, totals.Live)
		case m.h < 20 || width < 60:
			history = m.aggregateHistory(rows)
			avgCPU, maxCPU, avgRAM, maxRAM := "—", "—", "—", "—"
			if totals.CPUKnown > 0 {
				cpu = fmt.Sprintf("%.0f%%", totals.CPU)
			}
			if totals.RSSKnown > 0 {
				ram = briefRSS(totals.RAM)
			}
			if history.CPUSamples > 0 {
				avgCPU = fmt.Sprintf("%.0f%%", history.AvgCPU)
				maxCPU = fmt.Sprintf("%.0f%%", history.MaxCPU)
			}
			if history.Samples > 0 {
				avgRAM = briefRSS(history.AvgRAM)
				maxRAM = briefRSS(history.MaxRAM)
			}
			lines[0] = fmt.Sprintf("CPU now/avg/max %s/%s/%s %d/%d", cpu, avgCPU, maxCPU, totals.CPUKnown, totals.Live)
			lines = append(lines, fmt.Sprintf("RSS now/avg/max %s/%s/%s %d/%d", ram, avgRAM, maxRAM, totals.RSSKnown, totals.Live))
			if m.h >= 20 {
				lines = append(lines, fmt.Sprintf("  %d live · %d saved excluded", totals.Live, totals.Saved))
			}
		default:
			history = m.aggregateHistory(rows)
			avgCPU, maxCPU, avgRAM, maxRAM := "—", "—", "—", "—"
			if history.CPUSamples > 0 {
				avgCPU = fmt.Sprintf("%.1f%%", history.AvgCPU)
				maxCPU = fmt.Sprintf("%.1f%%", history.MaxCPU)
			}
			if history.Samples > 0 {
				avgRAM = rss(history.AvgRAM)
				maxRAM = rss(history.MaxRAM)
			}
			lines[0] = fmt.Sprintf("  Σ CPU now/avg/max  %s / %s / %s (%d/%d live)", cpu, avgCPU, maxCPU, totals.CPUKnown, totals.Live)
			lines = append(lines, fmt.Sprintf("    RSS now/avg/max  %s / %s / %s (%d/%d live)", ram, avgRAM, maxRAM, totals.RSSKnown, totals.Live))
			lines = append(lines, fmt.Sprintf("  %d live · %d saved excluded · unknown readings shown as —", totals.Live, totals.Saved))
		}
	} else if selected && !s.Saved {
		if m.h < 20 {
			lines[0] = "  ●  NOW  " + m.currentUsage(s)
		} else {
			cpu, ram := "—", "—"
			if s.Metrics.Available && !m.snapshot.At.IsZero() && time.Since(m.snapshot.At) <= 30*time.Second {
				ram = rss(s.Metrics.RAM)
				if s.Metrics.CPUKnown {
					cpu = fmt.Sprintf("%.1f%%", s.Metrics.CPU)
				}
			}
			history = m.historySummary(s)
			avgCPU, maxCPU, avgRAM, maxRAM := "—", "—", "—", "—"
			if history.CPUSamples > 0 {
				avgCPU = fmt.Sprintf("%.1f%%", history.AvgCPU)
				maxCPU = fmt.Sprintf("%.1f%%", history.MaxCPU)
			}
			if history.Samples > 0 {
				avgRAM = rss(history.AvgRAM)
				maxRAM = rss(history.MaxRAM)
			}
			if width < 60 {
				if ram != "—" {
					ram = briefRSS(s.Metrics.RAM)
				}
				if history.Samples > 0 {
					avgRAM = briefRSS(history.AvgRAM)
					maxRAM = briefRSS(history.MaxRAM)
				}
				lines[0] = fmt.Sprintf("CPU now/avg/max %s/%s/%s", cpu, avgCPU, maxCPU)
				lines = append(lines, fmt.Sprintf("RSS now/avg/max %s/%s/%s", ram, avgRAM, maxRAM))
			} else {
				lines[0] = fmt.Sprintf("  ●  CPU now/avg/max  %s / %s / %s", cpu, avgCPU, maxCPU)
				lines = append(lines, fmt.Sprintf("     RSS now/avg/max  %s / %s / %s", ram, avgRAM, maxRAM))
			}
		}
		if m.mode == "search" && m.h >= 16 {
			lines = append(lines, " ")
		}
	} else if m.h >= 20 {
		lines = append(lines, " ")
		if m.mode == "search" {
			lines = append(lines, " ")
		}
	} else if m.mode == "search" && m.h >= 16 {
		lines = append(lines, " ")
	}
	if m.h >= 28 {
		command := " "
		if selected && s.T3 != nil && (m.mode != "search" || m.searchFocus == searchResults) {
			command = "  T3 · " + s.T3.Project + " · " + clean(s.T3.Cwd) + " · Enter inspect"
		}
		if selected && (m.mode != "search" || m.searchFocus == searchResults) && s.Kind == "shell" && !s.Saved {
			switch {
			case s.RunningCommand != "":
				command = "  ▶ RUNNING  " + clean(s.RunningCommand)
			case s.LastCommand != "":
				command = "  ↶ LAST  " + clean(s.LastCommand) + " · not running"
			}
		}
		lines = append(lines, command)
	}
	if m.h >= 32 && width >= 78 {
		coverage := " "
		if selected && history.Samples > 0 {
			coverage = fmt.Sprintf("  ◦  %s · %d samples · %ds", m.prefs.historyLabel(), history.Samples, history.SpanSeconds)
		}
		lines = append(lines, coverage)
	}
	if m.mode == "search" && width >= 60 && m.h >= 32 {
		chart := ""
		if m.searchFocus == searchQuery {
			chart = m.aggregateChart(rows, width)
		} else if selected {
			chart = m.resourceChart(s, width)
		}
		if chart == "" {
			for range 2 * m.chartPlotRows() {
				lines = append(lines, " ")
			}
		} else {
			lines = append(lines, strings.Split(chart, "\n")...)
		}
	}
	for i := range lines {
		lines[i] = fit(lines[i], width)
	}
	return lipgloss.NewStyle().Foreground(muted).Render(strings.Join(lines, "\n"))
}

func (m model) sessionRow(s Session, index, width int, active bool) string {
	kind := strings.ToUpper(s.Kind)
	if s.T3 != nil {
		kind = "T3 " + kind
	}
	if s.Saved {
		kind = "SAVED " + kind
	}
	badge := s.State
	if s.Notice == "NEW" {
		badge = "NEW"
	}
	if s.Kind == "shell" && !s.Saved {
		badge = "IDLE"
		if s.RunningCommand != "" {
			badge = "LIVE"
		}
	}
	badgeColor := muted
	switch badge {
	case "RUN", "READY", "LIVE":
		badgeColor = mint
	case "ASK", "NEW":
		badgeColor = peach
	}
	if m.spaciousView() && m.mode == "home" {
		bg := ink
		marker := " "
		if active {
			bg = raised
			marker = "▌"
		}
		title := sessionTitle(s)
		first := marker + fmt.Sprintf(" %2d  ", index+1) + lipgloss.NewStyle().Foreground(pale).Bold(active).Render(fit(title, width-8))
		meta := lipgloss.NewStyle().Foreground(peach).Render(kind) + "  " +
			lipgloss.NewStyle().Foreground(badgeColor).Bold(true).Render(badge)
		if usage := m.currentUsage(s); usage != "" {
			meta = rightAligned(meta, lipgloss.NewStyle().Foreground(muted).Render(usage), width-7)
		}
		second := "       " + meta
		style := lipgloss.NewStyle().Width(width).Background(bg)
		return restoreBackground(style.Render(first+"\n"+second), rowBackground(active))
	}
	if m.spaciousView() && m.mode == "search" {
		marker := " "
		bg := ink
		if active {
			marker = "▌"
			bg = raised
		}
		prefix := fmt.Sprintf("%s %2d  ", marker, index+1)
		meta := "  " + kind + " " + badge
		usage := m.currentUsage(s)
		reserved := lipgloss.Width(prefix) + lipgloss.Width(meta)
		if usage != "" {
			reserved += lipgloss.Width(usage) + 2
		}
		title := lipgloss.NewStyle().Foreground(pale).Bold(active).Render(fit(sessionTitle(s), max(0, width-reserved)))
		row := prefix + title + lipgloss.NewStyle().Foreground(muted).Render(meta)
		if usage != "" {
			row += strings.Repeat(" ", max(2, width-lipgloss.Width(row)-lipgloss.Width(usage))) +
				lipgloss.NewStyle().Foreground(muted).Render(usage)
		}
		return restoreBackground(lipgloss.NewStyle().Width(width).Background(bg).Render(row), rowBackground(active))
	}
	left := lipgloss.NewStyle().Foreground(muted).Render(fmt.Sprintf("%2d", index+1))
	prefix := "  " + left + " "
	if width >= 38 {
		app := lipgloss.NewStyle().Foreground(peach).Render(fit(kind, 5))
		if width >= 52 {
			app = lipgloss.NewStyle().Foreground(peach).Render(fmt.Sprintf("%-10s", fit(kind, 10)))
			status := lipgloss.NewStyle().Foreground(badgeColor).Bold(true).Render(fmt.Sprintf("%-7s", fit(badge, 7)))
			prefix = "  " + left + "  " + app + " " + status + " "
		} else {
			prefix = "  " + left + "  " + app + " "
		}
	}
	usage := m.listUsage(s, width)
	indicator := " "
	bg := ink
	if active {
		indicator = "▌"
		bg = raised
	}
	if m.mode == "search" && usage != "" {
		titleWidth := max(0, width-lipgloss.Width(indicator+prefix)-lipgloss.Width(usage)-1)
		title := lipgloss.NewStyle().Foreground(pale).Render(fit(sessionTitle(s), titleWidth))
		left := indicator + prefix + title
		gap := strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(usage)))
		row := left + gap + lipgloss.NewStyle().Foreground(muted).Render(usage)
		return restoreBackground(lipgloss.NewStyle().Width(width).Background(bg).Render(row), rowBackground(active))
	}
	tail := ""
	if usage != "" {
		tail = " " + lipgloss.NewStyle().Foreground(muted).Render(usage)
	}
	available := max(1, width-lipgloss.Width(prefix)-lipgloss.Width(tail)-2)
	title := lipgloss.NewStyle().Foreground(pale).Render(fit(sessionTitle(s), available))
	return restoreBackground(lipgloss.NewStyle().Width(width).Background(bg).Render(indicator+prefix+title+tail), rowBackground(active))
}

func (m model) sessionsPanel(width, limit int) string {
	rows := m.listed()
	index := m.cursor
	if m.mode == "home" {
		index -= len(quickActions)
	}
	offset := 0
	if limit > 0 && index >= limit {
		offset = index - limit + 1
	}
	offset = min(offset, max(0, len(rows)-limit))
	var content []string
	for i := offset; i < len(rows) && i < offset+limit; i++ {
		active := i == index && (m.mode != "search" || m.searchFocus == searchResults)
		content = append(content, mouseZones.Mark(fmt.Sprintf("session-%d", i), m.sessionRow(rows[i], i, width-2, active)))
	}
	if limit > 0 && len(rows) == 0 {
		content = append(content, lipgloss.NewStyle().Foreground(muted).Width(width-2).Render("  No matching sessions · ↑ for controls"))
	}
	// Reserve the border even before rows arrive so height calculations stay stable.
	if len(content) == 0 {
		content = append(content, strings.Repeat(" ", max(0, width-2)))
	}
	return mouseZones.Mark("sessions", lipgloss.NewStyle().Width(width).Border(lipgloss.RoundedBorder()).BorderForeground(outline).Background(ink).Render(strings.Join(content, "\n")))
}

func relativeAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	seconds := int64(age / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds ago", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm ago", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh ago", seconds/3600)
	default:
		return fmt.Sprintf("%dd ago", seconds/86400)
	}
}

func (m model) footer(width int) string {
	help := "a jobs · ↑↓ move · Enter open · / find · p settings · ^T history · Esc logout"
	if m.mode == "search" {
		help = "^J jobs · ↑ controls · ←→ select · Space change · ↓ results · ^T history · Esc back"
	}
	if m.finder && m.mode == "search" {
		help = "^J jobs · ↑ controls · ←→ select · Space change · ↓ results · ^T history · Esc close"
	}
	if m.mode == "settings" {
		help = "Arrows move · Space/Enter change · Esc back"
	}
	if m.compactView() {
		help = "a jobs · ↑↓ Enter / p ^T Esc"
		if m.mode == "search" {
			help = "^J jobs · ↑↓ ←→ Space ^T Tab Esc"
		}
		if m.mode == "settings" {
			help = "Arrows Space Esc"
		}
	}
	status := "Loading…"
	if !m.snapshot.At.IsZero() {
		status = relativeAge(time.Since(m.snapshot.At))
	}
	if m.refreshing {
		status = m.spinner.View() + " updating"
	}
	if m.notice != "" {
		status = fit(clean(m.notice), max(1, width/2))
	}
	if width < 34 {
		status = ""
	}
	footer := lipgloss.NewStyle().Foreground(muted).Background(surface).Width(width).Padding(0, 1)
	return mouseZones.Mark("scheduled-entry", restoreBackground(footer.Render(rightAligned(fit(help, max(1, width-lipgloss.Width(status)-3)), lipgloss.NewStyle().Foreground(mint).Render(status), width-2)), surfaceBG))
}

func (m model) home(width, limit int) string {
	rows := m.listed()
	parts := []string{m.header(width)}
	if m.spaciousView() {
		parts = append(parts, "")
	}
	if m.h >= 16 {
		parts = append(parts, m.section("QUICK LAUNCH", "o r c l s h", width))
	}
	parts = append(parts, m.actionGrid(width))
	if !m.compactView() {
		parts = append(parts, "")
	}
	caption := fmt.Sprintf("%d available · %s ^T", len(rows), m.prefs.historyLabel())
	if !m.compactView() {
		caption = fmt.Sprintf("%d available · %s ^T history · / search", len(rows), m.prefs.historyLabel())
	}
	parts = append(parts, m.section("RECENT SESSIONS", caption, width), m.filterBar(width))
	if m.spaciousView() && m.h >= 32 {
		parts = append(parts, "")
	}
	parts = append(parts, m.sessionsPanel(width, limit))
	if detail := m.sessionDetail(width, rows, resourceTotals{}); detail != "" {
		parts = append(parts, detail)
	}
	parts = append(parts, m.footer(width))
	return strings.Join(parts, "\n")
}

func (m model) searchControls(width int) string {
	names := [6]string{"Agents", "Shells", "Saved", "Density", "History", "T3"}
	states := [6]string{"○ OFF", "○ OFF", "○ OFF", "Tight", m.prefs.historyLabel(), "○ OFF"}
	if m.prefs.T3 {
		states[5] = "● ON"
	}
	for i, on := range []bool{m.prefs.Agents, m.prefs.Shells, m.prefs.Resurrect} {
		if on {
			states[i] = "● ON"
		}
	}
	switch m.prefs.Layout {
	case densityCompact:
		states[3] = "Compact"
	case densitySpacey:
		states[3] = "Spacious"
	}
	if width >= 30 && width < 36 && m.prefs.Layout == densitySpacey {
		states[3] = "Spacey"
	}
	groups := [][]int{{0, 1, 2, 3, 4, 5}}
	if width < 90 {
		groups = [][]int{{0, 1, 2}, {3, 4, 5}}
	}
	if width < 30 {
		names = [6]string{"A", "S", "R", "D", "H", "T3"}
		states[5] = strings.Split(states[5], " ")[0]
		states[0], states[1], states[2] = strings.TrimSuffix(states[0], " ON"), strings.TrimSuffix(states[1], " ON"), strings.TrimSuffix(states[2], " ON")
		states[0], states[1], states[2] = strings.TrimSuffix(states[0], " OFF"), strings.TrimSuffix(states[1], " OFF"), strings.TrimSuffix(states[2], " OFF")
		states[3] = map[density]string{densityCompact: "Cmp", densityTight: "Tgt", densitySpacey: "Spc"}[m.prefs.Layout]
	}
	var lines []string
	for _, group := range groups {
		cellWidth := (width - len(group) + 1) / len(group)
		var cells []string
		for j, index := range group {
			n := cellWidth
			if width >= 30 && width < 45 && group[0] == 3 {
				// Keep History's value readable; the T3 toggle needs less room.
				n++
			}
			if j == len(group)-1 {
				preceding := cellWidth
				if width >= 30 && width < 45 && group[0] == 3 {
					preceding++
				}
				n = width - (preceding+1)*(len(group)-1)
			}
			active := m.searchFocus == searchFocus(index+1)
			bg, fg, marker := surface, pale, " "
			if active {
				bg, fg, marker = raised, mint, "▌"
			}
			title := names[index] + " " + states[index]
			if width < 90 && width >= 30 && index < 3 {
				title = names[index] + " " + strings.Split(states[index], " ")[0]
			}
			text := lipgloss.NewStyle().Width(n).Background(bg).Foreground(fg).Render(fit(marker+title, n))
			cells = append(cells, mouseZones.Mark(fmt.Sprintf("search-control-%d", index+1), restoreBackground(text, panelBackground(active))))
		}
		lines = append(lines, strings.Join(cells, " "))
	}
	return strings.Join(lines, "\n")
}

// searchBaseHeight is the frame height with the one-row empty results panel.
// Measure without painting charts or walking up to a day of cached samples.
func (m model) searchBaseHeight(width int) int {
	height := 2 + 1 + 3 + 1 // frame borders, header, empty panel, footer
	if !m.compactView() {
		height++
	}
	if m.h >= 18 {
		height++ // FIND A SESSION
	}
	if width < 90 {
		height += 2 // inline controls wrap
	} else {
		height++
	}
	if m.h < 18 {
		height++ // compact query
	} else {
		height += 3 // bordered query
	}
	if m.h >= 12 {
		height++ // RESULTS
		if width >= 22 {
			height++ // detail current/total row
			if m.h >= 16 {
				height++ // second CPU/RSS detail row
			}
			if m.h >= 20 {
				height++ // saved exclusion and coverage
			}
			if m.h >= 28 {
				height++
			}
			if m.h >= 32 && width >= 78 {
				height++
			}
			if m.h >= 32 && width >= 60 {
				height += 2 * m.chartPlotRows() // matching chart slots
			}
		}
	}
	return height
}

func (m model) search(width, limit int) string {
	focused := m.searchFocus == searchQuery
	border, marker, bodyBG := outline, "⌕", surface
	if focused {
		border, marker, bodyBG = mint, "▌", raised
	}
	query := ""
	if m.h < 18 {
		query = restoreBackground(lipgloss.NewStyle().Width(width).Background(bodyBG).Render(marker+" "+fit(m.input.View(), width-2)), panelBackground(focused))
	} else {
		body := restoreBackground(lipgloss.NewStyle().Width(width-4).Background(bodyBG).
			Render(marker+"  "+fit(m.input.View(), width-7)), panelBackground(focused))
		query = lipgloss.NewStyle().Width(width).Padding(0, 1).
			Border(lipgloss.RoundedBorder()).BorderForeground(border).Background(ink).Render(body)
	}
	query = mouseZones.Mark("search-query", query)
	rows := m.listed()
	count := len(rows)
	caption := fmt.Sprintf("%d matches", count)
	if count == 1 {
		caption = "1 match"
	}
	var totals resourceTotals
	if m.searchFocus == searchQuery || width >= 36 && m.h >= 12 {
		totals = m.aggregateResources(rows)
	}
	if width >= 36 && m.h >= 12 {
		cpu, ram := "—", "—"
		if totals.CPUKnown > 0 {
			cpu = fmt.Sprintf("%.1f%%", totals.CPU)
		}
		if totals.RSSKnown > 0 {
			ram = briefRSS(totals.RAM)
		}
		if width >= 70 && m.h >= 24 {
			caption += fmt.Sprintf(" · ΣCPU %s %d/%d · RSS %s %d/%d", cpu, totals.CPUKnown, totals.Live, ram, totals.RSSKnown, totals.Live)
		} else {
			caption += fmt.Sprintf(" · ΣC%s R%s", cpu, ram)
		}
	}
	parts := []string{m.header(width)}
	if m.h >= 18 {
		parts = append(parts, m.section("FIND A SESSION", "search all metadata", width))
	}
	parts = append(parts, m.searchControls(width), query)
	if m.h >= 12 {
		parts = append(parts, m.section("RESULTS", caption, width))
	}
	parts = append(parts, m.sessionsPanel(width, limit))
	if detail := m.sessionDetail(width, rows, totals); detail != "" {
		parts = append(parts, detail)
	}
	parts = append(parts, m.footer(width))
	return strings.Join(parts, "\n")
}

func (m model) settingsCard(index, width int) string {
	names := []string{"Agents", "Shells", "Saved sessions", "Density", "History span", "T3 sessions"}
	values := []bool{m.prefs.Agents, m.prefs.Shells, m.prefs.Resurrect, false, false, m.prefs.T3}
	checked := ""
	if index == 4 {
		checked = m.prefs.historyLabel()
	} else if index == 3 {
		switch m.prefs.Layout {
		case densityCompact:
			checked = "Compact"
		case densitySpacey:
			checked = "Spacious"
		default:
			checked = "Tight"
		}
	} else if values[index] {
		checked = "● ON"
	} else {
		checked = "○ OFF"
	}
	active := m.cursor == index
	border := outline
	bg := surface
	fg := pale
	if active {
		border = mint
		bg = raised
		fg = mint
	}
	if m.compactView() {
		names = []string{"Agents", "Shells", "Saved", "Density", "History", "T3 ^Y"}
		if index == 4 {
			checked = m.prefs.historyLabel()
		}
		if width < 16 {
			names = []string{"A", "S", "R", "D", "H", "T3"}
			if index == 3 && width < 12 {
				checked = map[density]string{densityCompact: "Cmp", densityTight: "Tgt", densitySpacey: "Spc"}[m.prefs.Layout]
			}
		}
		name := lipgloss.NewStyle().Foreground(fg).Bold(active).Render(fit(names[index], max(1, width-lipgloss.Width(checked)-1)))
		state := lipgloss.NewStyle().Foreground(peach).Render(checked)
		return lipgloss.NewStyle().Width(width).Background(bg).Render(backgroundText(name+" "+state, panelBackground(active)))
	}
	// Apply the selected fill to the body only: border SGR resets must not
	// spread the selected color into the adjacent card's border rows.
	text := lipgloss.NewStyle().Foreground(fg).Bold(active).Render(names[index]) + "  " + lipgloss.NewStyle().Foreground(peach).Render(checked)
	return lipgloss.NewStyle().Width(width).Padding(0, 1).Border(lipgloss.RoundedBorder()).BorderForeground(border).Background(bg).Render(backgroundText(fit(text, width-6), panelBackground(active)))
}

func (m model) settings(width int) string {
	col := (width - 1) / 2
	rows := []string{
		lipgloss.JoinHorizontal(lipgloss.Top, mouseZones.Mark("setting-0", m.settingsCard(0, col)), " ", mouseZones.Mark("setting-1", m.settingsCard(1, width-col-1))),
		lipgloss.JoinHorizontal(lipgloss.Top, mouseZones.Mark("setting-2", m.settingsCard(2, col)), " ", mouseZones.Mark("setting-3", m.settingsCard(3, width-col-1))),
		lipgloss.JoinHorizontal(lipgloss.Top, mouseZones.Mark("setting-4", m.settingsCard(4, col)), " ", mouseZones.Mark("setting-5", m.settingsCard(5, width-col-1))),
	}
	parts := []string{m.header(width), m.section("PREFERENCES", "saved across sessions", width), strings.Join(rows, "\n"), m.footer(width)}
	return strings.Join(parts, "\n")
}

func (m model) frame(content string, width int) string {
	return lipgloss.NewStyle().Width(width+4).Padding(0, 1).Border(lipgloss.RoundedBorder()).BorderForeground(outline).Background(ink).Render(content)
}

func (m model) View() tea.View {
	if m.w == 0 || m.h == 0 {
		v := tea.NewView("")
		v.AltScreen = true
		return v
	}
	if m.w < 28 || m.h < 9 {
		v := tea.NewView(lipgloss.Place(max(1, m.w), max(1, m.h), lipgloss.Center, lipgloss.Center, "◇ agentbox · enlarge terminal"))
		v.AltScreen = true
		return v
	}
	width := m.contentWidth()
	margin := min(2, max(0, m.h-28))
	var content string
	switch m.mode {
	case "t3":
		content = m.t3View(width)
	case "scheduled":
		content = m.scheduledView(width)
	case "settings":
		content = m.settings(width)
	case "search":
		spare := m.h - m.searchBaseHeight(width) - margin
		content = m.search(width, max(0, spare))
	default:
		base := m.frame(m.home(width, 0), width)
		spare := m.h - lipgloss.Height(base) - margin
		if m.spaciousView() {
			spare = (spare + 1) / 2
		}
		content = m.home(width, max(0, spare))
	}
	box := m.frame(content, width)
	if lipgloss.Height(box) > m.h {
		// The smallest home surface keeps both action and filter shortcuts visible.
		box = m.frame(m.header(width)+"\n"+fit("o r c l s h / p · a jobs", width)+"\n"+m.filterBar(width)+"\n"+m.footer(width), width)
	}
	v := tea.NewView(mouseZones.Scan(restoreBackground(lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box, lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Background(ink))), inkBG) + ansi.ResetStyle))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
