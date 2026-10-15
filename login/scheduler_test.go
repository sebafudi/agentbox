package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func schedulerTestSocket(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "scheduler-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

func TestSchedulerDelayedMutationDoesNotReportAgainstNewSelection(t *testing.T) {
	for _, operation := range []struct {
		name  string
		path  string
		runID string
	}{
		{name: "run", path: "/run"},
		{name: "toggle", path: "/jobs/patch"},
		{name: "stop", path: "/runs/stop", runID: "new"},
	} {
		for _, success := range []bool{false, true} {
			outcome := "failure"
			if success {
				outcome = "success"
			}
			t.Run(operation.name+"/"+outcome, func(t *testing.T) {
				m := scheduledTestModel(t)
				socket := schedulerTestSocket(t, func(w http.ResponseWriter, r *http.Request) {
					if !success {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error":"originating operation refused"}`))
						return
					}
					_, _ = w.Write([]byte(`{"ok":true}`))
				})
				t.Setenv("SCHEDULER_SOCKET", socket)
				id := m.scheduled.jobID
				if operation.runID != "" {
					id = operation.runID
				}
				cmd := m.scheduled.mutate(operation.path, map[string]string{"id": id}, "")
				if cmd == nil || !m.scheduled.mutating {
					t.Fatal("operation did not become pending")
				}
				// Change selection before executing/delivering the queued request.
				// A stop also needs isolation from another run of the same job.
				if operation.runID == "" {
					m.scheduled.jobID = "beta"
				} else {
					m.scheduled.runID = "old"
				}
				m.scheduled.err, m.scheduled.notice = "current selection error", "current selection notice"
				message := cmd().(schedulerMutationMsg)
				if message.jobID != "alpha" || message.runID != operation.runID || message.operation == "" {
					t.Fatalf("request lost its originating context: %+v", message)
				}
				next, refresh := m.Update(message)
				m = next.(model)
				if m.scheduled.mutating {
					t.Fatal("departed selection left operation pending")
				}
				if m.scheduled.err != "current selection error" || m.scheduled.notice != "current selection notice" {
					t.Fatal("delayed response replaced feedback for the current selection")
				}
				if success != (refresh != nil) || m.scheduled.refreshing != success {
					t.Fatal("successful departed operation did not refresh authoritative data")
				}
			})
		}
	}
}

func scheduledTestModel(t *testing.T) model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := initialModel(false, "")
	m.mode, m.w, m.h = "scheduled", 80, 24
	m.scheduled.loaded = true
	m.scheduled.snapshot = schedulerSnapshot{
		Jobs: []scheduledJob{
			{ID: "alpha", Name: "Alpha", Schedule: "0 9 * * *", Timezone: "Europe/Warsaw", Cwd: "/project", TimeoutMinutes: 30, Enabled: true, Prompt: strings.Repeat("long ", 100) + "needle", Runner: "omp"},
			{ID: "beta", Name: "Beta", Schedule: "0 8 * * *", Timezone: "UTC", Cwd: "/project", TimeoutMinutes: 10, Runner: "command", Argv: []string{"/usr/bin/true"}},
		},
		Runs: []scheduledRun{
			{ID: "new", JobID: "alpha", Status: "running", Runner: "omp"},
			{ID: "old", JobID: "alpha", Status: "success", Runner: "omp", Cwd: "/project", SessionDir: "/exact/session"},
		},
	}
	m.scheduled.reconcile()
	return m
}

func TestSchedulerSnapshotPreservesIDsAndDraft(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.jobID, m.scheduled.runID = "alpha", "old"
	job, _ := m.scheduled.selectedJob()
	m.scheduled.startEdit(job)
	m.scheduled.fields[schedulerFieldSchedule].SetValue("15 7 * * 1")
	m.scheduled.err = "invalid draft"
	m.scheduled.generation = 3
	snapshot := m.scheduled.snapshot
	snapshot.Jobs = []scheduledJob{snapshot.Jobs[1], snapshot.Jobs[0]}
	snapshot.Runs = append([]scheduledRun{{ID: "newer", JobID: "alpha"}}, snapshot.Runs...)
	next, _ := m.Update(schedulerSnapshotMsg{generation: 3, snapshot: snapshot})
	m = next.(model)
	if m.scheduled.jobID != "alpha" || m.scheduled.runID != "old" || m.scheduled.fields[schedulerFieldSchedule].Value() != "15 7 * * 1" || m.scheduled.err != "invalid draft" {
		t.Fatalf("refresh changed selection or editor draft: %+v", m.scheduled)
	}
	next, _ = m.Update(schedulerSnapshotMsg{generation: 2, snapshot: schedulerSnapshot{}})
	if len(next.(model).scheduled.snapshot.Jobs) != 2 {
		t.Fatal("stale snapshot replaced current data")
	}
	next, _ = m.Update(schedulerMutationMsg{generation: 2, editID: "alpha"})
	if next.(model).scheduled.page != "edit" {
		t.Fatal("stale write response closed editor")
	}
}

