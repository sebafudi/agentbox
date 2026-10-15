package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestResourceChartTracksHistoryAndUnknownCPU(t *testing.T) {
	now := time.Now()
	m := model{h: 32, snapshot: Snapshot{At: now}}
	s := Session{Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: now.Add(-20 * time.Minute), CPU: 90, CPUKnown: true, RAM: 4096},
		{At: now.Add(-10 * time.Minute), CPU: 100, CPUKnown: false, RAM: 8192},
		{At: now.Add(-5 * time.Minute), CPU: 0, CPUKnown: true, RAM: 0},
		{At: now, CPU: 10, CPUKnown: true, RAM: 2048},
	}}}
	ramOnly := s
	ramOnly.Metrics.History = append([]metricSample(nil), s.Metrics.History...)
	for i := range ramOnly.Metrics.History {
		ramOnly.Metrics.History[i].CPUKnown = false
	}
	withoutCPU := strings.Split(ansi.Strip(m.resourceChart(ramOnly, 60)), "\n")
	if strings.TrimSpace(withoutCPU[0][8:]+withoutCPU[1]) != "" ||
		strings.TrimSpace(withoutCPU[2][8:]+withoutCPU[3]) == "" {
		t.Fatalf("RAM-only history must not invent CPU values: %q", withoutCPU)
	}
	chart := m.resourceChart(s, 60)
	rows := strings.Split(ansi.Strip(chart), "\n")
	if len(rows) != 4 {
		t.Fatalf("expected two CPU and two RSS rows, got %q", chart)
	}
	for _, row := range strings.Split(chart, "\n") {
		if got := lipgloss.Width(row); got > 60 {
			t.Fatalf("chart exceeds width: %d", got)
		}
	}
	// A CPU-unknown sample still produces a RAM column, but no CPU column.
	unknownColumn := 8 + 2*40/3
	if ch := []rune(rows[0])[unknownColumn]; ch != ' ' || []rune(rows[1])[unknownColumn] != ' ' {
		t.Fatalf("unknown CPU should not appear as zero/usage: %q", chart)
	}
	if []rune(rows[2])[unknownColumn] == ' ' && []rune(rows[3])[unknownColumn] == ' ' {
		t.Fatalf("known RAM should still be visible: %q", chart)
	}
	// Observed zero has a thin solid edge; an unknown sample has no mark.
	zeroColumn := 8 + 5*40/6
	if []rune(rows[1])[zeroColumn] != '▁' {
		t.Fatalf("observed zero CPU should differ from a missing sample: %q", chart)
	}
	if []rune(rows[2])[zeroColumn] != '▔' || []rune(rows[3])[zeroColumn] != ' ' {
		t.Fatalf("zero RSS should remain at the top of the inverted graph: %q", chart)
	}
	if !strings.Contains(rows[0]+rows[1], "█") || !strings.Contains(rows[2]+rows[3], "█") {
		t.Fatalf("recorded high samples did not render: %q", chart)
	}
	if got := utf8.RuneCountInString(rows[0]); got != 60 {
		t.Fatalf("unexpected visible width: %d", got)
	}
}

