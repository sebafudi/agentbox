package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type schedulerUI struct {
	snapshot                         schedulerSnapshot
	loaded                           bool
	query                            textinput.Model
	queryFocus                       bool
	jobID, runID                     string
	page, returnMode                 string
	refreshing, mutating, logLoading bool
	generation, logGeneration        uint64
	err, notice                      string
	log                              schedulerRunDetail
	logErr                           string
	offset                           int
	fields                           []textinput.Model
	field                            int
	editID                           string
	editEnabled                      bool
	editOriginal                     scheduledJob
	editValues                       []string
	editCreating                     bool
	editReturnPage                   string
	deleteID, deleteName             string
	deleteReturnPage                 string
	models                           []string
	modelLoading, modelsLoaded       bool
	modelErr                         string
	editModelCustom                  bool
	editModels                       map[string]schedulerModelDraft
	editCustomModels                 map[string]string
}

type schedulerSnapshotMsg struct {
	generation uint64
	snapshot   schedulerSnapshot
	err        error
}

type schedulerMutationMsg struct {
	generation uint64
	editID     string
	jobID      string
	runID      string
	operation  string
	err        error
}

type schedulerLogMsg struct {
	generation uint64
	id         string
	detail     schedulerRunDetail
	err        error
}

type schedulerModelCatalogMsg struct {
	models []string
	err    error
}

type schedulerModelDraft struct {
	value  string
	custom bool
}

const (
	schedulerFieldID = iota
	schedulerFieldName
	schedulerFieldRunner
	schedulerFieldSchedule
	schedulerFieldTimezone
	schedulerFieldCwd
	schedulerFieldTimeout
	schedulerFieldArgv
	schedulerFieldPrompt
	schedulerFieldModel
	schedulerFieldEnabled
)

var schedulerJobID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

var schedulerRunners = []string{"command", "omp", "claude-print", "claude-sdk"}
var schedulerDefaultModels = []string{""}
var schedulerClaudeModels = []string{"", "opus", "sonnet", "fable", "claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-haiku-4-5-20251001"}

func newSchedulerUI() schedulerUI {
	q := textinput.New()
	q.Prompt = ""
	q.Placeholder = "name, ID, schedule, path…"
	q.CharLimit = 180
	q.SetVirtualCursor(true)
	return schedulerUI{query: q, page: "list"}
}

func (s schedulerUI) jobs() []scheduledJob {
	query := strings.ToLower(strings.TrimSpace(s.query.Value()))
	rows := make([]scheduledJob, 0, len(s.snapshot.Jobs))
	for _, j := range s.snapshot.Jobs {
		if matches(strings.ToLower(j.Name+" "+j.ID+" "+j.Schedule+" "+j.Timezone+" "+j.Runner+" "+j.Cwd), query) {
			rows = append(rows, j)
		}
	}
	return rows
}

func (s schedulerUI) selectedJob() (scheduledJob, bool) {
	for _, j := range s.snapshot.Jobs {
		if j.ID == s.jobID {
			return j, true
		}
	}
	return scheduledJob{}, false
}

func (s schedulerUI) runs() []scheduledRun {
	rows := make([]scheduledRun, 0)
	for _, r := range s.snapshot.Runs {
		if r.JobID == s.jobID {
			rows = append(rows, r)
		}
	}
	return rows
}

func (s schedulerUI) selectedRun() (scheduledRun, bool) {
	for _, r := range s.runs() {
		if r.ID == s.runID {
			return r, true
		}
	}
	return scheduledRun{}, false
}

func (s *schedulerUI) reconcile() {
	jobs := s.jobs()
	found := false
	for _, j := range jobs {
		if j.ID == s.jobID {
			found = true
			break
		}
	}
	if !found && s.page == "list" {
		s.jobID, s.runID = "", ""
		if len(jobs) > 0 {
			s.jobID = jobs[0].ID
		}
	}
	runs := s.runs()
	found = false
	for _, r := range runs {
		if r.ID == s.runID {
			found = true
			break
		}
	}
	if !found && s.page != "log" {
		s.runID = ""
		if len(runs) > 0 {
			s.runID = runs[0].ID
		}
	}
}

func (s *schedulerUI) refresh() tea.Cmd {
	if s.refreshing || s.mutating {
		return nil
	}
	s.refreshing = true
	s.generation++
	return schedulerLoad(s.generation)
}