func TestSchedulerSearchAndNestedEscape(t *testing.T) {
	m := scheduledTestModel(t)
	m.mode, m.finder = "search", true
	m.input.SetValue("existing query")
	m.cursor = 7
	next, _ := m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	m = next.(model)
	if m.mode != "scheduled" || m.cursor != 7 {
		t.Fatal("jobs entry disrupted finder state")
	}
	m.scheduled.query.SetValue("alpha warsaw")
	m.scheduled.reconcile()
	if rows := m.scheduled.jobs(); len(rows) != 1 || rows[0].ID != "alpha" {
		t.Fatal("search did not match name and timezone")
	}
	job, _ := m.scheduled.selectedJob()
	m.scheduled.startEdit(job)
	m.scheduled.fields[schedulerFieldTimezone].SetValue("unsaved")
	for _, want := range []string{"detail", "list", "search"} {
		next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		m = next.(model)
		if want == "search" {
			if m.mode != want {
				t.Fatalf("Esc mode %s", m.mode)
			}
		} else if m.scheduled.page != want {
			t.Fatalf("Esc page %s", m.scheduled.page)
		}
		if m.action.Verb != "" {
			t.Fatal("nested escape requested logout")
		}
	}
	if m.input.Value() != "existing query" || m.cursor != 7 {
		t.Fatal("finder state lost on return")
	}
	m.mode, m.cursor = "home", 0
	next, _ = m.Update(tea.KeyPressMsg{Code: 'j'})
	if next.(model).mode != "home" || next.(model).cursor != 2 {
		t.Fatal("home j navigation was hijacked")
	}
}

func TestSchedulerPatchOnlyChangesEditableConfiguration(t *testing.T) {
	m := scheduledTestModel(t)
	j, _ := m.scheduled.selectedJob()
	j.PermissionMode = "bypassPermissions"
	m.scheduled.startEdit(j)
	m.scheduled.fields[schedulerFieldSchedule].SetValue("30 10 * * 1-5")
	m.scheduled.fields[schedulerFieldTimeout].SetValue("45")
	patch, err := m.scheduled.editPatch()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"schedule": "30 10 * * 1-5", "timeoutMinutes": 45}
	if !reflect.DeepEqual(patch, want) {
		t.Fatalf("patch unexpectedly authorizes/replaces fields: %#v", patch)
	}
	m.scheduled.fields[schedulerFieldTimeout].SetValue("0")
	if _, err := m.scheduled.editPatch(); err == nil {
		t.Fatal("invalid timeout accepted")
	}
	m.scheduled.fields[schedulerFieldTimeout].SetValue("45")
	socket := schedulerTestSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/jobs/patch" {
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			ID      string         `json:"id"`
			Changes map[string]any `json:"changes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ID != "alpha" || len(body.Changes) != 2 {
			t.Errorf("bad patch body: %+v", body)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"timezone rejected"}`))
	})
	t.Setenv("SCHEDULER_SOCKET", socket)
	next, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	if cmd == nil || !m.scheduled.mutating {
		t.Fatal("save did not launch asynchronous request")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.scheduled.page != "edit" || m.scheduled.fields[schedulerFieldSchedule].Value() != "30 10 * * 1-5" || !strings.Contains(m.scheduled.err, "timezone rejected") {
		t.Fatal("server validation lost editor or error")
	}
}