func TestGraphScaleAndDownwardRSSForSessionAndSum(t *testing.T) {
	now := time.Now()
	start := now.Add(-30 * time.Minute)
	at := func(bucket int) time.Time {
		return start.Add(time.Duration(bucket)*30*time.Minute/40 + time.Second)
	}
	s := Session{Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: at(5), CPUKnown: true, CPU: 0, RAM: 0},
		{At: at(10), CPUKnown: true, CPU: 10, RAM: 256 << 20},
		{At: at(15), CPUKnown: true, CPU: 30, RAM: 1536 << 20},
		{At: at(18), CPUKnown: true, CPU: 35, RAM: 1700 << 20},
		{At: at(20), CPUKnown: true, CPU: 40, RAM: 2 << 30},
	}}}
	m := model{h: 32, snapshot: Snapshot{At: now}}
	for _, tc := range []struct {
		name  string
		chart string
	}{
		{"session", m.resourceChart(s, 60)},
		{"sum", m.aggregateChart([]Session{s}, 60)},
	} {
		rows := strings.Split(ansi.Strip(tc.chart), "\n")
		if len(rows) != 4 {
			t.Fatalf("%s: expected two CPU and two RSS rows: %q", tc.name, tc.chart)
		}
		axis := func(row int) string { return strings.TrimSpace(string([]rune(rows[row])[8+40:])) }
		if got := [4]string{axis(0), axis(1), axis(2), axis(3)}; got != [4]string{"40.0%", "0%", "0 B", "2.0 GiB"} {
			t.Fatalf("%s: CPU max/zero or RSS zero/max scale reversed: %v", tc.name, got)
		}
		atColumn := func(row, bucket int) rune { return []rune(rows[row])[8+bucket] }
		if atColumn(2, 0) != ' ' || atColumn(3, 0) != ' ' ||
			atColumn(2, 5) != '▔' || atColumn(3, 5) != ' ' ||
			atColumn(2, 10) != '▆' || atColumn(3, 10) != ' ' ||
			atColumn(2, 15) != '█' || atColumn(3, 15) != '▀' ||
			atColumn(2, 18) != '█' || atColumn(3, 18) != '▃' ||
			atColumn(2, 20) != '█' || atColumn(3, 20) != '█' {
			t.Fatalf("%s: RSS should grow down from top zero through partial to full: %q", tc.name, tc.chart)
		}
		for _, row := range strings.Split(tc.chart, "\n") {
			if got := lipgloss.Width(row); got != 60 {
				t.Fatalf("%s: chart row is %d cells, expected 60: %q", tc.name, got, row)
			}
		}
	}
}

func TestResourceChartsGainVerticalResolutionWithoutFillingMissingBuckets(t *testing.T) {
	now := time.Now()
	start := now.Add(-30 * time.Minute)
	at := func(bucket int) time.Time {
		return start.Add(time.Duration(bucket)*30*time.Minute/40 + time.Second)
	}
	s := Session{Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: at(5), CPUKnown: true, CPU: 0, RAM: 0},
		{At: at(15), CPUKnown: true, CPU: 20, RAM: 1 << 30},
		{At: at(20), CPUKnown: true, CPU: 40, RAM: 2 << 30},
		{At: at(25), CPUKnown: false, RAM: 128 << 20},
	}}}
	m := model{snapshot: Snapshot{At: now}}
	for _, tc := range []struct{ terminal, height int }{
		{32, 2}, {36, 4}, {44, 8}, {60, 8},
	} {
		m.h = tc.terminal
		for _, graph := range []struct {
			name  string
			chart string
		}{
			{"session", m.resourceChart(s, 60)},
			{"sum", m.aggregateChart([]Session{s}, 60)},
		} {
			rows := strings.Split(ansi.Strip(graph.chart), "\n")
			if len(rows) != 2*tc.height {
				t.Fatalf("%s at height %d: got %d graph rows, want %d", graph.name, tc.terminal, len(rows), 2*tc.height)
			}
			axis := func(y int) string { return strings.TrimSpace(string([]rune(rows[y])[48:])) }
			if axis(0) != "40.0%" || axis(tc.height-1) != "0%" ||
				axis(tc.height) != "0 B" || axis(2*tc.height-1) != "2.0 GiB" {
				t.Fatalf("%s at height %d: numeric extrema moved: %q", graph.name, tc.terminal, rows)
			}
			atColumn := func(y, bucket int) rune { return []rune(rows[y])[8+bucket] }
			for y := 0; y < 2*tc.height; y++ {
				if atColumn(y, 0) != ' ' || y < tc.height && atColumn(y, 25) != ' ' {
					t.Fatalf("%s at height %d: missing or unknown sample filled row %d: %q", graph.name, tc.terminal, y, rows[y])
				}
			}
			if atColumn(tc.height-1, 5) != '▁' || atColumn(tc.height, 5) != '▔' ||
				atColumn(0, 20) != '█' || atColumn(2*tc.height-1, 20) != '█' ||
				atColumn(tc.height/2, 15) != '█' ||
				atColumn(tc.height+tc.height/2-1, 15) != '█' ||
				atColumn(tc.height+tc.height/2, 15) != ' ' {
				t.Fatalf("%s at height %d: zero, peak or midscale bar is misplaced: %q", graph.name, tc.terminal, rows)
			}
			for _, row := range strings.Split(graph.chart, "\n") {
				if got := lipgloss.Width(row); got != 60 {
					t.Fatalf("%s at height %d: chart row width %d, want 60: %q", graph.name, tc.terminal, got, row)
				}
			}
		}
	}
}

