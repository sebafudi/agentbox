package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

type t3ProcessSession struct{ Name, Provider string }

// Match a dedicated provider through its open conversation history, without
// reading that history or provider credentials. Command/cwd alone identifies
// a backend, not an individual chat.
func t3ProcessSessionName(procPath string, conversations map[string]t3ProcessSession) string {
	cmdline, err := os.ReadFile(filepath.Join(procPath, "cmdline"))
	if err != nil {
		return ""
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if len(args) < 2 {
		return ""
	}
	binary := filepath.Base(string(args[0]))
	provider := ""
	if binary == "codex" && string(args[1]) == "app-server" {
		provider = "codex"
	} else {
		comm, _ := os.ReadFile(filepath.Join(procPath, "comm"))
		claude := binary == "claude" || strings.TrimSpace(string(comm)) == "claude" || bytes.Contains(cmdline, []byte("claude-agent-sdk/cli.js"))
		if claude && bytes.Contains(cmdline, []byte("\x00stream-json\x00")) {
			provider = "claude"
		}
	}
	if provider == "" {
		return ""
	}
	ids := make(map[string]bool)
	if provider == "claude" {
		// The SDK's durable session argument also identifies an idle backend
		// which closes its transcript file between writes.
		for i := 1; i+1 < len(args); i++ {
			if flag := string(args[i]); flag == "--resume" || flag == "--session-id" {
				id := string(args[i+1])
				if t3UUID.MatchString(id) {
					ids[strings.ToLower(id)] = true
				}
			}
		}
	}
	fds, _ := os.ReadDir(filepath.Join(procPath, "fd"))
	for _, fd := range fds {
		path, err := os.Readlink(filepath.Join(procPath, "fd", fd.Name()))
		if err != nil {
			continue
		}
		name := filepath.Base(strings.TrimSuffix(path, " (deleted)"))
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if strings.HasPrefix(id, "rollout-") && len(id) > 36 {
			id = id[len(id)-36:]
		}
		if t3UUID.MatchString(id) {
			ids[strings.ToLower(id)] = true
		}
	}
	// Shared servers cannot be divided accurately between conversations,
	// including chats outside this launcher's current list.
	if len(ids) != 1 {
		return ""
	}
	for id := range ids {
		s := conversations[id]
		if s.Provider == provider || (provider == "claude" && s.Provider == "claudeAgent") {
			return s.Name
		}
	}
	return ""
}
