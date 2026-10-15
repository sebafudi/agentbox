package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// T3's UI identity is distinct from the provider's resumable conversation ID.
type t3Thread struct {
	ID, Project, Cwd, Provider, Instance, Status, RuntimeStatus string
	Conversation, ResumeAt, ActiveTurn, LastError               string
	PendingApprovals, PendingInputs                             int
	Streaming                                                   bool
}

type t3Record struct {
	ID, Title, Project, Cwd, Provider, Instance, Status, RuntimeStatus string
	Cursor, ActiveTurn, LastError, Created, Updated, Archived          string
	PendingApprovals, PendingInputs, Streaming                         int
}

const t3ThreadQuery = `SELECT t.thread_id AS ID, t.title AS Title,
 p.title AS Project, COALESCE(NULLIF(t.worktree_path, ''), p.workspace_root) AS Cwd,
 COALESCE(r.provider_name, s.provider_name, '') AS Provider,
 COALESCE(r.provider_instance_id, s.provider_instance_id, '') AS Instance,
 COALESCE(s.status, r.status, '') AS Status, COALESCE(r.status, '') AS RuntimeStatus,
 COALESCE(r.resume_cursor_json, '') AS Cursor,
 COALESCE(s.active_turn_id, '') AS ActiveTurn, COALESCE(s.last_error, '') AS LastError,
 t.created_at AS Created, t.updated_at AS Updated,
 COALESCE(t.archived_at, '') AS Archived,
 t.pending_approval_count AS PendingApprovals, t.pending_user_input_count AS PendingInputs,
 EXISTS(SELECT 1 FROM projection_thread_messages m WHERE m.thread_id=t.thread_id AND m.is_streaming=1) AS Streaming
 FROM projection_threads t
 JOIN projection_projects p ON p.project_id=t.project_id
 LEFT JOIN projection_thread_sessions s ON s.thread_id=t.thread_id
 LEFT JOIN provider_session_runtime r ON r.thread_id=t.thread_id
 WHERE t.deleted_at IS NULL AND p.deleted_at IS NULL`

func t3DBPath() string { return filepath.Join(os.Getenv("HOME"), ".t3/userdata/state.sqlite") }