func TestResourceChartHidesUnavailableAndOldHistory(t *testing.T) {
	now := time.Now()
	s := Session{Metrics: SessionMetrics{Available: true, History: []metricSample{{At: now, CPUKnown: true, CPU: 10, RAM: 2048}}}}
	m := model{h: 36, snapshot: Snapshot{At: now}}
	if chart := m.resourceChart(s, 60); chart == "" {
		t.Fatal("live metrics did not produce a chart")
	}
	s.Saved = true
	if got := m.resourceChart(s, 60); got != "" {
		t.Fatalf("saved session has a chart: %q", got)
	}
	s.Saved = false
	s.Metrics.Available = false
	if got := m.resourceChart(s, 60); got != "" {
		t.Fatalf("unavailable metrics have a chart: %q", got)
	}
	s.Metrics.Available = true
	m.snapshot.At = now.Add(-time.Minute)
	if got := m.resourceChart(s, 60); got != "" {
		t.Fatalf("stale snapshot has a chart: %q", got)
	}
	m.snapshot.At = now
	if got := m.resourceChart(s, 59); got != "" {
		t.Fatalf("narrow viewport has a chart: %q", got)
	}
	m.h = 28
	if got := m.resourceChart(s, 60); got != "" {
		t.Fatalf("short viewport has a chart: %q", got)
	}
}

func TestResourceChartUsesSelectedLongWindow(t *testing.T) {
	now := time.Now()
	m := model{h: 32, snapshot: Snapshot{At: now}}
	s := Session{Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: now.Add(-23 * time.Hour), CPUKnown: true, CPU: 95, RAM: 20 * 1024 * 1024},
		{At: now.Add(-6 * time.Hour), CPUKnown: true, CPU: 40, RAM: 10 * 1024 * 1024},
		{At: now, CPUKnown: true, CPU: 5, RAM: 2 * 1024 * 1024},
	}}}
	for _, span := range []struct {
		minutes int
		label   string
	}{{60, "1h"}, {360, "6h"}, {720, "12h"}, {1440, "24h"}} {
		m.prefs.HistoryMinutes = span.minutes
		chart := ansi.Strip(m.resourceChart(s, 60))
		if !strings.Contains(chart, fmt.Sprintf("CPU %3s", span.label)) ||
			!strings.Contains(chart, fmt.Sprintf("RSS %3s", span.label)) {
			t.Errorf("%d-minute graph has wrong axis: %q", span.minutes, chart)
		}
	}
	oldColumn := 8 + 40/24
	m.prefs.HistoryMinutes = 1440
	long := strings.Split(ansi.Strip(m.resourceChart(s, 60)), "\n")
	if []rune(long[0])[oldColumn] == ' ' && []rune(long[1])[oldColumn] == ' ' {
		t.Fatalf("24h graph lost 23h-old CPU observation: %q", long)
	}
	m.prefs.HistoryMinutes = 720
	short := strings.Split(ansi.Strip(m.resourceChart(s, 60)), "\n")
	if []rune(short[0])[oldColumn] != ' ' || []rune(short[1])[oldColumn] != ' ' {
		t.Fatalf("12h graph retained out-of-window CPU observation: %q", short)
	}
}

func TestAggregateResourcesCoverageAndFilteredRows(t *testing.T) {
	now := time.Now()
	m := model{snapshot: Snapshot{At: now}}
	rows := []Session{
		{Name: "agent", Kind: "omp", Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 12.5, RAM: 10}},
		{Name: "shell", Kind: "shell", Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 8, RAM: 20}},
		{Name: "unknown-cpu", Kind: "shell", Metrics: SessionMetrics{Available: true, RAM: 30}},
		{Name: "unavailable", Kind: "omp", Metrics: SessionMetrics{CPUKnown: true, CPU: 100, RAM: 100}},
		{Name: "saved", Saved: true, Metrics: SessionMetrics{Available: true, CPUKnown: true, CPU: 200, RAM: 200}},
	}
	if got, want := m.aggregateResources(rows), (resourceTotals{CPU: 20.5, RAM: 60, Live: 4, CPUKnown: 2, RSSKnown: 3, Saved: 1}); got != want {
		t.Fatalf("all rows = %+v, want %+v", got, want)
	}
	if got, want := m.aggregateResources(rows[1:3]), (resourceTotals{CPU: 8, RAM: 50, Live: 2, CPUKnown: 1, RSSKnown: 2}); got != want {
		t.Fatalf("filtered rows = %+v, want %+v", got, want)
	}
	if got, want := m.aggregateResources(rows[:1]), (resourceTotals{CPU: 12.5, RAM: 10, Live: 1, CPUKnown: 1, RSSKnown: 1}); got != want {
		t.Fatalf("query-matched agent = %+v, want %+v", got, want)
	}
	if got := m.aggregateResources(nil); got != (resourceTotals{}) {
		t.Fatalf("empty filter = %+v", got)
	}
	m.snapshot.At = now.Add(-31 * time.Second)
	if got, want := m.aggregateResources(rows), (resourceTotals{Live: 4, Saved: 1}); got != want {
		t.Fatalf("stale snapshot = %+v, want %+v", got, want)
	}
	m.snapshot.At = time.Time{}
	if got, want := m.aggregateResources(rows[:2]), (resourceTotals{Live: 2}); got != want {
		t.Fatalf("missing snapshot = %+v, want %+v", got, want)
	}
}

