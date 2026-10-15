package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const testT3ID = "06ef22d7-5ee1-415d-bb4b-66155b88a162"
const testCodexID = "01a11313-0410-77f2-ac7a-e9f078af426c"
const testClaudeID = "ad3c9ae1-0a99-4a1a-ab93-ccb587fbbe59"

func t3Fixture(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is required for T3 integration tests")
	}
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(t3DBPath()), 0700); err != nil {
		t.Fatal(err)
	}
	t3SQL(t, `CREATE TABLE projection_projects (project_id TEXT, title TEXT, workspace_root TEXT, deleted_at TEXT);
 CREATE TABLE projection_threads (thread_id TEXT, project_id TEXT, title TEXT, worktree_path TEXT,
 created_at TEXT, updated_at TEXT, archived_at TEXT, deleted_at TEXT, pending_approval_count INTEGER DEFAULT 0, pending_user_input_count INTEGER DEFAULT 0);
 CREATE TABLE projection_thread_sessions (thread_id TEXT, status TEXT, provider_name TEXT, provider_instance_id TEXT, active_turn_id TEXT, last_error TEXT);
 CREATE TABLE provider_session_runtime (thread_id TEXT, provider_name TEXT, provider_instance_id TEXT, status TEXT, resume_cursor_json TEXT);
 CREATE TABLE projection_thread_messages (message_id TEXT, thread_id TEXT, role TEXT, text TEXT, created_at TEXT, is_streaming INTEGER);
 INSERT INTO projection_projects VALUES ('p', 'Sample Project', '/workspace/project', NULL);
 INSERT INTO projection_threads (thread_id,project_id,title,created_at,updated_at) VALUES
 ('`+testT3ID+`','p','Unclear Test Request','2026-10-06T10:00:00Z','2026-10-06T11:00:00Z'),
 ('11111111-1111-4111-8111-111111111111','p','Claude conversation','2026-10-06T10:00:00Z','2026-10-06T11:00:00Z'),
 ('22222222-2222-4222-8222-222222222222','p','Unstarted chat','2026-10-06T10:00:00Z','2026-10-06T11:00:00Z');
 INSERT INTO projection_thread_sessions VALUES ('`+testT3ID+`','ready','codex','codex',NULL,NULL);
 INSERT INTO provider_session_runtime VALUES ('`+testT3ID+`','codex','codex','ready','{"threadId":"`+testCodexID+`"}');
 INSERT INTO projection_thread_sessions VALUES ('11111111-1111-4111-8111-111111111111','ready','claudeAgent','claudeAgent',NULL,NULL);
 INSERT INTO provider_session_runtime VALUES ('11111111-1111-4111-8111-111111111111','claudeAgent','claudeAgent','ready','{"threadId":"claude-thread-synthetic","resume":"`+testClaudeID+`"}');
 INSERT INTO projection_thread_messages VALUES ('m1','`+testT3ID+`','user','Hello','2026-10-06T10:00:00Z',0),
 ('m2','`+testT3ID+`','assistant','Working on it','2026-10-06T10:01:00Z',0);`)
}

func t3SQL(t *testing.T, sql string) {
	t.Helper()
	if output, err := exec.Command("sqlite3", t3DBPath(), sql).CombinedOutput(); err != nil {
		t.Fatalf("fixture SQL: %v: %s", err, output)
	}
}