func TestSchedulerDraftSavePreservesConcurrentConfiguration(t *testing.T) {
	m := scheduledTestModel(t)
	j, _ := m.scheduled.selectedJob()
	j.TimeoutMinutes = 3
	m.scheduled.startEdit(j)
	persisted := map[string]any{
		"id": j.ID, "schedule": j.Schedule, "timezone": j.Timezone,
		"timeoutMinutes": j.TimeoutMinutes, "model": j.Model, "enabled": j.Enabled,
		"name": j.Name, "cwd": j.Cwd, "runner": j.Runner, "prompt": j.Prompt,
		"argv": []string{"/old/command", "secret argument"}, "permissionMode": "dontAsk",
		"allowedTools": []string{"Bash(git status:*)"}, "additionalDirectories": []string{"/extra"},
	}
	var lock sync.Mutex
	socket := schedulerTestSocket(t, func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []any{persisted}, "runs": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/jobs/patch":
			var body struct {
				ID      string         `json:"id"`
				Changes map[string]any `json:"changes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if body.ID != j.ID || len(body.Changes) == 0 {
				t.Errorf("invalid patch target or empty changes: %+v", body)
			}
			for field, value := range body.Changes {
				persisted[field] = value
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"job": persisted})
		default:
			http.NotFound(w, r)
		}
	})
	t.Setenv("SCHEDULER_SOCKET", socket)
	// A separate API client changes the live configuration while this draft
	// remains open. Even receiving that newer snapshot must not reset its baseline.
	external := map[string]any{"id": j.ID, "changes": map[string]any{
		"timeoutMinutes": 4, "model": "external-model", "enabled": false, "name": "External name", "cwd": "/external",
		"prompt": "External prompt", "argv": []string{"/external/command", "space preserved"}, "permissionMode": "acceptEdits",
		"allowedTools": []string{"Read"}, "additionalDirectories": []string{"/other"},
	}}
	if err := schedulerRequest(context.Background(), socket, http.MethodPost, "/jobs/patch", external, nil); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(schedulerLoad(m.scheduled.generation)())
	m = next.(model)
	if m.scheduled.fields[schedulerFieldTimeout].Value() != "3" || !m.scheduled.editEnabled {
		t.Fatal("external update overwrote the open draft")
	}
	m.scheduled.fields[schedulerFieldSchedule].SetValue("15 11 * * 1")
	next, save := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	if save == nil {
		t.Fatal("schedule change did not send a save")
	}
	next, refresh := m.Update(save())
	m = next.(model)
	if refresh == nil {
		t.Fatal("successful save did not fetch authoritative configuration")
	}
	next, _ = m.Update(refresh())
	m = next.(model)
	saved, ok := m.scheduled.selectedJob()
	if !ok || saved.Schedule != "15 11 * * 1" || saved.TimeoutMinutes != 4 ||
		saved.Model != "external-model" || saved.Enabled || saved.Name != "External name" ||
		saved.Cwd != "/external" || saved.Prompt != "External prompt" || saved.PermissionMode != "acceptEdits" ||
		!reflect.DeepEqual(saved.Argv, []string{"/external/command", "space preserved"}) {
		t.Fatalf("draft save overwrote a concurrent untouched field: %+v", saved)
	}
	lock.Lock()
	defer lock.Unlock()
	if !reflect.DeepEqual(persisted["allowedTools"], []any{"Read"}) || !reflect.DeepEqual(persisted["additionalDirectories"], []any{"/other"}) {
		t.Fatal("draft save changed uneditable tool/directory configuration")
	}
}

func TestSchedulerUnchangedDraftDoesNotSendEmptyPatch(t *testing.T) {
	m := scheduledTestModel(t)
	j, _ := m.scheduled.selectedJob()
	m.scheduled.startEdit(j)
	m.scheduled.fields[schedulerFieldSchedule].SetValue("15 11 * * 1")
	m.scheduled.fields[schedulerFieldSchedule].SetValue(j.Schedule)
	m.scheduled.editEnabled = !j.Enabled
	m.scheduled.editEnabled = j.Enabled
	next, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	if cmd != nil || m.scheduled.mutating || m.scheduled.page != "detail" {
		t.Fatal("unchanged/reverted draft attempted an empty server mutation")
	}
}

func TestSchedulerLogSelectionDoesNotFollowLatest(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.page, m.scheduled.runID = "log", "old"
	m.scheduled.logGeneration = 4
	next, _ := m.Update(schedulerLogMsg{generation: 3, id: "new", detail: schedulerRunDetail{Output: "wrong"}})
	m = next.(model)
	if m.scheduled.log.Output != "" {
		t.Fatal("stale log applied")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: 'm'})
	if next.(model).action.Name != "old" || next.(model).action.Verb != "scheduled-join" {
		t.Fatal("join did not capture selected run")
	}
	m.scheduled.snapshot.Runs = m.scheduled.snapshot.Runs[:1]
	m.scheduled.reconcile()
	if m.scheduled.runID != "old" {
		t.Fatal("pruned log silently switched to another run")
	}
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'm'})
	if cmd != nil || next.(model).action.Verb != "" {
		t.Fatal("join launched a different retained run")
	}
}

func TestSchedulerRequestErrorsAndCancellation(t *testing.T) {
	socket := schedulerTestSocket(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/snapshot":
			_, _ = w.Write([]byte(`{"jobs":[],"runs":[]}`))
		case "/invalid":
			_, _ = w.Write([]byte(`not json`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"run not found"}`))
		}
	})
	var result schedulerSnapshot
	if err := schedulerRequest(context.Background(), socket, http.MethodGet, "/snapshot", nil, &result); err != nil {
		t.Fatal(err)
	}
	if err := schedulerRequest(context.Background(), socket, http.MethodGet, "/invalid", nil, &result); err == nil {
		t.Fatal("malformed response accepted")
	}
	if err := schedulerRequest(context.Background(), socket, http.MethodGet, "/missing", nil, &result); err == nil || !strings.Contains(err.Error(), "run not found") {
		t.Fatalf("missing API error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := schedulerRequest(ctx, socket, http.MethodGet, "/snapshot", nil, &result); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	m := scheduledTestModel(t)
	next, _ := m.Update(schedulerSnapshotMsg{err: errors.New("scheduler unavailable")})
	if next.(model).scheduled.err == "" || len(next.(model).scheduled.snapshot.Jobs) != 2 {
		t.Fatal("unavailable service discarded cached data or hid failure")
	}
}

func TestSchedulerJoinReusesOnlyLiveExactRunSessions(t *testing.T) {
	run := scheduledRun{ID: "run with ; shell text", Status: "running"}
	base := schedulerJoinName(run)
	if name, live := scheduledSessionName(base, "other [Created]\n"+base+" [Created]"); name != base || !live {
		t.Fatal("live exact monitor was not reused")
	}
	name, live := scheduledSessionName(base, base+" [Created] (EXITED)")
	if live || name == base {
		t.Fatal("old saved writer layout could resurrect")
	}
	args, err := schedulerJoinArgs(run.ID, name, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args[len(args)-4:], []string{"--", "scheduler", "join", run.ID}) {
		t.Fatalf("join did not pass exact ID as literal argument: %q", args)
	}
	run.Status = "success"
	if schedulerJoinName(run) == base {
		t.Fatal("completed resume reused headless monitor identity")
	}
}

func TestSchedulerSurfacesFitTerminalAndStripLogControls(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.log = schedulerRunDetail{Run: m.scheduled.snapshot.Runs[0], Output: "safe\x1b[2J\x1b]52;c;secret\a\x00\n" + strings.Repeat("wide界", 100)}
	if text := schedulerText(m.scheduled.log.Output); strings.ContainsAny(text, "\x1b\a\x00") {
		t.Fatal("log can emit terminal controls")
	}
	for _, size := range [][2]int{{28, 9}, {42, 16}, {80, 24}, {120, 48}} {
		for _, page := range []string{"list", "detail", "log", "edit", "delete"} {
			m.w, m.h = size[0], size[1]
			m.scheduled.page = page
			if page == "edit" {
				j, _ := m.scheduled.selectedJob()
				m.scheduled.startEdit(j)
			}
			if page == "delete" {
				j, _ := m.scheduled.selectedJob()
				m.scheduled.startDelete(j)
			}
			for _, layout := range []density{densityCompact, densityTight, densitySpacey} {
				m.prefs.Layout = layout
				content := m.View().Content
				if lipgloss.Height(content) > m.h {
					t.Fatalf("%s %v exceeds height", page, size)
				}
				for _, line := range strings.Split(content, "\n") {
					if lipgloss.Width(line) > m.w {
						t.Fatalf("%s %v exceeds width", page, size)
					}
				}
			}
		}
	}
}

func TestSchedulerCreateRetainsDraftAcrossValidationAndUnavailableErrors(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.queryFocus = true
	next, _ := m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	m = next.(model)
	if m.scheduled.page != "edit" || !m.scheduled.editCreating || m.scheduled.editEnabled || m.scheduled.field != schedulerFieldID {
		t.Fatal("create is not reachable from query focus with paused command defaults")
	}
	m.scheduled.fields[schedulerFieldID].SetValue("fixture-command")
	m.scheduled.fields[schedulerFieldName].SetValue("Fixture command")
	m.scheduled.fields[schedulerFieldCwd].SetValue("/fixture")
	m.scheduled.fields[schedulerFieldArgv].SetValue(`/usr/bin/printf "two words"`)
	next, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	if cmd != nil || m.scheduled.page != "edit" || m.scheduled.err == "" || m.scheduled.fields[schedulerFieldArgv].Value() != `/usr/bin/printf "two words"` {
		t.Fatal("shell string accepted or local validation discarded the draft")
	}
	wantArgv := []string{"/usr/bin/printf", "two words", "", " leading and trailing ", "literal;not shell"}
	raw, _ := json.Marshal(wantArgv)
	m.scheduled.fields[schedulerFieldArgv].SetValue(string(raw))
	m.scheduled.fields[schedulerFieldTimezone].SetValue("Invalid/Zone")
	var posted map[string]any
	refuse := true
	socket := schedulerTestSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/jobs" {
			t.Errorf("unexpected create endpoint: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Error(err)
		}
		if refuse {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid IANA time zone"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	t.Setenv("SCHEDULER_SOCKET", socket)
	next, cmd = m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.scheduled.page != "edit" || !m.scheduled.editCreating || m.scheduled.fields[schedulerFieldArgv].Value() != string(raw) || !strings.Contains(m.scheduled.err, "invalid IANA") {
		t.Fatal("server rejection discarded the create draft")
	}
	m.scheduled.fields[schedulerFieldTimezone].SetValue("UTC")
	t.Setenv("SCHEDULER_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	next, cmd = m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.scheduled.page != "edit" || m.scheduled.fields[schedulerFieldName].Value() != "Fixture command" || !strings.Contains(m.scheduled.err, "unavailable") {
		t.Fatal("unavailable scheduler discarded the create draft")
	}
	refuse = false
	t.Setenv("SCHEDULER_SOCKET", socket)
	next, cmd = m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	next, refresh := m.Update(cmd())
	m = next.(model)
	if refresh == nil || m.scheduled.page != "detail" || m.scheduled.jobID != "fixture-command" || m.scheduled.editCreating {
		t.Fatal("successful create did not select the new job and refresh")
	}
	want := map[string]any{
		"id": "fixture-command", "name": "Fixture command", "runner": "command", "schedule": "0 9 * * *",
		"timezone": "UTC", "cwd": "/fixture", "timeoutMinutes": float64(30), "enabled": false,
		"argv": []any{wantArgv[0], wantArgv[1], wantArgv[2], wantArgv[3], wantArgv[4]},
	}
	if !reflect.DeepEqual(posted, want) {
		t.Fatalf("create leaked inactive fields or changed argument boundaries: %#v", posted)
	}
}

func TestSchedulerCreateRejectsExistingIDAndCancelReturnsToList(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.startCreate()
	m.scheduled.fields[schedulerFieldID].SetValue("alpha")
	m.scheduled.fields[schedulerFieldName].SetValue("Must not replace")
	m.scheduled.fields[schedulerFieldArgv].SetValue(`["/usr/bin/true"]`)
	next, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = next.(model)
	if cmd != nil || !strings.Contains(m.scheduled.err, "already exists") {
		t.Fatal("create silently replaced an existing job")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)
	if m.scheduled.page != "list" || m.scheduled.jobID != "alpha" || m.mode != "scheduled" {
		t.Fatal("cancelling create did not return to the existing list selection")
	}
}

func TestSchedulerFullEditIsDirtyOnlyAndPreservesInactiveFields(t *testing.T) {
	m := scheduledTestModel(t)
	j, _ := m.scheduled.selectedJob()
	j.PermissionMode = "bypassPermissions"
	j.Argv = []string{"/old/executable", "keep this argument"}
	j.Model = "custom-existing-model"
	j.Prompt = "line one\nline two"
	m.scheduled.startEdit(j)
	m.scheduled.fields[schedulerFieldID].SetValue("cannot-change-id")
	m.scheduled.fields[schedulerFieldName].SetValue("Renamed")
	m.scheduled.fields[schedulerFieldCwd].SetValue("/other/project")
	patch, err := m.scheduled.editPatch()
	if err != nil || !reflect.DeepEqual(patch, map[string]any{"name": "Renamed", "cwd": "/other/project"}) {
		t.Fatalf("edit modified untouched prompt/model/argv/ID/permissions: %#v %v", patch, err)
	}
	m.scheduled.fields[schedulerFieldPrompt].SetValue("new prompt draft")
	m.scheduled.cycleRunner(-1) // omp -> command
	m.scheduled.fields[schedulerFieldArgv].SetValue(`["/usr/bin/printf","space preserved"]`)
	m.scheduled.fields[schedulerFieldModel].SetValue("inactive model draft")
	patch, err = m.scheduled.editPatch()
	want := map[string]any{"name": "Renamed", "cwd": "/other/project", "runner": "command", "argv": []string{"/usr/bin/printf", "space preserved"}}
	if err != nil || !reflect.DeepEqual(patch, want) {
		t.Fatalf("command cutover changed inactive agent configuration: %#v %v", patch, err)
	}
	m.scheduled.cycleRunner(1)
	if m.scheduled.fields[schedulerFieldPrompt].Value() != "new prompt draft" || m.scheduled.fields[schedulerFieldArgv].Value() != `["/usr/bin/printf","space preserved"]` ||
		m.scheduled.fields[schedulerFieldModel].Value() != j.Model {
		t.Fatal("runner switching lost a dynamic field draft")
	}
	patch, err = m.scheduled.editPatch()
	if err != nil || !reflect.DeepEqual(patch, map[string]any{"name": "Renamed", "cwd": "/other/project", "prompt": "new prompt draft"}) {
		t.Fatalf("switching back patched inactive argv or existing custom model: %#v %v", patch, err)
	}
	if slices.Contains(m.scheduled.editorFields(), schedulerFieldID) {
		t.Fatal("edit exposes mutable ID")
	}
}

func TestSchedulerArgvValidationAndInactiveDrafts(t *testing.T) {
	for _, raw := range []string{`echo hello`, `null`, `[]`, `[""]`, `["   "]`, `[7]`, `["true",null]`, `["true",false]`, `["true","\u0000"]`, `{"0":"true"}`} {
		if _, err := schedulerArgv(raw); err == nil {
			t.Errorf("malformed argv accepted: %s", raw)
		}
	}
	m := scheduledTestModel(t)
	j, _ := m.scheduled.selectedJob()
	m.scheduled.startEdit(j)
	m.scheduled.fields[schedulerFieldArgv].SetValue("inactive malformed JSON")
	m.scheduled.fields[schedulerFieldName].SetValue("Agent remains valid")
	if patch, err := m.scheduled.editPatch(); err != nil || len(patch) != 1 {
		t.Fatalf("inactive argv blocked an agent patch: %#v %v", patch, err)
	}
	m.scheduled.cycleRunner(-1)
	m.scheduled.fields[schedulerFieldArgv].SetValue(`["/usr/bin/true",""]`)
	m.scheduled.fields[schedulerFieldPrompt].SetValue("")
	if _, err := m.scheduled.editPatch(); err != nil {
		t.Fatalf("inactive empty prompt blocked a command patch: %v", err)
	}
}

func TestSchedulerRunnerAndModelSelectorsPreservePerRunnerDrafts(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.models = []string{"", "provider/model-a", "provider/model-b"}
	j, _ := m.scheduled.selectedJob()
	j.Model = "owner/custom-omp-id"
	m.scheduled.startEdit(j)
	if !m.scheduled.editModelCustom {
		t.Fatal("existing custom model ID was not preserved as custom")
	}
	m.scheduled.field = schedulerFieldRunner
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = next.(model)
	if m.scheduled.fields[schedulerFieldRunner].Value() != "claude-print" || m.scheduled.fields[schedulerFieldModel].Value() != "" ||
		schedulerRunnerLabel("claude-print") != "Claude" || schedulerRunnerLabel("claude-sdk") != "Claude SDK" {
		t.Fatal("runner selector did not choose Claude with target-runner default model")
	}
	patch, err := m.scheduled.editPatch()
	if err != nil || !reflect.DeepEqual(patch, map[string]any{"runner": "claude-print", "model": ""}) {
		t.Fatalf("OMP ID leaked into Claude or model default was not explicit: %#v %v", patch, err)
	}
	m.scheduled.field = schedulerFieldModel
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = next.(model)
	if m.scheduled.fields[schedulerFieldModel].Value() != "opus" {
		t.Fatal("Claude model selector did not select a supported alias")
	}
	m.scheduled.cycleRunner(1)
	if m.scheduled.fields[schedulerFieldModel].Value() != "" {
		t.Fatal("first Claude SDK switch did not use its own default draft")
	}
	m.scheduled.cycleRunner(-1)
	if m.scheduled.fields[schedulerFieldModel].Value() != "opus" {
		t.Fatal("returning to Claude lost the selected model draft")
	}
	m.scheduled.cycleRunner(-1)
	if m.scheduled.fields[schedulerFieldModel].Value() != "owner/custom-omp-id" || !m.scheduled.editModelCustom {
		t.Fatal("returning to OMP lost its existing custom model")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	m = next.(model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = next.(model)
	if m.scheduled.fields[schedulerFieldModel].Value() != "provider/model-a" || m.scheduled.editModelCustom {
		t.Fatal("OMP model selector did not use the real catalog choices")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: 'c'})
	m = next.(model)
	if !m.scheduled.editModelCustom || !m.scheduled.fields[schedulerFieldModel].Focused() || m.scheduled.fields[schedulerFieldModel].Value() != "owner/custom-omp-id" {
		t.Fatal("custom model action did not focus/preserve the exact custom draft")
	}
	m.scheduled.cycleRunner(-1)
	if slices.Contains(m.scheduled.editorFields(), schedulerFieldModel) {
		t.Fatal("command runner exposes a model control")
	}
}

func TestSchedulerModelCatalogLoadsOnceAndKeepsDrafts(t *testing.T) {
	m := scheduledTestModel(t)
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = models ] && [ \"$2\" = --json ] || exit 1\nprintf '%s\\n' '{\"models\":[{\"selector\":\"provider/catalog-id\"},{\"selector\":\"provider/catalog-id\"}]}'\n"
	if err := os.WriteFile(filepath.Join(dir, "omp"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	j, _ := m.scheduled.selectedJob()
	j.Model = "provider/catalog-id"
	m.scheduled.startEdit(j)
	m.scheduled.fields[schedulerFieldPrompt].SetValue("draft remains intact")
	cmd := m.scheduled.loadModels()
	if cmd == nil || m.scheduled.loadModels() != nil {
		t.Fatal("catalog requests were not single-flight")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	if !m.scheduled.modelsLoaded || m.scheduled.modelLoading || m.scheduled.loadModels() != nil || m.scheduled.editModelCustom ||
		!reflect.DeepEqual(m.scheduled.models, []string{"", "provider/catalog-id"}) ||
		m.scheduled.fields[schedulerFieldPrompt].Value() != "draft remains intact" {
		t.Fatal("catalog was not cached/deduplicated or overwrote the editor draft")
	}
	m.scheduled.modelsLoaded = false
	t.Setenv("PATH", t.TempDir())
	next, _ = m.Update(m.scheduled.loadModels()())
	m = next.(model)
	if m.scheduled.modelErr == "" || !reflect.DeepEqual(m.scheduled.modelOptions("omp"), []string{""}) {
		t.Fatal("unavailable OMP catalog did not preserve default/custom choices")
	}
}

func TestSchedulerDeleteRequiresNamedConfirmationAndRetainsErrors(t *testing.T) {
	m := scheduledTestModel(t)
	m.scheduled.queryFocus = false
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'd'})
	m = next.(model)
	if cmd != nil || m.scheduled.page != "delete" {
		t.Fatal("delete bypassed confirmation or did not open it")
	}
	view := schedulerText(m.View().Content)
	if !strings.Contains(view, "Alpha") || !strings.Contains(view, "history") || !strings.Contains(view, "Irreversible") {
		t.Fatal("delete confirmation omits job name/history warning")
	}
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)
	if cmd != nil || m.scheduled.page != "list" || m.scheduled.jobID != "alpha" {
		t.Fatal("delete cancellation changed the selected job")
	}
	refusal := "job is running"
	deleted := false
	socket := schedulerTestSocket(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs/delete":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost || !reflect.DeepEqual(body, map[string]string{"id": "alpha"}) {
				t.Errorf("incorrect delete contract: %s %#v", r.Method, body)
			}
			if refusal != "" {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": refusal})
				return
			}
			deleted = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/snapshot":
			if !deleted {
				t.Error("snapshot fetched before accepted delete")
			}
			_, _ = w.Write([]byte(`{"jobs":[],"runs":[]}`))
		}
	})
	t.Setenv("SCHEDULER_SOCKET", socket)
	j, _ := m.scheduled.selectedJob()
	m.scheduled.startDelete(j)
	for _, reason := range []string{"job is running", "job has an open interactive continuation"} {
		refusal = reason
		next, cmd = m.Update(tea.KeyPressMsg{Code: 'y'})
		m = next.(model)
		next, refresh := m.Update(cmd())
		m = next.(model)
		if refresh != nil || m.scheduled.page != "delete" || m.scheduled.deleteName != "Alpha" || !strings.Contains(m.scheduled.err, reason) {
			t.Fatal("refused deletion lost confirmation/error context")
		}
	}
	refusal = ""
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)
	next, refresh := m.Update(cmd())
	m = next.(model)
	if refresh == nil || m.scheduled.page != "list" {
		t.Fatal("accepted delete did not return to the jobs list")
	}
	next, _ = m.Update(refresh())
	m = next.(model)
	if len(m.scheduled.snapshot.Jobs) != 0 || m.scheduled.jobID != "" || m.scheduled.runID != "" {
		t.Fatal("deleted job/history survived authoritative refresh")
	}
}