func t3Query(ctx context.Context, query string, result any) error {
	data, err := command(ctx, 2*time.Second, "sqlite3", "-readonly", "-json", "-cmd", ".timeout 500", t3DBPath(), query)
	if err != nil {
		return fmt.Errorf("read chats: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte("[]")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("invalid chat data: %w", err)
	}
	return nil
}

func collectT3(ctx context.Context) ([]Session, error) {
	if _, err := os.Stat(t3DBPath()); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var records []t3Record
	if err := t3Query(ctx, t3ThreadQuery, &records); err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(records))
	for _, r := range records {
		s := t3Session(r)
		observe(&s)
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func t3Session(r t3Record) Session {
	t := &t3Thread{ID: r.ID, Project: clean(r.Project), Cwd: r.Cwd,
		Provider: r.Provider, Instance: r.Instance, Status: r.Status, RuntimeStatus: r.RuntimeStatus,
		ActiveTurn: r.ActiveTurn, LastError: clean(r.LastError),
		PendingApprovals: r.PendingApprovals, PendingInputs: r.PendingInputs, Streaming: r.Streaming != 0}
	var cursor struct{ ThreadID, Resume, SessionID, ResumeSessionAt string }
	if json.Unmarshal([]byte(r.Cursor), &cursor) == nil {
		switch r.Provider {
		case "codex":
			t.Conversation = cursor.ThreadID
		case "claudeAgent", "claude":
			// Claude's threadId may be synthetic; resume is the durable CLI ID.
			t.Conversation = cursor.Resume
			if t.Conversation == "" {
				t.Conversation = cursor.SessionID
			}
			t.ResumeAt = cursor.ResumeSessionAt
		}
	}
	kind := r.Provider
	if kind == "claudeAgent" {
		kind = "claude"
	}
	if kind == "" {
		kind = "agent"
	}
	created, _ := time.Parse(time.RFC3339Nano, r.Created)
	updated, _ := time.Parse(time.RFC3339Nano, r.Updated)
	s := Session{Name: "t3-" + r.ID, Kind: kind, Title: clean(r.Title), T3: t,
		Created: created, Used: updated, Saved: r.Archived != "", Notice: "-", State: strings.ToUpper(r.Status)}
	if s.State == "RUNNING" {
		s.State = "RUN"
	}
	if s.State == "" {
		s.State = "NEW"
	}
	if t.PendingApprovals > 0 || t.PendingInputs > 0 {
		s.State = "ASK"
	}
	if st, err := os.Stat(marker(s.Name)); err == nil {
		s.Used = st.ModTime()
		s.FromUse = true
	}
	s.Context = strings.Join([]string{"T3", t.Project, t.Cwd, t.Provider, t.Status, t.ID, t.Conversation}, " ")
	return s
}

var t3UUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func findT3(ctx context.Context, id string) (Session, error) {
	if !t3UUID.MatchString(id) {
		return Session{}, fmt.Errorf("invalid T3 thread ID")
	}
	var records []t3Record
	if err := t3Query(ctx, t3ThreadQuery+" AND t.thread_id='"+id+"'", &records); err != nil {
		return Session{}, err
	}
	if len(records) != 1 {
		return Session{}, fmt.Errorf("T3 chat no longer exists")
	}
	return t3Session(records[0]), nil
}

func (t t3Thread) resumeBlocked() string {
	if t.Provider != "codex" && t.Provider != "claudeAgent" && t.Provider != "claude" {
		return "Terminal resume is available for Codex and Claude Code."
	}
	// The runtime row stores the resume cursor but may retain "running" after
	// a completed turn. The projected session status is the current chat state
	// (and t3ThreadQuery falls back to runtime status if no projection exists).
	if t.Status != "" && t.Status != "ready" && t.Status != "closed" && t.Status != "error" {
		return "This chat is active in T3; inspect it here or finish its turn in T3 before resuming."
	}
	if t.ActiveTurn != "" || t.Streaming || t.PendingApprovals > 0 || t.PendingInputs > 0 {
		return "This chat has an active turn or pending input; finish it in T3 before resuming."
	}
	if t.Status == "" || !t3UUID.MatchString(t.Conversation) {
		return "This chat has no resumable provider conversation yet."
	}
	if t.ResumeAt != "" && !t3UUID.MatchString(t.ResumeAt) {
		return "Claude's saved resume point is invalid."
	}
	if !filepath.IsAbs(t.Cwd) || strings.ContainsRune(t.Cwd, 0) {
		return "This chat has no valid working directory."
	}
	return ""
}

type t3Message struct {
	Role, Text, Created string
	Streaming           int
}

func loadT3Messages(ctx context.Context, id string) ([]t3Message, error) {
	if !t3UUID.MatchString(id) {
		return nil, fmt.Errorf("invalid T3 thread ID")
	}
	var messages []t3Message
	// Bound refreshes even for very long chats; retain the latest 200 messages.
	query := `SELECT role AS Role, substr(text, 1, 65536) AS Text, created_at AS Created, is_streaming AS Streaming
 FROM (SELECT * FROM projection_thread_messages WHERE thread_id='` + id + `'
 ORDER BY created_at DESC, message_id DESC LIMIT 200) ORDER BY created_at, message_id`
	err := t3Query(ctx, query, &messages)
	return messages, err
}

func t3ResumeCommand(t t3Thread) (string, []string, []string, error) {
	if reason := t.resumeBlocked(); reason != "" {
		return "", nil, nil, fmt.Errorf("%s", reason)
	}
	binary := "codex"
	args := []string{"resume", t.Conversation}
	homeVar := "CODEX_HOME"
	if t.Provider != "codex" {
		binary, homeVar = "claude", "CLAUDE_CONFIG_DIR"
		args = []string{"--resume", t.Conversation}
		if t.ResumeAt != "" {
			args = append(args, "--resume-session-at", t.ResumeAt)
		}
	}
	// Respect local provider instances' binary/home without copying launch flags
	// or granting the CLI T3's permission bypass settings.
	var settings struct {
		Providers         map[string]json.RawMessage
		ProviderInstances map[string]struct {
			Config json.RawMessage
		}
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(t3DBPath()), "settings.json"))
	if err != nil && !os.IsNotExist(err) {
		return "", nil, nil, err
	}
	if len(data) > 0 && json.Unmarshal(data, &settings) != nil {
		return "", nil, nil, fmt.Errorf("invalid T3 provider settings")
	}
	raw := settings.Providers[t.Provider]
	if instance, ok := settings.ProviderInstances[t.Instance]; ok {
		raw = instance.Config
	}
	var config struct{ BinaryPath, HomePath, ShadowHomePath, SetupMode string }
	if len(raw) > 0 && json.Unmarshal(raw, &config) != nil {
		return "", nil, nil, fmt.Errorf("invalid T3 provider configuration")
	}
	if config.SetupMode == "managed" {
		return "", nil, nil, fmt.Errorf("managed T3 provider: resume this chat in T3")
	}
	if config.BinaryPath != "" {
		binary = config.BinaryPath
	}
	home := config.HomePath
	if config.ShadowHomePath != "" {
		home = config.ShadowHomePath
	}
	env := cleanEnv()
	if home != "" {
		home = expandT3Path(home)
		if !filepath.IsAbs(home) {
			return "", nil, nil, fmt.Errorf("T3 provider home must be absolute")
		}
		filtered := env[:0]
		for _, item := range env {
			if !strings.HasPrefix(item, homeVar+"=") {
				filtered = append(filtered, item)
			}
		}
		env = append(filtered, homeVar+"="+home)
	}
	return expandT3Path(binary), args, env, nil
}

func expandT3Path(path string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}

func resumeT3(id string) error {
	ctx := context.Background()
	s, err := findT3(ctx, id) // Recheck live state, never trust a cached row to resume.
	if err != nil {
		return err
	}
	binary, args, env, err := t3ResumeCommand(*s.T3)
	if err != nil {
		return err
	}
	if info, err := os.Stat(s.T3.Cwd); err != nil || !info.IsDir() {
		return fmt.Errorf("T3 working directory is unavailable: %s", clean(s.T3.Cwd))
	}
	listing, err := zellijSessions(ctx, 2*time.Second, "--no-formatting")
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok || !strings.Contains(string(exit.Stderr)+string(listing), "No active zellij sessions") {
			return fmt.Errorf("list terminal sessions: %w", err)
		}
	}
	name, live := scheduledSessionName("t3-resume-"+id, string(listing))
	inside := os.Getenv("ZELLIJ") != ""
	flag := "--create"
	if inside {
		flag = "--create-background"
	}
	zargs := []string{"attach", flag, name, "--", binary}
	zargs = append(zargs, args...)
	if live {
		zargs = []string{"attach", name}
	}
	if inside {
		if !live {
			createCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(createCtx, "zellij", zargs...)
			cmd.Env, cmd.Dir = env, s.T3.Cwd
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("open T3 conversation: %w: %s", err, clean(string(output)))
			}
		}
		markUsed(s.Name)
		return exec.Command("zellij", "action", "switch-session", name).Run()
	}
	cmd := exec.Command("zellij", zargs...)
	cmd.Env, cmd.Dir = env, s.T3.Cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	markUsed(s.Name)
	err = cmd.Run()
	markUsed(s.Name)
	return err
}