func schedulerLoad(generation uint64) tea.Cmd {
	socket := schedulerSocket()
	return func() tea.Msg {
		var snapshot schedulerSnapshot
		err := schedulerRequest(context.Background(), socket, http.MethodGet, "/snapshot", nil, &snapshot)
		return schedulerSnapshotMsg{generation, snapshot, err}
	}
}

func (s *schedulerUI) fetchLog() tea.Cmd {
	if s.runID == "" || s.logLoading {
		return nil
	}
	s.logLoading = true
	s.logGeneration++
	generation, id, socket := s.logGeneration, s.runID, schedulerSocket()
	return func() tea.Msg {
		var detail schedulerRunDetail
		err := schedulerRequest(context.Background(), socket, http.MethodGet, scheduledRunPath(id), nil, &detail)
		if err == nil && detail.Run.ID != id {
			err = fmt.Errorf("scheduler returned a different run")
		}
		return schedulerLogMsg{generation, id, detail, err}
	}
}

func (s *schedulerUI) mutate(path string, body any, editID string) tea.Cmd {
	if s.mutating {
		return nil
	}
	s.mutating, s.refreshing = true, false
	s.generation++ // Invalidate any snapshot that started before this write.
	s.err, s.notice = "", ""
	generation, socket := s.generation, schedulerSocket()
	jobID, runID, operation := s.jobID, "", "update enabled state"
	switch path {
	case "/run":
		operation = "run"
	case "/runs/stop":
		runID, operation = s.runID, "stop"
	case "/jobs/delete":
		jobID, operation = s.deleteID, "delete job"
	case "/jobs":
		operation = "create job"
	}
	if editID != "" {
		jobID = editID
		if path != "/jobs" {
			operation = "save configuration"
		}
	}
	return func() tea.Msg {
		err := schedulerRequest(context.Background(), socket, http.MethodPost, path, body, nil)
		return schedulerMutationMsg{generation: generation, editID: editID, jobID: jobID, runID: runID, operation: operation, err: err}
	}
}

func (m model) openScheduledJobs() (tea.Model, tea.Cmd) {
	m.scheduled.returnMode = m.mode
	m.mode = "scheduled"
	m.input.Blur()
	m.scheduled.queryFocus = true
	m.scheduled.page = "list"
	m.scheduled.query.Focus()
	return m, tea.Batch(m.scheduled.refresh(), textinput.Blink)
}

func (s *schedulerUI) moveJob(delta int) {
	rows := s.jobs()
	for i, j := range rows {
		if j.ID == s.jobID {
			next := max(0, min(len(rows)-1, i+delta))
			if rows[next].ID != s.jobID {
				s.jobID, s.runID = rows[next].ID, ""
				s.reconcile()
			}
			return
		}
	}
}

func (s *schedulerUI) moveRun(delta int) {
	rows := s.runs()
	for i, r := range rows {
		if r.ID == s.runID {
			s.runID = rows[max(0, min(len(rows)-1, i+delta))].ID
			return
		}
	}
}

func (s *schedulerUI) startCreate() {
	cwd, _ := os.Getwd()
	s.startEditor(scheduledJob{Runner: "command", Schedule: "0 9 * * *", Timezone: "UTC", Cwd: cwd, TimeoutMinutes: 30}, true)
}

func (s *schedulerUI) startEdit(j scheduledJob) {
	s.startEditor(j, false)
}

func (s *schedulerUI) startEditor(j scheduledJob, creating bool) {
	s.editID, s.editEnabled, s.editCreating = j.ID, j.Enabled, creating
	s.editOriginal = j
	s.editOriginal.Argv = slices.Clone(j.Argv)
	s.editReturnPage = "detail"
	if creating {
		s.editReturnPage = s.page
	}
	s.page, s.offset = "edit", 0
	s.err, s.notice = "", ""
	s.queryFocus = false
	s.query.Blur()
	argv, _ := json.Marshal(j.Argv)
	if len(j.Argv) == 0 {
		argv = []byte("[]")
	}
	values := []string{j.ID, j.Name, j.Runner, j.Schedule, j.Timezone, j.Cwd, strconv.Itoa(j.TimeoutMinutes), string(argv), j.Prompt, j.Model}
	s.fields = make([]textinput.Model, schedulerFieldEnabled)
	s.editValues = make([]string, len(values))
	for i, value := range values {
		f := textinput.New()
		f.Prompt = ""
		f.CharLimit = 4096
		switch i {
		case schedulerFieldID:
			f.CharLimit = 40
		case schedulerFieldName, schedulerFieldModel:
			f.CharLimit = 100
		case schedulerFieldPrompt, schedulerFieldArgv:
			f.CharLimit = 200_000
		}
		f.SetVirtualCursor(true)
		f.SetValue(value)
		s.fields[i], s.editValues[i] = f, f.Value()
	}
	s.field = schedulerFieldName
	if creating {
		s.field = schedulerFieldID
	}
	s.fields[s.field].Focus()
	s.editModelCustom = j.Model != "" && !slices.Contains(s.modelOptions(j.Runner), j.Model)
	s.editModels = map[string]schedulerModelDraft{j.Runner: {j.Model, s.editModelCustom}}
	s.editCustomModels = map[string]string{j.Runner: j.Model}
}