func TestSchedulerCommandMonitorUsesLogsAndDisplaysOutcomeWithoutSecrets(t *testing.T) {
	m := scheduledTestModel(t)
	exit := 0
	r := scheduledRun{ID: "command-run", JobID: "alpha", Runner: "command", Status: "success", Outcome: "skipped", ExitCode: &exit, Reason: "fixture skip"}
	m.scheduled.snapshot.Runs = []scheduledRun{r}
	m.scheduled.snapshot.Jobs[0].Runner = "command"
	m.scheduled.snapshot.Jobs[0].Argv = []string{"/private/command", "secret-argv-token"}
	m.scheduled.snapshot.Jobs[0].Prompt = "secret-prompt-token"
	m.scheduled.snapshot.Jobs[0].LastRun = &r
	m.scheduled.page, m.scheduled.runID = "detail", r.ID
	socket := schedulerTestSocket(t, func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != scheduledRunPath(r.ID) || request.Method != http.MethodGet {
			t.Errorf("unexpected monitor endpoint: %s %s", request.Method, request.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(schedulerRunDetail{Run: r, Output: "fixture output"})
	})
	t.Setenv("SCHEDULER_SOCKET", socket)
	for _, page := range []string{"list", "detail"} {
		m.scheduled.page = page
		m.h = 48
		view := schedulerText(m.View().Content)
		if strings.Contains(view, "secret-argv-token") || strings.Contains(view, "secret-prompt-token") || strings.Contains(view, "/private/command") {
			t.Fatal("list/preview displayed command or prompt credentials")
		}
		if !strings.Contains(view, "skipped") {
			t.Fatal("last result omitted watcher outcome")
		}
	}
	m.scheduled.query.SetValue("secret-prompt-token")
	if len(m.scheduled.jobs()) != 0 {
		t.Fatal("prompt secrets are searchable from the normal job list")
	}
	m.scheduled.query.SetValue("")
	m.scheduled.page = "detail"
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'm'})
	m = next.(model)
	if cmd == nil || m.scheduled.page != "log" || m.action.Verb != "" {
		t.Fatal("completed command monitor launched an LLM continuation")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	lines := strings.Join(m.scheduled.logLines(100), "\n")
	if !strings.Contains(lines, "success · skipped") || !strings.Contains(lines, "exit: 0") || !strings.Contains(lines, "fixture skip") {
		t.Fatal("command run lost outcome/exit/reason diagnostics")
	}
	if err := joinScheduledRun(r.ID); err == nil || !strings.Contains(err.Error(), "no agent session") {
		t.Fatal("backend join guard allowed a completed command agent continuation")
	}
}