func TestAggregateHistorySumsSimultaneousReadings(t *testing.T) {
	now := time.Now()
	t1, t2, t3 := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute)
	m := model{snapshot: Snapshot{At: now}, prefs: preferences{HistoryMinutes: 5}}
	rows := []Session{
		{Name: "a", Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: t1, CPUKnown: true, CPU: 100, RAM: 100},
			{At: t2, CPUKnown: true, CPU: 10, RAM: 10},
			{At: t3, CPUKnown: true, CPU: 50, RAM: 30},
		}}},
		{Name: "b", Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: t1, CPUKnown: true, CPU: 10, RAM: 10},
			{At: t2, CPUKnown: true, CPU: 100, RAM: 100},
		}}},
		{Name: "unknown", Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: t2, CPU: 999, RAM: 5},
		}}},
		{Name: "saved", Saved: true, Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: t1, CPUKnown: true, CPU: 1000, RAM: 1000},
		}}},
		{Name: "unavailable", Metrics: SessionMetrics{History: []metricSample{
			{At: t2, CPUKnown: true, CPU: 1000, RAM: 1000},
		}}},
	}
	got := m.aggregateHistory(rows)
	if !got.Available || !got.CPUKnown || got.Samples != 3 || got.CPUSamples != 3 ||
		got.SpanSeconds != 120 || got.AvgCPU != 90 || got.MaxCPU != 110 ||
		got.AvgRAM != 85 || got.MaxRAM != 115 {
		t.Fatalf("simultaneous sums with missing and unknown readings = %+v", got)
	}
	if got := m.aggregateHistory(rows[1:2]); !got.Available || got.Samples != 2 ||
		got.CPUSamples != 2 || got.AvgCPU != 55 || got.MaxCPU != 100 ||
		got.AvgRAM != 55 || got.MaxRAM != 100 || got.SpanSeconds != 60 {
		t.Fatalf("filtered session sums = %+v", got)
	}
}

func TestAggregateHistoryWindowCoverageAndUnavailableData(t *testing.T) {
	now := time.Now()
	m := model{snapshot: Snapshot{At: now}, prefs: preferences{HistoryMinutes: 5}}
	rows := []Session{{Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: now.Add(-23 * time.Hour), CPUKnown: true, CPU: 80, RAM: 80},
		{At: now.Add(-5*time.Minute - time.Second), CPUKnown: true, CPU: 40, RAM: 40},
		{At: now.Add(-5 * time.Minute), RAM: 0},
		{At: now.Add(-time.Minute), CPU: 999, RAM: 20},
		{At: now.Add(time.Second), CPUKnown: true, CPU: 500, RAM: 500},
	}}}}
	got := m.aggregateHistory(rows)
	if !got.Available || got.CPUKnown || got.Samples != 2 || got.CPUSamples != 0 ||
		got.SpanSeconds != 240 || got.AvgRAM != 10 || got.MaxRAM != 20 ||
		got.AvgCPU != 0 || got.MaxCPU != 0 {
		t.Fatalf("five-minute RAM-only history = %+v", got)
	}
	m.prefs.HistoryMinutes = 1440
	got = m.aggregateHistory(rows)
	if got.Samples != 4 || got.CPUSamples != 2 || got.SpanSeconds != 23*60*60-60 ||
		got.AvgCPU != 60 || got.MaxCPU != 80 || got.AvgRAM != 35 || got.MaxRAM != 80 {
		t.Fatalf("full-day history = %+v", got)
	}
	for name, selected := range map[string][]Session{
		"empty":            nil,
		"only saved":       {{Saved: true, Metrics: rows[0].Metrics}},
		"only unavailable": {{Metrics: SessionMetrics{History: rows[0].Metrics.History}}},
		"outside window": {{Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: now.Add(-6 * time.Minute), CPUKnown: true, CPU: 50, RAM: 50},
		}}}},
	} {
		m.prefs.HistoryMinutes = 5
		if got := m.aggregateHistory(selected); !reflect.DeepEqual(got, SessionMetrics{}) {
			t.Errorf("%s produced history: %+v", name, got)
		}
	}
	m.snapshot.At = now.Add(-31 * time.Second)
	if got := m.aggregateHistory(rows); !reflect.DeepEqual(got, SessionMetrics{}) {
		t.Fatalf("stale snapshot produced history: %+v", got)
	}
	m.snapshot.At = time.Time{}
	if got := m.aggregateHistory(rows); !reflect.DeepEqual(got, SessionMetrics{}) {
		t.Fatalf("missing snapshot produced history: %+v", got)
	}
}