func (s schedulerUI) editorFields() []int {
	fields := make([]int, 0, schedulerFieldEnabled+1)
	if s.editCreating {
		fields = append(fields, schedulerFieldID)
	}
	fields = append(fields, schedulerFieldName, schedulerFieldRunner, schedulerFieldSchedule, schedulerFieldTimezone, schedulerFieldCwd, schedulerFieldTimeout)
	if s.fields[schedulerFieldRunner].Value() == "command" {
		fields = append(fields, schedulerFieldArgv)
	} else {
		fields = append(fields, schedulerFieldPrompt, schedulerFieldModel)
	}
	return append(fields, schedulerFieldEnabled)
}

func (s schedulerUI) fieldChanged(field int) bool {
	return s.fields[field].Value() != s.editValues[field]
}

func schedulerArgv(value string) ([]string, error) {
	var items []any
	if err := json.Unmarshal([]byte(value), &items); err != nil || len(items) == 0 {
		return nil, fmt.Errorf("argv must be a nonempty JSON array of strings, for example [\"/usr/bin/printf\",\"hello world\"]")
	}
	argv := make([]string, len(items))
	for i, item := range items {
		arg, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("argv[%d] must be a JSON string", i)
		}
		argv[i] = arg
		if strings.ContainsRune(arg, '\x00') {
			return nil, fmt.Errorf("argv[%d] must not contain NUL", i)
		}
	}
	if strings.TrimSpace(argv[0]) == "" {
		return nil, fmt.Errorf("argv[0] must name an executable")
	}
	return argv, nil
}

func (s schedulerUI) editorConfig() (map[string]any, error) {
	value := func(field int) string { return strings.TrimSpace(s.fields[field].Value()) }
	id := s.editID
	if s.editCreating {
		id = value(schedulerFieldID)
		if !schedulerJobID.MatchString(id) {
			return nil, fmt.Errorf("ID must be 1–40 lowercase letters, digits or hyphens, starting with a letter or digit")
		}
		for _, j := range s.snapshot.Jobs {
			if j.ID == id {
				return nil, fmt.Errorf("ID %q already exists; use edit instead", id)
			}
		}
	}
	name, runner := value(schedulerFieldName), value(schedulerFieldRunner)
	if name == "" || len(name) > 100 {
		return nil, fmt.Errorf("name must be 1–100 characters")
	}
	if !slices.Contains(schedulerRunners, runner) {
		return nil, fmt.Errorf("select command, omp, claude-print or claude-sdk")
	}
	schedule, zone, cwd := value(schedulerFieldSchedule), value(schedulerFieldTimezone), value(schedulerFieldCwd)
	if len(strings.Fields(schedule)) != 5 {
		return nil, fmt.Errorf("schedule must contain five cron fields")
	}
	if zone == "" {
		return nil, fmt.Errorf("timezone is required (for example Europe/Warsaw)")
	}
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("cwd must be an absolute directory")
	}
	timeout, err := strconv.Atoi(value(schedulerFieldTimeout))
	if err != nil || timeout < 1 || timeout > 1440 {
		return nil, fmt.Errorf("timeout must be 1–1440 whole minutes")
	}
	config := map[string]any{"id": id, "name": name, "runner": runner, "schedule": schedule, "timezone": zone, "cwd": cwd, "timeoutMinutes": timeout, "enabled": s.editEnabled}
	if runner == "command" {
		argv, err := schedulerArgv(s.fields[schedulerFieldArgv].Value())
		if err != nil {
			return nil, err
		}
		config["argv"] = argv
	} else {
		prompt, model := s.fields[schedulerFieldPrompt].Value(), s.fields[schedulerFieldModel].Value()
		// Text inputs normalize controls/newlines. Unedited values retain their
		// exact persisted form instead of authorizing that incidental normalization.
		if !s.editCreating && !s.fieldChanged(schedulerFieldPrompt) {
			prompt = s.editOriginal.Prompt
		}
		if !s.editCreating && runner == s.editOriginal.Runner && !s.fieldChanged(schedulerFieldModel) {
			model = s.editOriginal.Model
		}
		if strings.TrimSpace(prompt) == "" || len(prompt) > 200_000 {
			return nil, fmt.Errorf("prompt must be 1–200000 characters")
		}
		if len(model) > 100 {
			return nil, fmt.Errorf("model must be at most 100 characters")
		}
		config["prompt"], config["model"] = prompt, model
	}
	// The server validates full cron syntax, timezone and directory existence.
	return config, nil
}

