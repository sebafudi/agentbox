package main

import (
	"fmt"
	"image/color"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/sparkline"
)

// resourceTotals describes the selected rows. Unknown readings are represented
// by coverage counts rather than zero-valued measurements.
type resourceTotals struct {
	CPU      float64
	RAM      uint64
	Live     int
	CPUKnown int
	RSSKnown int
	Saved    int
}

func (m model) aggregateResources(rows []Session) resourceTotals {
	var totals resourceTotals
	fresh := !m.snapshot.At.IsZero() && time.Since(m.snapshot.At) <= 30*time.Second
	for _, s := range rows {
		if s.T3 != nil && !s.Metrics.Available {
			continue // Unmapped/shared backends have no attributable process usage.
		}
		if s.Saved {
			totals.Saved++
			continue
		}
		totals.Live++
		if !fresh || !s.Metrics.Available {
			continue
		}
		totals.RSSKnown++
		totals.RAM += s.Metrics.RAM
		if s.Metrics.CPUKnown {
			totals.CPUKnown++
			totals.CPU += s.Metrics.CPU
		}
	}
	return totals
}

// aggregateHistory summarizes simultaneous readings from the selected live
// sessions. A timestamp includes only sessions observed at that instant; it
// does not stand in for missing sessions or use each session's individual peak.
func (m model) aggregateHistory(rows []Session) SessionMetrics {
	var result SessionMetrics
	if m.snapshot.At.IsZero() || time.Since(m.snapshot.At) > 30*time.Second {
		return result
	}

	start := m.snapshot.At.Add(-m.prefs.historyWindow())
	type cursor struct {
		samples []metricSample
		index   int
	}
	cursors := make([]cursor, 0, len(rows))
	for _, s := range rows {
		if s.Saved || !s.Metrics.Available {
			continue
		}
		history := s.Metrics.History
		first := sort.Search(len(history), func(i int) bool {
			return !history[i].At.Before(start)
		})
		if first < len(history) && !history[first].At.After(m.snapshot.At) {
			cursors = append(cursors, cursor{samples: history, index: first})
		}
	}
	if len(cursors) == 0 {
		return result
	}

	var firstAt, lastAt time.Time
	var ramLo, ramHi uint64
	var cpuSum float64
	for {
		var at time.Time
		for i := range cursors {
			c := &cursors[i]
			if c.index < len(c.samples) {
				next := c.samples[c.index].At
				if !next.After(m.snapshot.At) && (at.IsZero() || next.Before(at)) {
					at = next
				}
			}
		}
		if at.IsZero() {
			break
		}

		var ram uint64
		var cpu float64
		cpuKnown := false
		for i := range cursors {
			c := &cursors[i]
			if c.index >= len(c.samples) || !c.samples[c.index].At.Equal(at) {
				continue
			}
			sample := c.samples[c.index]
			c.index++
			// An implausibly large combined RSS saturates rather than wrapping
			// to a misleadingly small value.
			if sample.RAM > ^uint64(0)-ram {
				ram = ^uint64(0)
			} else {
				ram += sample.RAM
			}
			if sample.CPUKnown && !math.IsNaN(sample.CPU) && !math.IsInf(sample.CPU, 0) {
				cpu += sample.CPU
				cpuKnown = true
			}
		}
		if result.Samples == 0 {
			firstAt = at
		}
		lastAt = at
		result.Samples++
		result.MaxRAM = max(result.MaxRAM, ram)
		var carry uint64
		ramLo, carry = bits.Add64(ramLo, ram, 0)
		ramHi += carry
		if cpuKnown {
			result.CPUSamples++
			cpuSum += cpu
			result.MaxCPU = max(result.MaxCPU, cpu)
		}
	}
	if result.Samples > 0 {
		result.Available = true
		result.SpanSeconds = int(lastAt.Sub(firstAt).Seconds())
		result.AvgRAM, _ = bits.Div64(ramHi, ramLo, uint64(result.Samples))
	}
	if result.CPUSamples > 0 {
		result.CPUKnown = true
		result.AvgCPU = cpuSum / float64(result.CPUSamples)
	}
	return result
}