func TestAggregateHistoryRAMDoesNotWrap(t *testing.T) {
	now := time.Now()
	m := model{snapshot: Snapshot{At: now}}
	rows := []Session{
		{Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: now.Add(-time.Minute), RAM: ^uint64(0)},
			{At: now, RAM: ^uint64(0)},
		}}},
		{Metrics: SessionMetrics{Available: true, History: []metricSample{
			{At: now.Add(-time.Minute), RAM: 1},
		}}},
	}
	got := m.aggregateHistory(rows)
	if got.Samples != 2 || got.CPUSamples != 0 || got.MaxRAM != ^uint64(0) || got.AvgRAM != ^uint64(0) {
		t.Fatalf("RSS summation wrapped around: %+v", got)
	}
}

func TestAggregateChartAlignsAndUsesLatestPerSession(t *testing.T) {
	now := time.Now()
	m := model{h: 32, snapshot: Snapshot{At: now}}
	start := now.Add(-30 * time.Minute)
	at := func(bucket int, offset time.Duration) time.Time {
		return start.Add(time.Duration(bucket) * 30 * time.Minute / 40).Add(offset)
	}
	first := Session{Name: "agent", Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: start.Add(-time.Second), CPUKnown: true, CPU: 900, RAM: 900},
		{At: at(10, time.Second), CPUKnown: true, CPU: 100, RAM: 200},
		{At: at(10, 4*time.Second), CPUKnown: true, CPU: 5, RAM: 20},
		{At: at(30, time.Second), CPUKnown: true, CPU: 80, RAM: 50},
		{At: at(35, time.Second), CPUKnown: true, CPU: 80, RAM: 50},
		{At: now.Add(time.Second), CPUKnown: true, CPU: 900, RAM: 900},
	}}}
	second := Session{Name: "shell", Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: at(10, 7*time.Second), CPUKnown: true, CPU: 10, RAM: 30},
		{At: at(20, time.Second), CPUKnown: true, CPU: 500, RAM: 500},
		{At: at(20, 3*time.Second), CPUKnown: false, CPU: 500, RAM: 33},
		{At: at(30, 5*time.Second), CPUKnown: true, CPU: 40, RAM: 50},
		{At: at(35, 3*time.Second), CPUKnown: false, CPU: 500, RAM: 25},
	}}}
	chart := m.aggregateChart([]Session{first, second}, 60)
	lines := strings.Split(ansi.Strip(chart), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ΣCPU 30m") || !strings.HasPrefix(lines[2], "ΣRSS 30m") {
		t.Fatalf("aggregate chart labels/rows = %q", chart)
	}
	for _, line := range strings.Split(chart, "\n") {
		if lipgloss.Width(line) != 60 {
			t.Fatalf("chart width %d, want 60: %q", lipgloss.Width(line), chart)
		}
	}
	for _, line := range strings.Split(m.aggregateChart([]Session{first, second}, 100), "\n") {
		if got := lipgloss.Width(line); got > 100 {
			t.Fatalf("wide chart overflowed: %d", got)
		}
	}
	atColumn := func(row, bucket int) rune { return []rune(lines[row])[8+bucket] }
	if atColumn(0, 10) != ' ' || atColumn(1, 10) == ' ' || atColumn(2, 10) != '█' || atColumn(3, 10) != ' ' {
		t.Fatalf("bucket 10 should sum latest low observations (CPU=15, RAM=50), not peaks: %q", chart)
	}
	if atColumn(0, 20) != ' ' || atColumn(1, 20) != ' ' ||
		(atColumn(2, 20) == ' ' && atColumn(3, 20) == ' ') {
		t.Fatalf("latest unknown CPU must leave a gap while RSS remains measured: %q", chart)
	}
	if atColumn(0, 30) == ' ' || atColumn(0, 30) == atColumn(0, 35) {
		t.Fatalf("two sessions' aligned CPU readings must sum (120 > 80): %q", chart)
	}
	if atColumn(0, 35) == ' ' && atColumn(1, 35) == ' ' {
		t.Fatalf("unknown CPU in one session erased another session's known CPU: %q", chart)
	}
	if atColumn(0, 0) != ' ' || atColumn(1, 0) != ' ' ||
		atColumn(2, 0) != ' ' || atColumn(3, 0) != ' ' ||
		atColumn(0, 39) != ' ' || atColumn(1, 39) != ' ' {
		t.Fatalf("out-of-window and future observations should not fill gaps: %q", chart)
	}
	if got := m.aggregateChart([]Session{first}, 60); got == chart {
		t.Fatal("filtered row set did not change graph")
	}
	if got := m.aggregateChart(nil, 60); got != "" {
		t.Fatalf("no matching rows generated graph: %q", got)
	}
}