func (s schedulerUI) editPatch() (map[string]any, error) {
	config, err := s.editorConfig()
	if err != nil {
		return nil, err
	}
	original := s.editOriginal
	changes := make(map[string]any)
	for _, field := range []struct {
		index int
		key   string
		value string
	}{
		{schedulerFieldName, "name", original.Name},
		{schedulerFieldRunner, "runner", original.Runner},
		{schedulerFieldSchedule, "schedule", original.Schedule},
		{schedulerFieldTimezone, "timezone", original.Timezone},
		{schedulerFieldCwd, "cwd", original.Cwd},
		{schedulerFieldPrompt, "prompt", original.Prompt},
	} {
		if v, active := config[field.key]; active && s.fieldChanged(field.index) && v != field.value {
			changes[field.key] = v
		}
	}
	if model, active := config["model"]; active && (s.fieldChanged(schedulerFieldModel) || config["runner"] != original.Runner) && model != original.Model {
		changes["model"] = model
	}
	if timeout := config["timeoutMinutes"].(int); s.fieldChanged(schedulerFieldTimeout) && timeout != original.TimeoutMinutes {
		changes["timeoutMinutes"] = timeout
	}
	if argv, active := config["argv"]; active && s.fieldChanged(schedulerFieldArgv) && !slices.Equal(argv.([]string), original.Argv) {
		changes["argv"] = argv
	}
	if s.editEnabled != original.Enabled {
		changes["enabled"] = s.editEnabled
	}
	return changes, nil
}

func (s *schedulerUI) openLog() tea.Cmd {
	s.page, s.offset, s.logErr = "log", 0, ""
	s.log = schedulerRunDetail{}
	s.logLoading = false
	s.logGeneration++
	return s.fetchLog()
}

func (s *schedulerUI) startDelete(j scheduledJob) {
	s.deleteID, s.deleteName, s.deleteReturnPage = j.ID, j.Name, s.page
	s.page, s.err, s.notice = "delete", "", ""
}

func (s *schedulerUI) cycleRunner(delta int) {
	field := &s.fields[schedulerFieldRunner]
	s.editModels[field.Value()] = schedulerModelDraft{s.fields[schedulerFieldModel].Value(), s.editModelCustom}
	if s.editModelCustom {
		s.editCustomModels[field.Value()] = s.fields[schedulerFieldModel].Value()
	}
	index := slices.Index(schedulerRunners, field.Value())
	index = (index + delta + len(schedulerRunners)) % len(schedulerRunners)
	runner := schedulerRunners[index]
	field.SetValue(runner)
	draft := s.editModels[runner]
	s.fields[schedulerFieldModel].SetValue(draft.value)
	s.editModelCustom = draft.custom
}

func (s schedulerUI) modelOptions(runner string) []string {
	if runner == "omp" {
		if len(s.models) == 0 {
			return schedulerDefaultModels
		}
		return s.models
	}
	return schedulerClaudeModels
}

func (s *schedulerUI) cycleModel(delta int) {
	field := &s.fields[schedulerFieldModel]
	runner := s.fields[schedulerFieldRunner].Value()
	options := s.modelOptions(runner)
	if s.editModelCustom {
		s.editCustomModels[runner] = field.Value()
	}
	index := slices.Index(options, field.Value())
	if s.editModelCustom || index < 0 {
		index = len(options)
	}
	index = (index + delta + len(options) + 1) % (len(options) + 1)
	s.editModelCustom = index == len(options)
	if !s.editModelCustom {
		field.SetValue(options[index])
		field.Blur()
	} else {
		field.SetValue(s.editCustomModels[runner])
		field.Focus()
	}
}