func TestT3DiscoverySearchAndConversationView(t *testing.T) {
	t3Fixture(t)
	t3SQL(t, `UPDATE projection_threads SET worktree_path='/workspace/worktree' WHERE thread_id='`+testT3ID+`';
 UPDATE projection_threads SET archived_at='2026-10-06' WHERE title='Claude conversation';
 INSERT INTO projection_threads (thread_id,project_id,title,created_at,updated_at,deleted_at)
 VALUES ('deleted','p','Deleted chat','','','2026-10-06');`)
	sessions, err := collectT3(context.Background())
	if err != nil || len(sessions) != 3 {
		t.Fatalf("discovery: %v %+v", err, sessions)
	}
	s := sessions[0]
	if s.T3.Conversation != testCodexID || s.T3.ID != testT3ID || s.T3.Cwd != "/workspace/worktree" || s.State != "READY" {
		t.Fatalf("Codex mapping: %+v", s.T3)
	}
	if sessions[1].Kind != "claude" || !sessions[1].Saved || sessions[1].T3.Conversation != testClaudeID {
		t.Fatalf("Claude mapping: %+v", sessions[1])
	}
	if sessions[2].T3.resumeBlocked() == "" {
		t.Fatal("unstarted chat should remain inspectable without resuming")
	}
	// Exercise persisted discovery rather than injecting rows directly into search.
	if err := saveCache(Snapshot{Sessions: sessions, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m := initialModel(true, "")
	m.w, m.h = 100, 30
	m.input.SetValue("t3 sample unclear " + testCodexID)
	if rows := m.listed(); len(rows) != 1 || rows[0].T3.ID != testT3ID {
		t.Fatalf("T3 metadata search: %+v", rows)
	}
	if totals := m.aggregateResources(sessions); totals.Live != 0 || totals.RSSKnown != 0 {
		t.Fatalf("T3 chats counted as independent processes: %+v", totals)
	}
	m.searchFocus = searchResults
	result, cmd := m.activate()
	m = result.(model)
	if m.mode != "t3" || cmd == nil || m.action.Verb != "" {
		t.Fatalf("selection did not open inspector: %+v", m.action)
	}
	result, _ = m.Update(cmd())
	m = result.(model)
	if m.t3.err != "" || len(m.t3.messages) != 2 {
		t.Fatalf("transcript: %s %+v", m.t3.err, m.t3.messages)
	}
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "Working on it") || !strings.Contains(plain, testCodexID) {
		t.Fatalf("conversation absent: %s", plain)
	}
	result, _ = m.updateT3Key("esc")
	m = result.(model)
	if m.mode != "search" || !strings.Contains(m.input.Value(), "unclear") || m.searchFocus != searchResults {
		t.Fatal("inspector did not preserve search state")
	}
	// Number shortcuts take the same path as Enter and mouse selection.
	m.mode, m.finder = "home", false
	result, _ = m.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	if result.(model).mode != "t3" || result.(model).action.Verb != "" {
		t.Fatal("number shortcut tried attaching to a T3 UI ID")
	}
}

func TestT3ResumeUsesExactProviderIdentityAndRechecksState(t *testing.T) {
	t3Fixture(t)
	cwd := t.TempDir()
	t3SQL(t, `UPDATE projection_projects SET workspace_root='`+cwd+`';`)
	// Real T3 databases can retain running in the runtime row after settling.
	t3SQL(t, `UPDATE provider_session_runtime SET status='running' WHERE thread_id='`+testT3ID+`';`)
	bin := t.TempDir()
	logPath := filepath.Join(bin, "calls")
	t.Setenv("T3_TEST_LOG", logPath)
	script := "#!/bin/sh\nif [ \"$1\" = list-sessions ]; then exit 0; fi\nprintf '%s\\n' \"$PWD\" \"$@\" >> \"$T3_TEST_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "zellij"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("ZELLIJ", "")
	if err := resumeT3(testT3ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), cwd+"\nattach\n--create\nt3-resume-"+testT3ID+"\n--\ncodex\nresume\n"+testCodexID+"\n") {
		t.Fatalf("wrong Codex continuation: %v %s", err, data)
	}
	// Both the finder and direct action re-read the DB before opening a writer.
	t3SQL(t, `UPDATE projection_thread_sessions SET status='running',active_turn_id='active' WHERE thread_id='`+testT3ID+`';`)
	if err := runAction(Action{Verb: "t3-resume", Name: testT3ID}); err == nil {
		t.Fatal("cached idle status allowed resuming a now-running chat")
	}
	after, _ := os.ReadFile(logPath)
	if string(data) != string(after) {
		t.Fatal("busy chat spawned a terminal")
	}
	t.Setenv("ZELLIJ", "inside")
	if err := resumeT3("11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(logPath)
	if !strings.Contains(string(data), "--create-background\n") || !strings.Contains(string(data), "claude\n--resume\n"+testClaudeID) || !strings.Contains(string(data), "action\nswitch-session\n") {
		t.Fatalf("Claude finder continuation: %s", data)
	}
}

func TestT3ResumeBlockingAndProviderSettings(t *testing.T) {
	t3Fixture(t)
	s, err := findT3(context.Background(), testT3ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*t3Thread){
		func(t *t3Thread) { t.Status = "running" },
		func(t *t3Thread) { t.Status = "connecting" },
		func(t *t3Thread) { t.PendingApprovals = 1 },
		func(t *t3Thread) { t.PendingInputs = 1 },
		func(t *t3Thread) { t.ActiveTurn = "turn" },
		func(t *t3Thread) { t.Streaming = true },
		func(t *t3Thread) { t.Conversation = "--last" },
		func(t *t3Thread) { t.Provider = "unknown" },
	} {
		thread := *s.T3
		change(&thread)
		if _, _, _, err := t3ResumeCommand(thread); err == nil {
			t.Fatalf("unsafe or ambiguous resume allowed: %+v", thread)
		}
	}
	config := `{"providerInstances":{"codex":{"config":{"binaryPath":"~/bin/custom-codex","homePath":"~/.provider-home"}}}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(t3DBPath()), "settings.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	binary, args, env, err := t3ResumeCommand(*s.T3)
	if err != nil || binary != filepath.Join(os.Getenv("HOME"), "bin/custom-codex") || len(args) != 2 || !strings.Contains(strings.Join(env, "\n"), "CODEX_HOME="+filepath.Join(os.Getenv("HOME"), ".provider-home")) {
		t.Fatalf("local provider config: %v %s %v", err, binary, args)
	}
	if _, err := findT3(context.Background(), "x' OR 1=1 --"); err == nil {
		t.Fatal("untrusted ID reached the database")
	}
}

func TestT3InspectorFitsAndSanitizesStreamingMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := t3Session(t3Record{ID: testT3ID, Title: "Inspector", Project: "Sample", Provider: "codex", Status: "running", Cwd: "/workspace/" + strings.Repeat("nested/", 60)})
	for _, size := range [][2]int{{100, 44}, {80, 28}, {42, 16}, {40, 12}, {28, 9}} {
		m := initialModel(false, "")
		m.w, m.h, m.mode = size[0], size[1], "t3"
		m.t3 = t3UI{session: s, returnMode: "home", messages: []t3Message{{Role: "assistant", Text: "Hello\x1b[31m red\x1b[0m\x07\n" + strings.Repeat("streamed content ", 150), Streaming: 1}}}
		content := m.View().Content
		plain := ansi.Strip(content)
		if lipgloss.Width(content) > m.w || lipgloss.Height(content) > m.h || !strings.Contains(plain, "Inspector") || (m.h >= 16 && !strings.Contains(plain, "Hello red")) || strings.ContainsRune(content, '\x07') {
			t.Fatalf("%dx%d inspector overflows or unsafe: %q", m.w, m.h, plain)
		}
		result, _ := m.updateT3Key("r")
		if result.(model).action.Verb != "" {
			t.Fatal("running inspector allowed resume")
		}
	}
}

func TestT3DatabaseFailureRetainsCachedChats(t *testing.T) {
	t3Fixture(t)
	sessions, err := collectT3(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := saveCache(Snapshot{Sessions: sessions, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Simulate an incompatible/corrupt T3 database and an unavailable Zellij.
	if err := os.WriteFile(t3DBPath(), []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "zellij"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	snapshot := collect(context.Background())
	if snapshot.Err != "" || len(snapshot.Sessions) != 3 || !strings.Contains(snapshot.Warning, "T3 unavailable") {
		t.Fatalf("failure erased cached chats: %+v", snapshot)
	}
	// Ensure all the additive metadata survives the persistent cache.
	data, _ := json.Marshal(snapshot)
	var restored Snapshot
	if json.Unmarshal(data, &restored) != nil || restored.Sessions[0].T3 == nil {
		t.Fatal("T3 metadata did not roundtrip")
	}
}

func TestT3VisibilityShortcutPersistsAndKeepsSearch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(stateRoot(), 0700); err != nil {
		t.Fatal(err)
	}
	// Existing preference files have no T3 key; keep chats visible on migration.
	if err := os.WriteFile(prefsPath(), []byte("agents=1\nshells=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := initialModel(false, "")
	m.w, m.h = 80, 28
	m.snapshot.Sessions = []Session{
		{Name: "terminal", Title: "Project", Kind: "omp"},
		{Name: "t3-chat", Title: "Project", Kind: "codex", T3: &t3Thread{}},
		{Name: "t3-archived", Title: "Project", Kind: "codex", T3: &t3Thread{}, Saved: true},
	}
	if len(m.listed()) != 2 || !strings.Contains(ansi.Strip(m.filterBar(74)), "T3 ^Y") {
		t.Fatal("T3 visibility default or shortcut label missing")
	}
	key := tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}
	next, _ := m.Update(key)
	m = next.(model)
	if len(m.listed()) != 1 || readPrefs().T3 || !readPrefs().Agents {
		t.Fatal("home shortcut did not hide only T3 chats and persist")
	}
	m.mode, m.searchFocus = "search", searchQuery
	m.input.SetValue("Project")
	m.input.Focus()
	next, _ = m.Update(key)
	m = next.(model)
	if len(m.listed()) != 2 || !readPrefs().T3 || m.input.Value() != "Project" || !m.input.Focused() {
		t.Fatal("search shortcut lost query/focus or did not restore chats")
	}
	m.toggleFilter("ctrl+g")
	if rows := m.listed(); len(rows) != 1 || rows[0].T3 == nil {
		t.Fatal("T3 visibility should be independent of the terminal Agents filter")
	}
	m.toggleFilter("ctrl+v")
	if len(m.listed()) != 2 {
		t.Fatal("Saved filter did not include archived T3 chats")
	}
	m.searchFocus = searchT3
	next, _ = m.updateSearch(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)
	if len(m.listed()) != 0 || readPrefs().T3 {
		t.Fatal("inline T3 control did not toggle")
	}
	m.mode, m.cursor = "settings", 5
	next, _ = m.updateSettings("space")
	if !next.(model).prefs.T3 || !readPrefs().T3 {
		t.Fatal("T3 preference card did not toggle")
	}
}