func TestSchedulerEditorKeyboardFocusFitsMinimumTerminal(t *testing.T) {
	m := scheduledTestModel(t)
	m.w, m.h = 28, 9
	m.scheduled.startCreate()
	for _, runner := range schedulerRunners {
		for m.scheduled.fields[schedulerFieldRunner].Value() != runner {
			m.scheduled.cycleRunner(1)
		}
		visible := m.scheduled.editorFields()
		m.scheduled.field = visible[0]
		for _, field := range visible {
			if m.scheduled.field != field {
				t.Fatalf("keyboard focus skipped field %d for %s", field, runner)
			}
			for _, line := range strings.Split(m.View().Content, "\n") {
				if lipgloss.Width(line) > m.w {
					t.Fatal("focused editor field overflows narrow terminal")
				}
			}
			next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			m = next.(model)
		}
		if m.scheduled.field != visible[0] {
			t.Fatal("editor tab traversal did not wrap")
		}
	}
}

func TestSchedulerLongUnfocusedArgumentsCannotHideValidationDraft(t *testing.T) {
	for _, size := range [][2]int{{28, 9}, {42, 17}, {110, 33}} {
		m := scheduledTestModel(t)
		m.w, m.h = size[0], size[1]
		j := m.scheduled.snapshot.Jobs[0]
		j.Runner = "command"
		j.Argv = []string{"/usr/bin/printf", strings.Repeat("argument with spaces ", 100)}
		m.scheduled.startEdit(j)
		for range 2 {
			next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			m = next.(model)
		}
		m.scheduled.fields[schedulerFieldSchedule].SetValue("bad cron")
		m.scheduled.err = "schedule must be a five-field cron expression"
		view := m.View().Content
		if !strings.Contains(schedulerText(view), "bad cron") {
			t.Fatalf("%dx%d hides the focused invalid draft behind the home fallback", m.w, m.h)
		}
		if lipgloss.Height(view) > m.h {
			t.Fatalf("%dx%d editor exceeds available height", m.w, m.h)
		}
		for _, row := range strings.Split(view, "\n") {
			if lipgloss.Width(row) > m.w {
				t.Fatalf("%dx%d editor exceeds available width", m.w, m.h)
			}
		}
	}
}