func (s *schedulerUI) loadModels() tea.Cmd {
	if s.modelsLoaded || s.modelLoading {
		return nil
	}
	s.modelLoading = true
	return func() tea.Msg {
		data, err := command(context.Background(), 5*time.Second, "omp", "models", "--json")
		if err != nil {
			return schedulerModelCatalogMsg{err: fmt.Errorf("OMP model catalog unavailable; default/custom remain available")}
		}
		var catalog struct {
			Models []struct {
				Selector string `json:"selector"`
			} `json:"models"`
		}
		if err := json.Unmarshal(data, &catalog); err != nil {
			return schedulerModelCatalogMsg{err: fmt.Errorf("Invalid OMP model catalog; default/custom remain available")}
		}
		models := make([]string, 1, len(catalog.Models)+1)
		for _, entry := range catalog.Models {
			if entry.Selector != "" && len(entry.Selector) <= 100 && !slices.Contains(models, entry.Selector) {
				models = append(models, entry.Selector)
			}
		}
		return schedulerModelCatalogMsg{models: models}
	}
}

func (m model) updateScheduled(message tea.Msg) (tea.Model, tea.Cmd) {
	s := &m.scheduled
	s.offset = min(s.offset, m.scheduledMaxOffset())
	switch msg := message.(type) {
	case schedulerModelCatalogMsg:
		s.modelLoading, s.modelsLoaded = false, true
		s.models = msg.models
		if msg.err != nil {
			s.modelErr = msg.err.Error()
		}
		if s.page == "edit" && s.fields[schedulerFieldRunner].Value() == "omp" && !s.fieldChanged(schedulerFieldModel) && slices.Contains(s.models, s.fields[schedulerFieldModel].Value()) {
			s.editModelCustom = false
			s.editModels["omp"] = schedulerModelDraft{s.fields[schedulerFieldModel].Value(), false}
		}
		return m, nil
	case schedulerSnapshotMsg:
		if msg.generation != s.generation {
			return m, nil
		}
		s.refreshing = false
		if msg.err != nil {
			s.err = msg.err.Error()
			return m, nil
		}
		s.snapshot, s.loaded = msg.snapshot, true
		if s.page != "edit" && s.page != "delete" {
			s.err = ""
		}
		// Editor buffers and editID intentionally survive refresh and job removal.
		s.reconcile()
		return m, nil
	case schedulerMutationMsg:
		if msg.generation != s.generation {
			return m, nil
		}
		s.mutating = false
		current := m.mode == "scheduled" && msg.jobID == s.jobID &&
			(msg.runID == "" || msg.runID == s.runID)
		if msg.editID != "" {
			current = m.mode == "scheduled" && s.page == "edit" && s.editID == msg.editID
		} else if msg.operation == "delete job" {
			current = m.mode == "scheduled" && s.page == "delete" && s.deleteID == msg.jobID
		}
		// A request still settles after navigation, but its feedback belongs only
		// to its originating selection. Successful writes always refresh state.
		if !current {
			if msg.err == nil {
				return m, s.refresh()
			}
			return m, nil
		}
		if msg.err != nil {
			s.err = msg.operation + ": " + msg.err.Error()
			return m, nil
		}
		s.notice = msg.operation + " accepted"
		if msg.editID != "" {
			s.page, s.jobID, s.runID = "detail", msg.editID, ""
			s.fields, s.editValues = nil, nil
			s.editCreating = false
		} else if msg.operation == "delete job" {
			s.page, s.jobID, s.runID = "list", "", ""
			s.deleteID, s.deleteName = "", ""
		}
		return m, s.refresh()
	case schedulerLogMsg:
		if msg.generation != s.logGeneration {
			return m, nil
		}
		s.logLoading = false
		if msg.id != s.runID || s.page != "log" {
			return m, nil
		}
		if msg.err != nil {
			s.logErr = msg.err.Error()
		} else {
			s.log, s.logErr = msg.detail, ""
		}
		return m, nil
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return m, nil
		}
		if s.page == "list" {
			if mouseZones.Get("scheduled-query").InBounds(msg) {
				s.queryFocus = true
				s.query.Focus()
				return m, textinput.Blink
			}
			for i, j := range s.jobs() {
				if mouseZones.Get(fmt.Sprintf("scheduled-job-%d", i)).InBounds(msg) {
					if !s.queryFocus && s.jobID == j.ID {
						s.page, s.offset = "detail", 0
					}
					s.jobID, s.queryFocus = j.ID, false
					s.query.Blur()
					s.reconcile()
					return m, nil
				}
			}
		}
		if s.page == "detail" {
			for i, r := range s.runs() {
				if mouseZones.Get(fmt.Sprintf("scheduled-run-%d", i)).InBounds(msg) {
					s.runID = r.ID
					return m, nil
				}
			}
		}
		return m, nil
	case tea.MouseWheelMsg:
		delta := 1
		if msg.Button == tea.MouseWheelUp {
			delta = -1
		}
		switch s.page {
		case "list":
			s.moveJob(delta)
		case "detail":
			s.moveRun(delta)
		case "log":
			s.offset = max(0, s.offset+delta)
		}
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if s.page == "edit" {
			return m.updateScheduledEditor(msg)
		}
		if s.page == "delete" {
			switch key {
			case "esc", "n":
				s.page, s.err = s.deleteReturnPage, ""
				s.deleteID, s.deleteName = "", ""
			case "y", "enter":
				return m, s.mutate("/jobs/delete", map[string]string{"id": s.deleteID}, "")
			}
			return m, nil
		}
		if key == "ctrl+n" && s.page != "log" && !s.mutating {
			s.startCreate()
			return m, tea.Batch(textinput.Blink, s.loadModels())
		}
		if key == "esc" {
			s.err, s.notice = "", ""
			switch s.page {
			case "log":
				s.page, s.offset = "detail", 0
			case "detail":
				s.page, s.offset = "list", 0
			default:
				m.mode = s.returnMode
				if m.mode != "search" {
					m.mode = "home"
				}
				s.query.Blur()
				if m.mode == "search" && m.searchFocus == searchQuery {
					m.input.Focus()
				}
			}
			return m, nil
		}
		if s.page == "list" && s.queryFocus {
			switch key {
			case "down", "tab", "enter":
				s.queryFocus = false
				s.query.Blur()
				return m, nil
			case "ctrl+r":
				return m, s.refresh()
			default:
				var cmd tea.Cmd
				s.query, cmd = s.query.Update(msg)
				s.reconcile()
				return m, cmd
			}
		}
		switch key {
		case "ctrl+r":
			if s.page == "log" {
				return m, s.fetchLog()
			}
			return m, s.refresh()
		case "/", "tab":
			if s.page == "list" {
				s.queryFocus = true
				s.query.Focus()
				return m, textinput.Blink
			}
		case "up", "k":
			switch s.page {
			case "list":
				s.moveJob(-1)
			case "detail":
				s.moveRun(-1)
			case "log":
				s.offset = max(0, s.offset-1)
			}
		case "down", "j":
			switch s.page {
			case "list":
				s.moveJob(1)
			case "detail":
				s.moveRun(1)
			case "log":
				s.offset++
			}
		case "pgup":
			s.offset = max(0, s.offset-max(1, m.h/2))
		case "pgdown":
			s.offset += max(1, m.h/2)
		case "home":
			s.offset = 0
		case "end":
			s.offset = 1 << 30
		case "enter", "l":
			if s.page == "list" {
				if _, ok := s.selectedJob(); ok {
					s.page, s.offset = "detail", 0
				}
			} else if s.page == "detail" && s.runID != "" {
				return m, s.openLog()
			}
		case "n":
			if s.page != "log" && !s.mutating {
				s.startCreate()
				return m, tea.Batch(textinput.Blink, s.loadModels())
			}
		case "d":
			if s.page != "log" && !s.mutating {
				if j, ok := s.selectedJob(); ok {
					s.startDelete(j)
				}
			}
		case "e":
			if s.page != "log" && !s.mutating {
				if j, ok := s.selectedJob(); ok {
					s.startEdit(j)
					return m, tea.Batch(textinput.Blink, s.loadModels())
				}
			}
		case "space":
			if s.page != "log" {
				if j, ok := s.selectedJob(); ok {
					return m, s.mutate("/jobs/patch", map[string]any{"id": j.ID, "changes": map[string]any{"enabled": !j.Enabled}}, "")
				}
			}
		case "r":
			if s.page != "log" {
				if j, ok := s.selectedJob(); ok {
					return m, s.mutate("/run", map[string]string{"id": j.ID}, "")
				}
			}
		case "x":
			if s.page != "list" {
				if r, ok := s.selectedRun(); ok && r.Status == "running" {
					return m, s.mutate("/runs/stop", map[string]string{"id": r.ID}, "")
				}
			}
		case "m":
			if s.page != "list" {
				if r, ok := s.selectedRun(); ok {
					if r.Status != "running" && r.ContinuationBlocked != "" {
						s.err = clean(r.ContinuationBlocked)
						return m, nil
					}
					if r.commandLogOnly() {
						return m, s.openLog()
					}
					if r.Status != "running" && !r.hasWrappedSession() && (r.Cwd == "" || (r.Runner == "omp" && r.SessionDir == "") || (r.Runner != "omp" && r.SessionID == "")) {
						s.err = "Exact session metadata unavailable for this run; inspect its log instead."
						return m, nil
					}
					m.action = Action{Verb: "scheduled-join", Name: r.ID}
					return m, tea.Quit
				}
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	if s.page == "edit" && s.field < len(s.fields) {
		s.fields[s.field], cmd = s.fields[s.field].Update(message)
	} else if s.queryFocus {
		s.query, cmd = s.query.Update(message)
	}
	return m, cmd
}

func (m model) updateScheduledEditor(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := &m.scheduled
	if msg.String() == "esc" {
		s.page, s.err = s.editReturnPage, ""
		s.fields, s.editValues = nil, nil
		return m, nil
	}
	if s.mutating {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+s":
		if s.editCreating {
			config, err := s.editorConfig()
			if err != nil {
				s.err = err.Error()
				return m, nil
			}
			s.editID = config["id"].(string)
			return m, s.mutate("/jobs", config, s.editID)
		}
		patch, err := s.editPatch()
		if err != nil {
			s.err = err.Error()
			return m, nil
		}
		if len(patch) == 0 {
			s.page, s.err, s.notice = "detail", "", "No configuration changes"
			s.fields, s.editValues = nil, nil
			return m, nil
		}
		return m, s.mutate("/jobs/patch", map[string]any{"id": s.editID, "changes": patch}, s.editID)
	case "tab", "down", "shift+tab", "up", "enter":
		if s.field < len(s.fields) {
			s.fields[s.field].Blur()
		}
		delta := 1
		if msg.String() == "shift+tab" || msg.String() == "up" {
			delta = -1
		}
		visible := s.editorFields()
		index := slices.Index(visible, s.field)
		s.field = visible[(index+delta+len(visible))%len(visible)]
		if s.field < len(s.fields) && s.field != schedulerFieldRunner && (s.field != schedulerFieldModel || s.editModelCustom) {
			s.fields[s.field].Focus()
		}
		return m, textinput.Blink
	case "space":
		if s.field == schedulerFieldEnabled {
			s.editEnabled = !s.editEnabled
			return m, nil
		}
		if s.field == schedulerFieldRunner {
			s.cycleRunner(1)
			return m, nil
		}
		if s.field == schedulerFieldModel && !s.editModelCustom {
			s.cycleModel(1)
			return m, textinput.Blink
		}
	case "left", "right":
		delta := 1
		if msg.String() == "left" {
			delta = -1
		}
		if s.field == schedulerFieldRunner {
			s.cycleRunner(delta)
			return m, nil
		}
		if s.field == schedulerFieldModel && !s.editModelCustom {
			s.cycleModel(delta)
			return m, textinput.Blink
		}
	case "ctrl+o":
		if s.field == schedulerFieldModel {
			s.cycleModel(1)
			return m, textinput.Blink
		}
	case "c":
		if s.field == schedulerFieldModel && !s.editModelCustom {
			s.editModelCustom = true
			s.fields[schedulerFieldModel].SetValue(s.editCustomModels[s.fields[schedulerFieldRunner].Value()])
			s.fields[schedulerFieldModel].Focus()
			return m, textinput.Blink
		}
	}
	if s.field < len(s.fields) && s.field != schedulerFieldRunner && (s.field != schedulerFieldModel || s.editModelCustom) {
		var cmd tea.Cmd
		s.fields[s.field], cmd = s.fields[s.field].Update(msg)
		return m, cmd
	}
	return m, nil
}
