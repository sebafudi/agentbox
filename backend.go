package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Session struct {
	Name, Kind, Title, State, Notice, Context, PaneID string
	RunningCommand, LastCommand                       string
	Saved                                             bool
	Used, Created                                     time.Time
	FromUse                                           bool
	Metrics                                           SessionMetrics
	T3                                                *t3Thread `json:",omitempty"`
	panes                                             []pane
}
type Snapshot struct {
	Sessions []Session
	At       time.Time
	Err      string
	Warning  string
}
type Action struct {
	Verb, Name, PaneID string
	Saved              bool
}

func stateRoot() string { return filepath.Join(os.Getenv("HOME"), ".local/state/agentbox-login") }

// Separate state from older dashboards still running in existing SSH sessions.
func cachePath() string { return filepath.Join(stateRoot(), "snapshot-v3.json") }
func marker(name string) string {
	return filepath.Join(stateRoot(), "last-used", fmt.Sprintf("%x", sha256.Sum256([]byte(name))))
}
func loadCache() Snapshot {
	var s Snapshot
	data, err := os.ReadFile(cachePath())
	if os.IsNotExist(err) {
		data, err = os.ReadFile(filepath.Join(stateRoot(), "snapshot-v2.json"))
	}
	if err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}
func saveCache(s Snapshot) error {
	dir := stateRoot()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "snapshot-v3.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	// Only v3 writers participate in ordering. The v2 fallback warms first
	// launch, but an older dashboard must not prevent creation of the new cache.
	var old Snapshot
	if data, err := os.ReadFile(cachePath()); err == nil && json.Unmarshal(data, &old) == nil &&
		!old.At.IsZero() && old.At.After(s.At) {
		return nil
	}
	f, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	data, err := json.Marshal(s)
	if err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), cachePath())
}
func markUsed(name string) {
	if name == "" {
		return
	}
	path := marker(name)
	if os.MkdirAll(filepath.Dir(path), 0700) == nil {
		_ = os.WriteFile(path, nil, 0600)
	}
	_ = os.Remove(filepath.Join(stateRoot(), "state/unread", filepath.Base(path)))
}
func command(ctx context.Context, limit time.Duration, name string, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(c, name, args...)
	cmd.Env = cleanEnv()
	return cmd.Output()
}

// zellijSessions lists sessions; a user who has never started one gets an empty list, not an error.
func zellijSessions(ctx context.Context, limit time.Duration, args ...string) ([]byte, error) {
	data, err := command(ctx, limit, "zellij", append([]string{"list-sessions"}, args...)...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "No active zellij sessions") {
		return nil, nil
	}
	return data, err
}
func cleanEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "ZELLIJ=") && !strings.HasPrefix(item, "ZELLIJ_SESSION_NAME=") {
			env = append(env, item)
		}
	}
	return env
}
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')
			continue
		}
		if r < 32 || (r >= 127 && r < 160) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

type pane struct {
	ID              int    `json:"id"`
	Plugin          bool   `json:"is_plugin"`
	Focused         bool   `json:"is_focused"`
	Exited          bool   `json:"exited"`
	Title           string `json:"title"`
	Command         string `json:"pane_command"`
	TerminalCommand string `json:"terminal_command"`
	Cwd             string `json:"pane_cwd"`
	Tab             string `json:"tab_name"`
}

func (p pane) cmd() string {
	if p.Command != "" {
		return p.Command
	}
	return p.TerminalCommand
}

var agentCommand = regexp.MustCompile(`(?:^|/)\b(omp|claude|codex)(?:\s|$)`)
var nameCreation = regexp.MustCompile(`^(?:omp|claude|shell)-([0-9]{8})-([0-9]{6})-`)
var runningTitle = regexp.MustCompile(`^π [⠁-⣿]`)
var layoutCwd = regexp.MustCompile(`cwd(?:=|\s+)"([^"]+)"`)
var resumeID = regexp.MustCompile(`args\s+"--resume"\s+"([0-9a-f-]{36})"`)