// resourceChart retains the individual session history when a result is selected.
// Unlike the aggregate chart, each bucket shows that session's peak sample.
func (m model) resourceChart(s Session, width int) string {
	if width < 60 || m.h < 32 || s.Saved || !s.Metrics.Available ||
		m.snapshot.At.IsZero() || time.Since(m.snapshot.At) > 30*time.Second ||
		len(s.Metrics.History) == 0 {
		return ""
	}

	window := m.prefs.historyWindow()
	start := m.snapshot.At.Add(-window)
	history := s.Metrics.History
	first := sort.Search(len(history), func(i int) bool {
		return !history[i].At.Before(start)
	})
	if first == len(history) || history[first].At.After(m.snapshot.At) {
		return ""
	}
	const labelWidth = 8
	columns := min(width-labelWidth-chartAxisWidth, 72)
	cpu, ram := make([]float64, columns), make([]float64, columns)
	cpuSeen, ramSeen := make([]bool, columns), make([]bool, columns)
	for _, sample := range history[first:] {
		elapsed := sample.At.Sub(start)
		if elapsed > window {
			break
		}
		index := min(int(elapsed*time.Duration(columns)/window), columns-1)
		if sample.CPUKnown && !math.IsNaN(sample.CPU) && !math.IsInf(sample.CPU, 0) {
			cpuSeen[index] = true
			cpu[index] = max(cpu[index], sample.CPU)
		}
		ramSeen[index] = true
		ram[index] = max(ram[index], float64(sample.RAM))
	}
	if !anySample(ramSeen) {
		return ""
	}

	return m.renderResourceChart(cpu, ram, cpuSeen, ramSeen, false)
}

// aggregateChart aligns observations to snapshot-relative buckets. Each
// session's last observation in a bucket replaces its earlier ones; the bucket
// sums only sessions with measured values there. A bucket with no observed
// RSS stays blank; unknown CPU is never turned into a zero observation.
func (m model) aggregateChart(rows []Session, width int) string {
	if width < 60 || m.h < 32 || m.snapshot.At.IsZero() ||
		time.Since(m.snapshot.At) > 30*time.Second {
		return ""
	}

	window := m.prefs.historyWindow()
	start := m.snapshot.At.Add(-window)
	const labelWidth = 8
	columns := min(width-labelWidth-chartAxisWidth, 72)
	var cpu, ram []float64
	var cpuSeen, ramSeen []bool
	for _, s := range rows {
		if s.Saved || !s.Metrics.Available {
			continue
		}
		history := s.Metrics.History
		first := sort.Search(len(history), func(i int) bool {
			return !history[i].At.Before(start)
		})
		if first == len(history) || history[first].At.After(m.snapshot.At) {
			continue
		}
		if ramSeen == nil {
			cpu, ram = make([]float64, columns), make([]float64, columns)
			cpuSeen, ramSeen = make([]bool, columns), make([]bool, columns)
		}
		// Flush each bucket once per session. In particular, an unknown latest
		// CPU observation supersedes an earlier known CPU reading in that bucket.
		bucket := -1
		var latest metricSample
		addLatest := func() {
			if bucket < 0 {
				return
			}
			ramSeen[bucket] = true
			ram[bucket] += float64(latest.RAM)
			if latest.CPUKnown && !math.IsNaN(latest.CPU) && !math.IsInf(latest.CPU, 0) {
				cpuSeen[bucket] = true
				cpu[bucket] += latest.CPU
			}
		}
		for _, sample := range history[first:] {
			if sample.At.After(m.snapshot.At) {
				break
			}
			index := min(int(sample.At.Sub(start)*time.Duration(columns)/window), columns-1)
			if index != bucket {
				addLatest()
				bucket = index
			}
			latest = sample
		}
		addLatest()
	}
	if ramSeen == nil {
		return ""
	}

	return m.renderResourceChart(cpu, ram, cpuSeen, ramSeen, true)
}

const chartAxisWidth = 12

func plotScale(values []float64, seen []bool, minimum float64) float64 {
	scale := minimum
	for i, value := range values {
		if seen[i] {
			scale = max(scale, value)
		}
	}
	return scale
}

func chartAxis(text string) string {
	return fmt.Sprintf("%*s", chartAxisWidth, text)
}

// Each graph gains two rows for every four lines of terminal height, capped
// so taller terminals still leave room for search results.
func (m model) chartPlotRows() int {
	if m.h < 32 {
		return 0
	}
	return min(8, 2+(m.h-32)/2)
}