func TestSchedulerAgentCreateIncludesOnlyActiveRunnerConfiguration(t *testing.T) {
	for _, runner := range []string{"omp", "claude-print", "claude-sdk"} {
		t.Run(runner, func(t *testing.T) {
			m := scheduledTestModel(t)
			m.scheduled.startCreate()
			m.scheduled.fields[schedulerFieldID].SetValue("fixture-agent")
			m.scheduled.fields[schedulerFieldName].SetValue("Fixture agent")
			for m.scheduled.fields[schedulerFieldRunner].Value() != runner {
				m.scheduled.cycleRunner(1)
			}
			m.scheduled.fields[schedulerFieldPrompt].SetValue("A fixture-only prompt; never invoked.")
			m.scheduled.fields[schedulerFieldModel].SetValue("custom/exact-model")
			m.scheduled.fields[schedulerFieldArgv].SetValue("inactive invalid command JSON")
			config, err := m.scheduled.editorConfig()
			if err != nil {
				t.Fatal(err)
			}
			if config["runner"] != runner || config["model"] != "custom/exact-model" || config["enabled"] != false ||
				config["prompt"] != "A fixture-only prompt; never invoked." {
				t.Fatalf("agent create lost active fields/default pause: %#v", config)
			}
			for _, field := range []string{"argv", "permissionMode", "allowedTools", "additionalDirectories"} {
				if _, included := config[field]; included {
					t.Fatalf("agent create authorizes an inactive/uneditable field: %s", field)
				}
			}
		})
	}
}

