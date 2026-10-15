package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Scheduler output is untrusted terminal text; never replay escape/control sequences.
func schedulerText(value string) string {
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func scheduledTime(value string) string {
	if value == "" {
		return "—"
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.Local().Format("02 Jan 15:04:05 MST")
	}
	return clean(value)
}

func scheduledDuration(r scheduledRun) string {
	if r.DurationMs != nil {
		return (time.Duration(*r.DurationMs) * time.Millisecond).Round(time.Second).String()
	}
	start, err := time.Parse(time.RFC3339Nano, r.StartedAt)
	if err != nil {
		return "—"
	}
	end, err := time.Parse(time.RFC3339Nano, r.FinishedAt)
	if r.Status == "running" {
		end, err = time.Now(), nil
	}
	if err != nil {
		return "—"
	}
	return end.Sub(start).Round(time.Second).String()
}

func scheduledRunResult(r scheduledRun) string {
	if r.Outcome != "" && r.Outcome != r.Status {
		return r.Status + " · " + r.Outcome
	}
	return r.Status
}

func scheduledRunAction(r scheduledRun) string {
	if r.Status != "running" && r.ContinuationBlocked != "" {
		return "continuation unavailable; Enter views log"
	}
	if r.commandLogOnly() {
		return "m view command log (no agent session)"
	}
	if r.Status == "running" {
		return "m monitor (read-only)"
	}
	return "m resume exact session"
}

func schedulerRunnerLabel(runner string) string {
	switch runner {
	case "omp":
		return "OMP"
	case "claude-print":
		return "Claude"
	case "claude-sdk":
		return "Claude SDK"
	case "command":
		return "Command"
	default:
		return clean(runner)
	}
}

func scheduledState(j scheduledJob) string {
	if j.Status == "running" {
		if !j.Enabled {
			return "running · paused"
		}
		return "running"
	}
	if !j.Enabled {
		return "paused"
	}
	return "enabled · " + j.Status
}

func schedulerWrap(text string, width int) []string {
	text = strings.ReplaceAll(schedulerText(text), "\t", "    ")
	return strings.Split(ansi.Hardwrap(text, max(1, width), true), "\n")
}

func schedulerWindow(total, selected, count int) (int, int) {
	count = max(1, count)
	start := max(0, min(selected-count/2, total-count))
	return start, min(total, start+count)
}

func (m model) scheduledView(width int) string {
	s := m.scheduled
	// Frame takes two rows. This surface does not fall back to the home screen.
	budget := max(1, m.h-2)
	heading := label("SCHEDULED JOBS", mint)
	if s.page != "list" {
		heading += label(" / "+strings.ToUpper(s.page), muted)
	}
	help := "/ search · ↑↓ select · Enter details · n/^N new · e edit · d delete · Space pause · r run · ^R · Esc"
	switch s.page {
	case "detail":
		help = "↑↓ runs · Enter log · m monitor/log/resume · x stop · e edit · d delete · Space pause · r run · PgUp/Dn · Esc"
	case "log":
		help = "↑↓ PgUp/Dn scroll · End tail · m monitor/log/resume · x stop · ^R refresh · Esc details"
	case "edit":
		help = "Tab/↑↓ field · ←→/Space select · c custom · ^O model cycle · ^S save · Esc cancel"
	case "delete":
		help = "y / Enter permanently delete job + history + logs · Esc / n cancel"
	}
	if width < 55 {
		help = "↑↓ Enter · ^N n e d r\n/ Tab Space ^R · Esc"
		switch s.page {
		case "detail":
			help = "↑↓ Enter m x e d r\nPgUp/Dn Space · Esc"
		case "log":
			help = "↑↓ PgUp/Dn End m x ^R\nEsc details"
		case "edit":
			help = "^S save Esc cancel\nTab ↑↓ ←→ ^O c"
		case "delete":
			help = "y delete all\nEsc/n cancel"
		}
	}
	status := s.notice
	if !s.loaded {
		status = "Connecting to scheduler…"
	}
	if s.refreshing {
		status = "Refreshing…"
	}
	if s.mutating {
		status = "Saving / sending request…"
	}
	if s.err != "" {
		status = s.err
		if s.loaded && s.page != "edit" && s.page != "delete" {
			status += " · showing last snapshot"
		}
	}
	if s.page == "log" && s.logErr != "" {
		status = s.logErr
	}
	if status == "" && s.page == "edit" && s.fields[schedulerFieldRunner].Value() == "omp" && s.field == schedulerFieldModel {
		if s.modelLoading {
			status = "Loading OMP model catalog…"
		} else if s.modelErr != "" {
			status = s.modelErr
		}
	}
	if status == "" {
		status = "Retained history · local scheduler"
	}
	// Reserve visible controls and error/status even at the minimum terminal height.
	helpLines := schedulerWrap(help, width)
	if len(helpLines) > 2 {
		helpLines = helpLines[:2]
	}
	for len(helpLines) < 2 {
		helpLines = append(helpLines, "")
	}
	statusLines := schedulerWrap(status, width)
	if len(statusLines) > 2 {
		statusLines = statusLines[:2]
	}
	for len(statusLines) < 2 {
		statusLines = append(statusLines, "")
	}
	available := max(1, budget-1-len(helpLines)-len(statusLines))
	var body []string
	switch s.page {
	case "edit":
		body = m.scheduledEditor(width, available)
	case "delete":
		if available < 4 {
			body = append(body, fit("Delete "+s.deleteName+" ("+s.deleteID+")?", width), fit("Job/history/logs", width))
		} else {
			body = append(body, schedulerWrap("Delete "+s.deleteName+" ("+s.deleteID+")?", width)...)
			body = append(body, schedulerWrap("Irreversible: removes this job, all retained history and logs. Running jobs or open continuations cannot be deleted.", width)...)
		}
	case "detail":
		body = m.scheduledDetail(width, available)
	case "log":
		if s.logLoading && s.log.Run.ID == "" {
			body = []string{"Loading selected run log…"}
		} else {
			lines := s.logLines(width)
			start := min(s.offset, max(0, len(lines)-max(1, available-1)))
			body = append(body, fit(fmt.Sprintf("%s · %d/%d", scheduledRunAction(s.log.Run), start+1, len(lines)), width))
			body = append(body, lines[start:min(len(lines), start+max(0, available-1))]...)
		}
	default:
		s.query.SetWidth(max(1, width-8))
		body = append(body, mouseZones.Mark("scheduled-query", "Find > "+s.query.View()))
		rows := s.jobs()
		if len(rows) == 0 {
			text := "No matching scheduled jobs."
			if !s.loaded {
				text = "Scheduler data unavailable until connected."
			}
			body = append(body, fit(text, width))
		} else {
			selected := 0
			for i, j := range rows {
				if j.ID == s.jobID {
					selected = i
				}
			}
			stride := 1
			if !m.compactView() && available >= 5 {
				stride = 2
			}
			if m.spaciousView() && available >= 8 {
				stride = 3
			}
			start, end := schedulerWindow(len(rows), selected, max(1, (available-1)/stride))
			for i := start; i < end; i++ {
				j := rows[i]
				prefix := "  "
				if j.ID == s.jobID {
					prefix = "› "
				}
				result := ""
				if j.LastRun != nil {
					result = " · last " + scheduledRunResult(*j.LastRun)
				}
				text := fit(prefix+clean(j.Name)+" · "+schedulerRunnerLabel(j.Runner)+" · "+scheduledState(j)+result, width)
				if j.ID == s.jobID {
					text = lipgloss.NewStyle().Foreground(mint).Background(raised).Width(width).Render(text)
				}
				body = append(body, mouseZones.Mark(fmt.Sprintf("scheduled-job-%d", i), text))
				if stride >= 2 {
					body = append(body, fit("  "+clean(j.Schedule)+" · "+clean(j.Timezone)+" · next "+scheduledTime(j.NextRun), width))
				}
				if stride == 3 {
					body = append(body, "")
				}
			}
		}
	}
	if len(body) > available {
		body = body[:available]
	}
	parts := []string{fit(heading, width)}
	parts = append(parts, body...)
	for len(parts) < budget-len(helpLines)-len(statusLines) {
		parts = append(parts, "")
	}
	for _, line := range statusLines {
		parts = append(parts, label(fit(line, width), peach))
	}
	for _, line := range helpLines {
		parts = append(parts, label(fit(line, width), muted))
	}
	return strings.Join(parts, "\n")
}

func (m model) scheduledEditor(width, available int) []string {
	s := m.scheduled
	names := []string{"ID (immutable after creation)", "Name", "Runner (Space / ←→)", "Schedule (five-field cron)", "Timezone (IANA)", "Cwd (absolute directory)", "Timeout (minutes)", "Argv (JSON array; NOT shell)", "Prompt", "Model: ←→, c custom", "Enabled"}
	title := "Editing " + clean(s.editID) + " · permissions unchanged"
	if s.editCreating {
		title = "New job · starts paused · permissions use server defaults"
	}
	rows := []string{fit(title, width)}
	selected := 0
	for _, i := range s.editorFields() {
		prefix := "  "
		if s.field == i {
			prefix = "› "
			selected = len(rows) + 1
		}
		if i == schedulerFieldEnabled {
			enabled := "paused"
			if s.editEnabled {
				enabled = "enabled"
			}
			if width < 28 {
				enabled = "off"
				if s.editEnabled {
					enabled = "on"
				}
			}
			rows = append(rows, fit(prefix+"Enabled: "+enabled+" (Space)", width))
			if s.field == i {
				selected = len(rows) - 1
			}
			continue
		}
		f := s.fields[i]
		rows = append(rows, fit(prefix+names[i], width))
		if i == schedulerFieldRunner {
			rows = append(rows, fit("  ‹ "+schedulerRunnerLabel(f.Value())+" ›", width))
		} else if i == schedulerFieldModel && !s.editModelCustom {
			value := f.Value()
			if value == "" {
				value = "default"
			}
			rows = append(rows, fit("  ‹ "+value+" ›", width))
		} else if i == schedulerFieldModel {
			f.SetWidth(max(1, width-11))
			rows = append(rows, fit("  Custom: "+f.View(), width))
		} else {
			if s.field == i {
				f.SetWidth(max(1, width-3))
				rows = append(rows, fit("  "+f.View(), width))
			} else {
				rows = append(rows, fit("  "+schedulerText(f.Value()), width))
			}
		}
	}
	start, end := schedulerWindow(len(rows), selected, available)
	if selected > 0 && available >= 2 && start == selected {
		start--
		end = min(len(rows), start+available)
	}
	return rows[start:end]
}

func scheduledConfigLines(j scheduledJob, width int) []string {
	lines := []string{}
	add := func(text string) { lines = append(lines, schedulerWrap(text, width)...) }
	add(j.Name + " · " + j.ID + " · " + scheduledState(j))
	add("Schedule: " + j.Schedule + " · " + j.Timezone)
	add("Next: " + scheduledTime(j.NextRun))
	if j.LastRun != nil {
		add("Last: " + scheduledTime(j.LastRun.StartedAt) + " · " + scheduledRunResult(*j.LastRun) + " · " + scheduledDuration(*j.LastRun))
	}
	add(fmt.Sprintf("Retained: %d total · %d success · %d failed · %d running", j.Stats.Total, j.Stats.Success, j.Stats.Failed, j.Stats.Running))
	if j.Runner == "command" {
		add(fmt.Sprintf("Runner: command · timeout: %dm", j.TimeoutMinutes))
		add("Command arguments hidden; e opens the configuration editor.")
	} else {
		modelName := j.Model
		if modelName == "" {
			modelName = "default"
		}
		add(fmt.Sprintf("Runner: %s · model: %s · timeout: %dm", j.Runner, modelName, j.TimeoutMinutes))
		add("Permissions: " + j.PermissionMode + " (not editable)")
		add("Prompt hidden; e opens the configuration editor.")
	}
	add("Directory: " + j.Cwd)
	return lines
}

func (s schedulerUI) logLines(width int) []string {
	r := s.log.Run
	exit := "—"
	if r.ExitCode != nil {
		exit = fmt.Sprint(*r.ExitCode)
	}
	text := fmt.Sprintf("Run: %s\nStatus: %s · exit: %s · duration: %s\nStarted: %s\nFinished: %s\nReason: %s\nDirectory: %s\nLog: %s\nSession: %s%s\n\n%s",
		r.ID, scheduledRunResult(r), exit, scheduledDuration(r), scheduledTime(r.StartedAt), scheduledTime(r.FinishedAt), r.Reason, r.Cwd, r.Log, r.SessionDir, r.SessionID, s.log.Output)
	return schedulerWrap(text, width)
}

func (m model) scheduledMaxOffset() int {
	s := m.scheduled
	available := max(1, m.h-7)
	if s.page == "log" {
		return max(0, len(s.logLines(m.contentWidth()))-max(1, available-1))
	}
	if s.page == "detail" {
		j, ok := s.selectedJob()
		if !ok {
			return 0
		}
		historyHeight := min(max(2, available/2), len(s.runs())+2)
		return max(0, len(scheduledConfigLines(j, m.contentWidth()))-max(1, available-historyHeight))
	}
	return 0
}

func (m model) scheduledDetail(width, available int) []string {
	s := m.scheduled
	j, ok := s.selectedJob()
	if !ok {
		return []string{"Job no longer exists. Esc returns to jobs."}
	}
	lines := scheduledConfigLines(j, width)
	runs := s.runs()
	historyHeight := min(max(2, available/2), len(runs)+2)
	configHeight := max(1, available-historyHeight)
	start := min(s.offset, max(0, len(lines)-configHeight))
	body := append([]string{}, lines[start:min(len(lines), start+configHeight)]...)
	body = append(body, label(fmt.Sprintf("RUN HISTORY (%d) · ↑↓ select", len(runs)), peach))
	selected := 0
	for i, r := range runs {
		if r.ID == s.runID {
			selected = i
		}
	}
	from, to := schedulerWindow(len(runs), selected, max(1, available-len(body)-1))
	for i := from; i < to; i++ {
		r := runs[i]
		prefix := "  "
		if r.ID == s.runID {
			prefix = "› "
		}
		text := fit(prefix+scheduledTime(r.StartedAt)+" · "+scheduledRunResult(r)+" · "+scheduledDuration(r), width)
		if r.ID == s.runID {
			text = label(text, mint)
		}
		body = append(body, mouseZones.Mark(fmt.Sprintf("scheduled-run-%d", i), text))
	}
	if r, ok := s.selectedRun(); ok {
		join := scheduledRunAction(r)
		if r.Reason != "" {
			join += " · " + r.Reason
		}
		body = append(body, fit(clean(join), width))
	} else {
		body = append(body, "No retained runs.")
	}
	return body
}