func storedTitle(kind, id string) string {
	if kind == "omp" {
		query := "SELECT title FROM session_titles WHERE session_id = '" + id + "' LIMIT 1"
		data, err := command(context.Background(), 700*time.Millisecond, "sqlite3", "-readonly", filepath.Join(os.Getenv("HOME"), ".omp/agent/history.db"), query)
		if err == nil {
			return clean(string(data))
		}
		return ""
	}
	if kind == "claude" {
		files, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".claude/projects/*", id+".jsonl"))
		for _, path := range files {
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 4096), 4*1024*1024)
			title := ""
			for scanner.Scan() {
				if !strings.Contains(scanner.Text(), `"custom-title"`) {
					continue
				}
				var record struct {
					Type  string `json:"type"`
					Title string `json:"customTitle"`
				}
				if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Type == "custom-title" {
					title = clean(record.Title)
				}
			}
			f.Close()
			if title != "" {
				return title
			}
		}
	}
	return ""
}

func kindOf(cmd string) string {
	if m := agentCommand.FindStringSubmatch(cmd); len(m) > 1 {
		return m[1]
	}
	return "shell"
}
func savedMetadata(s *Session) {
	data, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".cache/zellij/contract_version_1/session_info", s.Name, "session-layout.kdl"))
	layout := string(data)
	switch {
	case strings.Contains(layout, `pane command="omp"`):
		s.Kind = "omp"
	case strings.Contains(layout, `pane command="claude"`):
		s.Kind = "claude"
	case strings.Contains(layout, `pane command="codex"`):
		s.Kind = "codex"
	case strings.HasPrefix(s.Name, "omp-") || s.Name == "agentbox":
		s.Kind = "omp"
	case strings.HasPrefix(s.Name, "claude-"):
		s.Kind = "claude"
	default:
		s.Kind = "shell"
	}
	if matches := layoutCwd.FindStringSubmatch(layout); len(matches) > 1 {
		s.Title = "Resume in " + strings.Replace(matches[1], os.Getenv("HOME"), "~", 1)
		s.Context = matches[1]
	}
	if match := resumeID.FindStringSubmatch(layout); len(match) > 1 {
		if topic := storedTitle(s.Kind, match[1]); topic != "" {
			s.Title = topic
		}
	}
	if s.Title == "" {
		s.Title = s.Name
	}
	s.State = "SAVED"
}
func createdAt(name string) time.Time {
	path := filepath.Join(os.Getenv("HOME"), ".cache/zellij/contract_version_1/session_info", name)
	var stat unix.Statx_t
	if unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat) == nil && stat.Mask&unix.STATX_BTIME != 0 {
		return time.Unix(stat.Btime.Sec, 0)
	}
	if m := nameCreation.FindStringSubmatch(name); len(m) > 0 {
		t, e := time.ParseInLocation("20060102-150405", m[1]+"-"+m[2], time.Local)
		if e == nil {
			return t
		}
	}
	return time.Time{}
}
func observe(s *Session) {
	if s.Kind == "shell" || s.Saved {
		return
	}
	path := filepath.Join(stateRoot(), "state", filepath.Base(marker(s.Name)))
	old, _ := os.ReadFile(path)
	if s.State == "READY" || s.State == "ASK" {
		if string(old) == "RUN" || (string(old) == "ASK" && s.State == "READY") {
			_ = os.MkdirAll(filepath.Dir(path)+"/unread", 0700)
			_ = os.WriteFile(filepath.Dir(path)+"/unread/"+filepath.Base(path), nil, 0600)
		}
	}
	if s.State == "RUN" {
		_ = os.Remove(filepath.Dir(path) + "/unread/" + filepath.Base(path))
	}
	if string(old) != s.State {
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		_ = os.WriteFile(path, []byte(s.State), 0600)
	}
	if _, err := os.Stat(filepath.Dir(path) + "/unread/" + filepath.Base(path)); err == nil {
		s.Notice = "NEW"
	} else if !s.Used.IsZero() {
		if st, e := os.Stat(path); e == nil && s.Used.After(st.ModTime()) {
			s.Notice = "SEEN"
		}
	}
}
func inspect(ctx context.Context, s Session) Session {
	if s.Saved {
		savedMetadata(&s)
		return s
	}
	data, err := command(ctx, 1200*time.Millisecond, "zellij", "-s", s.Name, "action", "list-panes", "--command", "--json")
	if err != nil {
		s.Kind = "shell"
		s.Title = s.Name
		s.State = "UNKNOWN"
		return s
	}
	var panes []pane
	if json.Unmarshal(data, &panes) != nil {
		s.Kind = "shell"
		s.Title = s.Name
		s.State = "UNKNOWN"
		return s
	}
	s.panes = panes
	var focus *pane
	var agent *pane
	for i := range panes {
		p := &panes[i]
		if p.Plugin {
			continue
		}
		if p.Focused {
			focus = p
		}
		if !p.Exited && agent == nil && kindOf(p.cmd()) != "shell" {
			agent = p
		}
		s.Context += " " + clean(p.Cwd) + " " + clean(p.cmd()) + " " + clean(p.Tab) + " " + clean(p.Title)
	}
	s.Kind = "shell"
	s.State = "SHELL"
	if agent != nil {
		s.Kind = kindOf(agent.cmd())
		s.PaneID = fmt.Sprint(agent.ID)
		s.Title = clean(agent.Title)
		switch {
		case strings.HasPrefix(s.Title, "π >"):
			s.State = "READY"
		case runningTitle.MatchString(s.Title):
			s.State = "RUN"
		default:
			s.State = "UNKNOWN"
		}
		if s.Kind == "claude" {
			s.State = "UNKNOWN"
		}
		if screen, err := command(ctx, 800*time.Millisecond, "zellij", "-s", s.Name, "action", "dump-screen", "--pane-id", "terminal_"+s.PaneID); err == nil {
			view := string(screen)
			if s.Kind == "omp" && strings.Contains(view, "╭─ Ask ") && (strings.Contains(view, "Enter select") || strings.Contains(view, "Enter submit") || strings.Contains(view, "Space toggle")) {
				s.State = "ASK"
			}
			if s.Kind == "claude" {
				lines := strings.Split(view, "\n")
				if len(lines) > 12 {
					lines = lines[len(lines)-12:]
				}
				bottom := strings.Join(lines, "\n")
				lower := strings.ToLower(bottom)
				switch {
				case strings.Contains(lower, "esc to cancel") && (strings.Contains(lower, "yes") || strings.Contains(lower, "allow")):
					s.State = "ASK"
				case strings.Contains(lower, "esc to interrupt"):
					s.State = "RUN"
				case strings.Contains(bottom, "\n❯"):
					s.State = "READY"
				}
			}
		}
	} else if focus != nil {
		s.Title = clean(focus.Title)
		if s.Title == "" {
			s.Title = clean(focus.cmd())
		}
	}
	if s.Title == "" {
		s.Title = s.Name
	}
	observe(&s)
	return s
}
func collect(ctx context.Context) Snapshot {
	data, zellijErr := zellijSessions(ctx, 3*time.Second, "--reverse", "--no-formatting")
	var sessions []Session
	for _, line := range strings.Split(string(data), "\n") {
		name, _, ok := strings.Cut(line, " [Created")
		if !ok || name == "" {
			continue
		}
		s := Session{Name: clean(name), Saved: strings.Contains(line, "(EXITED"), Created: createdAt(name), Notice: "-"}
		if st, e := os.Stat(marker(name)); e == nil && !s.Saved {
			s.Used = st.ModTime()
			s.FromUse = true
		} else {
			s.Used = s.Created
		}
		sessions = append(sessions, s)
	}
	out := make([]Session, len(sessions))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, s Session) { defer wg.Done(); slots <- struct{}{}; out[i] = inspect(ctx, s); <-slots }(i, s)
	}
	wg.Wait()
	t3, err := collectT3(ctx)
	if err != nil {
		// Retain chats through a transient database failure without hiding Zellij.
		for _, s := range loadCache().Sessions {
			if s.T3 != nil {
				out = append(out, s)
			}
		}
	} else {
		out = append(out, t3...)
	}
	populateMetrics(out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Saved != out[j].Saved {
			return !out[i].Saved
		}
		return out[i].Used.After(out[j].Used)
	})
	snapshot := Snapshot{Sessions: out, At: time.Now()}
	if err != nil {
		snapshot.Warning = "T3 unavailable: " + err.Error()
	}
	if zellijErr != nil {
		if len(out) == 0 {
			snapshot.Err = "Zellij unavailable"
		} else {
			snapshot.Warning = strings.TrimSpace(snapshot.Warning + " Zellij unavailable")
		}
	}
	return snapshot
}
func runAction(a Action) error {
	if a.Verb == "t3-resume" {
		return resumeT3(a.Name)
	}
	if a.Verb == "scheduled-join" {
		return joinScheduledRun(a.Name)
	}
	if a.Verb == "plain-shell" || a.Verb == "logout" {
		return nil
	}
	if a.Verb == "switch" {
		if a.Name == "" {
			return fmt.Errorf("missing session")
		}
		if a.Saved {
			// switch-session only accepts running servers. Resurrect the saved layout
			// without stealing this terminal, then switch the existing Zellij client.
			cmd := exec.Command("zellij", "attach", "--create-background", a.Name)
			cmd.Env = cleanEnv()
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("resurrect %s: %w", a.Name, err)
			}
		}
		markUsed(a.Name)
		args := []string{"action", "switch-session", a.Name}
		if a.PaneID != "" {
			args = append(args, "--pane-id", "terminal_"+a.PaneID)
		}
		return exec.Command("zellij", args...).Run()
	}
	var args []string
	name := a.Name
	switch a.Verb {
	case "attach":
		if name == "" {
			return fmt.Errorf("missing session")
		}
		args = []string{"attach", name}
	case "omp-new", "omp-last", "claude-new", "claude-last", "shell-new":
		kind := strings.SplitN(a.Verb, "-", 2)[0]
		if strings.HasSuffix(a.Verb, "-last") {
			// A cached row can outlive its Zellij server; never attach to a stale name.
			if live, err := zellijSessions(context.Background(), 2*time.Second, "--reverse", "--no-formatting"); err == nil {
				for _, s := range loadCache().Sessions {
					if s.Saved || s.Kind != kind {
						continue
					}
					for _, line := range strings.Split(string(live), "\n") {
						if strings.HasPrefix(line, s.Name+" [Created") && !strings.Contains(line, "(EXITED") {
							return runAction(Action{Verb: "attach", Name: s.Name})
						}
					}
				}
			}
		}
		name = fmt.Sprintf("%s-%s-%d", kind, time.Now().Format("20060102-150405"), os.Getpid())
		commandName := kind
		if kind == "shell" {
			commandName = "bash"
		}
		args = []string{"attach", "--create", name, "--", commandName}
		if kind == "claude" {
			// Makes bypass mode reachable via Shift+Tab; sessions still start in default mode.
			args = append(args, "--allow-dangerously-skip-permissions")
		}
		if strings.HasSuffix(a.Verb, "-last") {
			args = append(args, "--continue")
		}
	default:
		return fmt.Errorf("unknown action %q", a.Verb)
	}
	markUsed(name)
	cmd := exec.Command("zellij", args...)
	cmd.Env = cleanEnv()
	cmd.Dir = filepath.Join(os.Getenv("HOME"), "code")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	markUsed(name)
	return err
}