func TestSchedulerWrappedClaudeMonitorAndResume(t *testing.T) {
	for _, status := range []string{"running", "success"} {
		t.Run(status, func(t *testing.T) {
			m := scheduledTestModel(t)
			r := scheduledRun{ID: "wrapped-run", JobID: "alpha", Runner: "command", Status: status, Cwd: "/state", WrappedAgent: "claude"}
			if status != "running" {
				r.Continuation = &scheduledContinuation{Kind: "claude", SessionID: "12345678-1234-4234-8234-123456789012", Cwd: r.Cwd}
			}
			m.scheduled.snapshot.Runs = []scheduledRun{r}
			m.scheduled.snapshot.Jobs[0].LastRun = &r
			m.scheduled.page, m.scheduled.runID = "detail", r.ID
			next, cmd := m.Update(tea.KeyPressMsg{Code: 'm'})
			got := next.(model)
			if cmd == nil || got.action.Verb != "scheduled-join" || got.action.Name != r.ID || got.scheduled.page == "log" {
				t.Fatal("wrapped agent did not use scheduler join")
			}
			want := "resume exact session"
			mode := "scheduled-resume-"
			if status == "running" {
				want, mode = "monitor (read-only)", "scheduled-monitor-"
			}
			if !strings.Contains(scheduledRunAction(r), want) || !strings.HasPrefix(schedulerJoinName(r), mode) {
				t.Fatal("wrapped run mode mislabeled or reused the other mode's session")
			}
		})
	}
}