func (m model) renderResourceChart(cpu, ram []float64, cpuSeen, ramSeen []bool, aggregate bool) string {
	cpuScale := plotScale(cpu, cpuSeen, 10)
	ramScale := plotScale(ram, ramSeen, 1)
	height := m.chartPlotRows()
	cpuRows := chartRows(cpu, cpuSeen, cpuScale, height, mint)
	ramRows := invertedRows(ram, ramSeen, ramScale, height, peach)
	cpuLabel, ramLabel := fmt.Sprintf("CPU %3s ", m.prefs.historyLabel()), fmt.Sprintf("RSS %3s ", m.prefs.historyLabel())
	if aggregate {
		cpuLabel, ramLabel = fmt.Sprintf("ΣCPU %3s", m.prefs.historyLabel()), fmt.Sprintf("ΣRSS %3s", m.prefs.historyLabel())
	}
	topCPU, zeroCPU := "", ""
	if anySample(cpuSeen) {
		topCPU, zeroCPU = fmt.Sprintf("%.1f%%", cpuScale), "0%"
	}
	const labelWidth = 8
	indent := strings.Repeat(" ", labelWidth)
	lines := make([]string, 0, 2*height)
	for y, row := range cpuRows {
		label, axis := indent, ""
		if y == 0 {
			label, axis = lipgloss.NewStyle().Foreground(mint).Bold(true).Render(cpuLabel), topCPU
		} else if y == height-1 {
			axis = zeroCPU
		}
		lines = append(lines, label+row+chartAxis(axis))
	}
	for y, row := range ramRows {
		label, axis := indent, ""
		if y == 0 {
			label, axis = lipgloss.NewStyle().Foreground(peach).Bold(true).Render(ramLabel), "0 B"
		} else if y == height-1 {
			axis = rss(uint64(ramScale))
		}
		lines = append(lines, label+row+chartAxis(axis))
	}
	return strings.Join(lines, "\n")
}

// RSS fills solid blocks downward from zero at the top. Inverted foreground
// and background colors form the missing upper eighths without dotted glyphs.
// Unobserved buckets stay blank; a thin top edge marks observed zero.
func invertedRows(values []float64, seen []bool, scale float64, height int, tone color.Color) []string {
	normal := lipgloss.NewStyle().Foreground(tone)
	inverse := lipgloss.NewStyle().Foreground(ink).Background(tone)
	glyphs := [...]rune{' ', '▔', '▆', '▅', '▀', '▃', '▂', '▁', '█'}
	var fill [9]string
	for level := 1; level <= 8; level++ {
		if level == 1 || level == 4 || level == 8 {
			fill[level] = normal.Render(string(glyphs[level]))
		} else {
			fill[level] = inverse.Render(string(glyphs[level]))
		}
	}
	rows := make([]strings.Builder, height)
	for i := range rows {
		rows[i].Grow(len(values) * 12)
	}
	for i, value := range values {
		if !seen[i] {
			for y := range rows {
				rows[y].WriteByte(' ')
			}
			continue
		}
		if value <= 0 {
			rows[0].WriteString(fill[1])
			for y := 1; y < height; y++ {
				rows[y].WriteByte(' ')
			}
			continue
		}
		level := min(height*8, max(1, int(math.Round(value*float64(height*8)/scale))))
		for y := range rows {
			filled := min(8, max(0, level-y*8))
			if filled == 0 {
				rows[y].WriteByte(' ')
			} else {
				rows[y].WriteString(fill[filled])
			}
		}
	}
	lines := make([]string, height)
	for y := range rows {
		lines[y] = restoreBackground(rows[y].String(), inkBG)
	}
	return lines
}

func anySample(seen []bool) bool {
	for _, ok := range seen {
		if ok {
			return true
		}
	}
	return false
}

// chartRows paints an ntcharts sparkline, clearing unobserved buckets after
// drawing. A thin baseline block distinguishes measured zero from a gap.
func chartRows(values []float64, seen []bool, scale float64, height int, tone color.Color) []string {
	if !anySample(seen) {
		rows := make([]string, height)
		blank := strings.Repeat(" ", len(values))
		for y := range rows {
			rows[y] = blank
		}
		return rows
	}
	style := lipgloss.NewStyle().Foreground(tone)
	chart := sparkline.New(len(values), height, sparkline.WithStyle(style),
		sparkline.WithMaxValue(scale), sparkline.WithNoAutoMaxValue())
	chart.PushAll(values)
	chart.DrawColumnsOnly()
	zero := canvas.NewCellWithStyle('▁', lipgloss.NewStyle().Foreground(muted))
	blank := canvas.NewCell(' ')
	for i, ok := range seen {
		if !ok {
			for y := 0; y < height; y++ {
				chart.Canvas.SetCell(canvas.Point{X: i, Y: y}, blank)
			}
		} else if values[i]*float64(height*8) < scale {
			chart.Canvas.SetCell(canvas.Point{X: i, Y: height - 1}, zero)
		}
	}
	return strings.Split(chart.View(), "\n")
}
