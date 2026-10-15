package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type scheduledRun struct {
	ID                  string                 `json:"id"`
	JobID               string                 `json:"jobId"`
	JobName             string                 `json:"jobName"`
	Runner              string                 `json:"runner"`
	Status              string                 `json:"status"`
	Outcome             string                 `json:"outcome"`
	StartedAt           string                 `json:"startedAt"`
	FinishedAt          string                 `json:"finishedAt"`
	DurationMs          *int64                 `json:"durationMs"`
	ExitCode            *int                   `json:"exitCode"`
	Reason              string                 `json:"reason"`
	Log                 string                 `json:"log"`
	Cwd                 string                 `json:"cwd"`
	SessionDir          string                 `json:"sessionDir"`
	SessionID           string                 `json:"sessionId"`
	WrappedAgent        string                 `json:"wrappedAgent"`
	Continuation        *scheduledContinuation `json:"continuation"`
	ContinuationBlocked string                 `json:"continuationBlocked"`
}

type scheduledContinuation struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

func (r scheduledRun) hasWrappedSession() bool {
	return r.Runner == "command" && r.Continuation != nil && r.Continuation.Kind == "claude" &&
		r.Continuation.SessionID != "" && r.Continuation.Cwd != "" && r.Continuation.Cwd == r.Cwd
}

func (r scheduledRun) commandLogOnly() bool {
	return r.Runner == "command" && !r.hasWrappedSession() && r.WrappedAgent != "claude"
}

type scheduledJob struct {
	ID                    string        `json:"id"`
	Name                  string        `json:"name"`
	Schedule              string        `json:"schedule"`
	Timezone              string        `json:"timezone"`
	TimeoutMinutes        int           `json:"timeoutMinutes"`
	Model                 string        `json:"model"`
	Runner                string        `json:"runner"`
	Cwd                   string        `json:"cwd"`
	Prompt                string        `json:"prompt"`
	Argv                  []string      `json:"argv"`
	PermissionMode        string        `json:"permissionMode"`
	AllowedTools          []string      `json:"allowedTools"`
	AdditionalDirectories []string      `json:"additionalDirectories"`
	Enabled               bool          `json:"enabled"`
	Status                string        `json:"status"`
	NextRun               string        `json:"nextRun"`
	LastRun               *scheduledRun `json:"lastRun"`
	Stats                 struct {
		Total          int    `json:"total"`
		Success        int    `json:"success"`
		Failed         int    `json:"failed"`
		Running        int    `json:"running"`
		LastDurationMs *int64 `json:"lastDurationMs"`
	} `json:"stats"`
}

type schedulerSnapshot struct {
	Jobs []scheduledJob `json:"jobs"`
	Runs []scheduledRun `json:"runs"`
}

type schedulerRunDetail struct {
	Run    scheduledRun `json:"run"`
	Output string       `json:"output"`
}

func schedulerSocket() string {
	if socket := os.Getenv("SCHEDULER_SOCKET"); socket != "" {
		return socket
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	return filepath.Join(dir, "agent-scheduler.sock")
}

// Every request is local-only and deadline-bounded, with no state-file fallback.
func schedulerRequest(ctx context.Context, socket, method, path string, body any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, "http://scheduler"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("scheduler unavailable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(res.StatusCode)
		}
		return fmt.Errorf("scheduler: %s (HTTP %d)", clean(failure.Error), res.StatusCode)
	}
	if result == nil {
		result = &struct{}{}
	}
	if err = json.NewDecoder(res.Body).Decode(result); err != nil {
		return fmt.Errorf("invalid scheduler response: %w", err)
	}
	return nil
}

func scheduledRunPath(id string) string { return "/runs/" + url.PathEscape(id) }

func schedulerJoinName(run scheduledRun) string {
	mode := "resume"
	if run.Status == "running" || run.commandLogOnly() {
		mode = "monitor"
	}
	return fmt.Sprintf("scheduled-%s-%x", mode, sha256.Sum256([]byte(run.ID)))[:len("scheduled-"+mode+"-")+20]
}

func scheduledSessionName(base, listing string) (name string, live bool) {
	stale := false
	next := 1
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		candidate := fields[0]
		if candidate != base && !strings.HasPrefix(candidate, base+"-") {
			continue
		}
		if strings.Contains(line, "(EXITED") {
			stale = true
			if n, err := strconv.Atoi(strings.TrimPrefix(candidate, base+"-")); err == nil {
				next = max(next, n+1)
			}
			continue
		}
		return candidate, true
	}
	// Never resurrect an old layout: it may contain a completed interactive writer.
	if stale {
		return fmt.Sprintf("%s-%d", base, next), false
	}
	return base, false
}

func schedulerJoinArgs(runID, name string, background bool) ([]string, error) {
	if runID == "" || name == "" {
		return nil, fmt.Errorf("missing scheduled run or session ID")
	}
	flag := "--create"
	if background {
		flag = "--create-background"
	}
	// Pass the exact run ID as an argument, never through shell interpolation.
	return []string{"attach", flag, name, "--", "scheduler", "join", runID}, nil
}

func joinScheduledRun(runID string) error {
	if runID == "" {
		return fmt.Errorf("missing scheduled run ID")
	}
	var detail schedulerRunDetail
	if err := schedulerRequest(context.Background(), schedulerSocket(), http.MethodGet, scheduledRunPath(runID), nil, &detail); err != nil {
		return err
	}
	if detail.Run.ID != runID {
		return fmt.Errorf("scheduler returned a different run")
	}
	if detail.Run.Status != "running" && detail.Run.ContinuationBlocked != "" {
		return fmt.Errorf("%s", clean(detail.Run.ContinuationBlocked))
	}
	if detail.Run.Runner == "command" && detail.Run.Status != "running" && !detail.Run.hasWrappedSession() {
		return fmt.Errorf("command runs have no agent session; view their logs instead")
	}
	listing, err := zellijSessions(context.Background(), 2*time.Second, "--no-formatting")
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok || (!strings.Contains(string(exit.Stderr), "No active zellij sessions") && !strings.Contains(string(listing), "No active zellij sessions")) {
			return fmt.Errorf("list scheduled sessions: %w", err)
		}
	}
	name, live := scheduledSessionName(schedulerJoinName(detail.Run), string(listing))
	inside := os.Getenv("ZELLIJ") != ""
	args, err := schedulerJoinArgs(runID, name, inside)
	if err != nil {
		return err
	}
	if inside {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if !live {
			create := exec.CommandContext(ctx, "zellij", args...)
			create.Env = cleanEnv()
			if output, err := create.CombinedOutput(); err != nil {
				return fmt.Errorf("open scheduled session: %w: %s", err, clean(string(output)))
			}
		}
		return exec.CommandContext(ctx, "zellij", "action", "switch-session", name).Run()
	}
	if live {
		args = []string{"attach", name}
	}
	cmd := exec.Command("zellij", args...)
	cmd.Env = cleanEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