func TestSchedulerUnavailableAndInvalidContinuationNeverLaunches(t *testing.T) {
	for _, runner := range []string{"command", "claude-print"} {
		m := scheduledTestModel(t)
		r := scheduledRun{ID: "unavailable", JobID: "alpha", Runner: runner, Status: "success", Cwd: "/state", SessionID: "saved", ContinuationBlocked: "Saved session evidence unavailable"}
		m.scheduled.snapshot.Runs = []scheduledRun{r}
		m.scheduled.snapshot.Jobs[0].LastRun = &r
		m.scheduled.page, m.scheduled.runID = "detail", r.ID
		next, cmd := m.Update(tea.KeyPressMsg{Code: 'm'})
		got := next.(model)
		if cmd != nil || got.action.Verb != "" || got.scheduled.err == "" {
			t.Fatal("unavailable session remained resumable")
		}
		socket := schedulerTestSocket(t, func(w http.ResponseWriter, request *http.Request) {
			_ = json.NewEncoder(w).Encode(schedulerRunDetail{Run: r})
		})
		t.Setenv("SCHEDULER_SOCKET", socket)
		if err := joinScheduledRun(r.ID); err == nil {
			t.Fatal("backend ignored unavailable session evidence")
		}
	}
	r := scheduledRun{Runner: "command", Cwd: "/state", Continuation: &scheduledContinuation{Kind: "claude", SessionID: "saved", Cwd: "/other"}}
	if r.hasWrappedSession() {
		t.Fatal("mismatched working directory accepted")
	}
	r.Continuation.Cwd, r.Continuation.Kind = r.Cwd, "arbitrary-command"
	if r.hasWrappedSession() {
		t.Fatal("arbitrary executable continuation accepted")
	}
}