func TestAggregateChartAvailabilityAndHistoryWindows(t *testing.T) {
	now := time.Now()
	m := model{h: 32, snapshot: Snapshot{At: now}}
	observed := Session{Metrics: SessionMetrics{Available: true, History: []metricSample{
		{At: now.Add(-23 * time.Hour), CPUKnown: true, CPU: 50, RAM: 20},
		{At: now.Add(-10 * time.Minute), CPUKnown: false, RAM: 0},
	}}}
	saved := observed
	saved.Saved = true
	unavailable := observed
	unavailable.Metrics.Available = false
	if got := m.aggregateChart([]Session{saved, unavailable}, 60); got != "" {
		t.Fatalf("unavailable or saved history generated graph: %q", got)
	}
	if got := m.aggregateChart([]Session{{Metrics: SessionMetrics{Available: true, History: []metricSample{{At: now.Add(-31 * time.Minute), RAM: 3}}}}}, 60); got != "" {
		t.Fatalf("out-of-window history generated graph: %q", got)
	}
	chart := m.aggregateChart([]Session{observed}, 60)
	lines := strings.Split(ansi.Strip(chart), "\n")
	if strings.TrimSpace(string([]rune(lines[0])[8:])+lines[1]) != "" || []rune(lines[2])[8+26] != '▔' {
		t.Fatalf("unknown CPU must not be drawn, measured zero RAM must be marked: %q", chart)
	}
	m.prefs.HistoryMinutes = 1440
	long := strings.Split(ansi.Strip(m.aggregateChart([]Session{observed}, 60)), "\n")
	if !strings.HasPrefix(long[0], "ΣCPU 24h") || ([]rune(long[0])[8+40/24] == ' ' && []rune(long[1])[8+40/24] == ' ') {
		t.Fatalf("24h window omitted 23h observation: %q", long)
	}
	m.prefs.HistoryMinutes = 720
	short := strings.Split(ansi.Strip(m.aggregateChart([]Session{observed}, 60)), "\n")
	if []rune(short[0])[8+40/24] != ' ' || []rune(short[1])[8+40/24] != ' ' {
		t.Fatalf("12h window included 23h observation: %q", short)
	}
	m.snapshot.At = now.Add(-time.Minute)
	if got := m.aggregateChart([]Session{observed}, 60); got != "" {
		t.Fatalf("stale snapshot generated graph: %q", got)
	}
	m.snapshot.At = now
	if got := m.aggregateChart([]Session{observed}, 59); got != "" {
		t.Fatalf("narrow chart: %q", got)
	}
	m.h = 31
	if got := m.aggregateChart([]Session{observed}, 60); got != "" {
		t.Fatalf("short viewport chart: %q", got)
	}
}
