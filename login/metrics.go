package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// CPU is percentage of one logical CPU. RAM sums process RSS, so shared pages
// can be counted more than once. Neither value includes Zellij's server.
type SessionMetrics struct {
	Available   bool
	CPU         float64
	CPUKnown    bool
	RAM         uint64
	AvgCPU      float64
	MaxCPU      float64
	AvgRAM      uint64
	MaxRAM      uint64
	Samples     int
	CPUSamples  int
	SpanSeconds int
	History     []metricSample
}

const (
	metricWindow   = 24 * time.Hour
	metricInterval = 8 * time.Second
	metricLimit    = int(metricWindow/metricInterval) + 1
	// Linux exposes process CPU times and starttime in USER_HZ (100 on this Linux architecture).
	linuxClockTicks = 100.0
)

type processKey struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

type processUsage struct {
	Key    processKey
	Parent int `json:"-"`
	Ticks  uint64
	RSS    uint64
	Pane   string `json:"-"`
	Pgrp   int    `json:"-"`
	Tpgid  int    `json:"-"`
	TTY    int    `json:"-"`
	Cmd    string `json:"-"`
	Idle   bool   `json:"-"`
}

type metricSample struct {
	At       time.Time `json:"at"`
	CPU      float64   `json:"cpu"`
	CPUKnown bool      `json:"cpu_known"`
	RAM      uint64    `json:"ram"`
}

type sessionHistory struct {
	Server   processKey        `json:"server"`
	Last     time.Time         `json:"last"`
	Prior    []processUsage    `json:"prior"`
	Samples  []metricSample    `json:"samples"`
	Commands map[string]string `json:"commands,omitempty"`
}

type metricHistory struct {
	Sessions map[string]*sessionHistory `json:"sessions"`
}

type liveProcesses struct {
	server processKey
	items  []processUsage
	bad    bool
}

// /proc/<pid>/stat uses a parenthesized command name which may itself contain
// spaces and parentheses. Everything following the final ") " is positional.
func parseProcessStat(data []byte, pid int, pageSize uint64) (processUsage, error) {
	at := bytes.LastIndex(data, []byte(") "))
	if at < 0 || !bytes.HasPrefix(data, []byte(strconv.Itoa(pid)+" (")) {
		return processUsage{}, errors.New("invalid proc stat")
	}
	fields := bytes.Fields(data[at+2:])
	if len(fields) < 22 {
		return processUsage{}, errors.New("short proc stat")
	}
	parent, err := strconv.Atoi(string(fields[1]))
	if err != nil || parent < 0 {
		return processUsage{}, errors.New("invalid parent pid")
	}
	pgrp, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return processUsage{}, errors.New("invalid process group")
	}
	tty, err := strconv.Atoi(string(fields[4]))
	if err != nil {
		return processUsage{}, errors.New("invalid tty")
	}
	tpgid, err := strconv.Atoi(string(fields[5]))
	if err != nil {
		return processUsage{}, errors.New("invalid foreground group")
	}
	read := func(index int) (uint64, error) { return strconv.ParseUint(string(fields[index]), 10, 64) }
	user, err := read(11)
	if err != nil {
		return processUsage{}, err
	}
	system, err := read(12)
	if err != nil {
		return processUsage{}, err
	}
	start, err := read(19)
	if err != nil {
		return processUsage{}, err
	}
	pages, err := strconv.ParseInt(string(fields[21]), 10, 64)
	if err != nil || pages < 0 || start == 0 || system > ^uint64(0)-user {
		return processUsage{}, errors.New("invalid proc counters")
	}
	return processUsage{Key: processKey{PID: pid, Start: start}, Parent: parent, Ticks: user + system, RSS: uint64(pages) * pageSize, Pgrp: pgrp, Tpgid: tpgid, TTY: tty}, nil
}

func envValue(env []byte, key string) string {
	prefix := []byte(key + "=")
	for len(env) > 0 {
		item, rest, ok := bytes.Cut(env, []byte{0})
		if !ok {
			item, rest = env, nil
		}
		if bytes.HasPrefix(item, prefix) {
			return string(item[len(prefix):])
		}
		env = rest
	}
	return ""
}

func zellijServer(cmdline []byte) bool {
	parts := bytes.Split(cmdline, []byte{0})
	if len(parts) < 2 || filepath.Base(string(parts[0])) != "zellij" {
		return false
	}
	for _, arg := range parts[1:] {
		if string(arg) == "--server" {
			return true
		}
	}
	return false
}

func idleShell(cmdline []byte) bool {
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if len(args) == 0 {
		return false
	}
	name := filepath.Base(strings.TrimPrefix(string(args[0]), "-"))
	switch name {
	case "bash", "zsh", "fish", "sh", "dash", "ksh", "nu":
	default:
		return false
	}
	for _, arg := range args[1:] {
		if bytes.Equal(arg, []byte("-c")) || bytes.Equal(arg, []byte("-lc")) || bytes.Equal(arg, []byte("-ic")) || (len(arg) > 0 && arg[0] != '-') {
			return false
		}
	}
	return true
}

func displayProcessCommand(cmdline []byte) string {
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if len(args) == 0 || len(args[0]) == 0 {
		return ""
	}
	args[0] = []byte(filepath.Base(string(args[0])))
	if len(args) > 4 {
		args = args[:4]
	}
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if len(arg) != 0 {
			parts = append(parts, clean(string(arg)))
		}
	}
	command := strings.Join(parts, " ")
	if len(command) > 160 {
		command = strings.ToValidUTF8(command[:160], "")
	}
	return command
}

func scanSessionProcesses(root string, names map[string]bool) (map[string]*liveProcesses, error) {
	return scanWorkspaceProcesses(root, names, nil)
}

func scanWorkspaceProcesses(root string, names map[string]bool, conversations map[string]t3ProcessSession) (map[string]*liveProcesses, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	found := make(map[string]*liveProcesses, len(names))
	for name := range names {
		found[name] = &liveProcesses{}
	}
	uid := uint32(os.Getuid())
	pageSize := uint64(os.Getpagesize())
	// A collector inside a pane inherits its marker, but is not pane workload.
	ownPID := -1
	if root == "/proc" {
		ownPID = os.Getpid()
	}
	type candidate struct {
		usage processUsage
		name  string
		pane  string
		path  string
	}
	all := make(map[int]*candidate)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == ownPID {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uid {
			continue
		}
		env, _ := os.ReadFile(filepath.Join(path, "environ"))
		name := envValue(env, "ZELLIJ_SESSION_NAME")
		pane := envValue(env, "ZELLIJ_PANE_ID")
		data, err := os.ReadFile(filepath.Join(path, "stat"))
		if err != nil {
			if group := found[name]; group != nil && pane != "" {
				group.bad = true
			}
			continue
		}
		usage, err := parseProcessStat(data, pid, pageSize)
		if err != nil {
			if group := found[name]; group != nil && pane != "" {
				group.bad = true
			}
			continue
		}
		all[pid] = &candidate{usage: usage, name: name, pane: pane, path: path}
	}
	providerRoots := make(map[int]string)
	if len(conversations) > 0 {
		for pid, p := range all {
			if name := t3ProcessSessionName(p.path, conversations); name != "" {
				providerRoots[pid] = name
				if key := found[name].server; key.PID == 0 || pid < key.PID {
					found[name].server = p.usage.Key
				}
			}
		}
	}
	// Explicit provider ownership takes priority over inherited pane markers:
	// a T3 server can itself have been started from inside a Zellij terminal.
	var providerOwner func(int, map[int]bool) string
	providerOwner = func(pid int, seen map[int]bool) string {
		p := all[pid]
		if p == nil || seen[pid] {
			return ""
		}
		seen[pid] = true
		if name := providerRoots[pid]; name != "" {
			return name
		}
		return providerOwner(p.usage.Parent, seen)
	}
	// Children may clear their pane environment or make it unreadable (btop
	// does this). Their same-user parent chain still identifies the pane;
	// ancestry of the Zellij server alone never does.
	var owner func(int, map[int]bool) (string, string)
	owner = func(pid int, seen map[int]bool) (string, string) {
		p := all[pid]
		if p == nil || seen[pid] {
			return "", ""
		}
		seen[pid] = true
		if p.pane != "" && found[p.name] != nil {
			return p.name, p.pane
		}
		return owner(p.usage.Parent, seen)
	}
	for pid, p := range all {
		name := providerOwner(pid, make(map[int]bool))
		pane := "t3"
		if name == "" {
			name, pane = owner(pid, make(map[int]bool))
		}
		if pane == "" {
			continue
		}
		group := found[name]
		cmdline, err := os.ReadFile(filepath.Join(p.path, "cmdline"))
		if err != nil {
			group.bad = true
			continue
		}
		if zellijServer(cmdline) {
			group.server = p.usage.Key
			continue
		}
		p.usage.Pane = pane
		p.usage.Cmd = displayProcessCommand(cmdline)
		p.usage.Idle = idleShell(cmdline)
		group.items = append(group.items, p.usage)
	}
	for _, p := range all {
		if found[p.name] == nil || p.pane != "" {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(p.path, "cmdline"))
		if err == nil && zellijServer(cmdline) {
			found[p.name].server = p.usage.Key
		}
	}
	return found, nil
}

// A shell at its prompt is not a running command, even when its own process
// group owns the tty. Prefer a foreground job in the focused pane.
func shellCommands(s *Session, live *liveProcesses, h *sessionHistory) {
	if s.Kind != "shell" || len(s.panes) == 0 {
		return
	}
	ordered := make([]pane, 0, len(s.panes))
	for _, p := range s.panes {
		if !p.Plugin && !p.Exited && p.Focused {
			ordered = append(ordered, p)
		}
	}
	for _, p := range s.panes {
		if !p.Plugin && !p.Exited && !p.Focused {
			ordered = append(ordered, p)
		}
	}
	keep := make(map[string]string, min(len(ordered), 32))
	for _, pane := range ordered {
		if len(keep) >= 32 {
			break
		}
		id := strconv.Itoa(pane.ID)
		last := h.Commands[id]
		if last == "" {
			command := strings.Fields(pane.cmd())
			if len(command) != 0 && kindOf(command[0]) == "shell" && !idleShell([]byte(strings.Join(command, "\x00")+"\x00")) {
				last = clean(pane.cmd())
			}
		}
		if last != "" {
			keep[id] = last
		}
		if s.LastCommand == "" {
			s.LastCommand = last
		}
		var tty, foreground int
		var shellStart uint64
		for i := range live.items {
			proc := &live.items[i]
			if proc.Pane == id && proc.Idle && proc.TTY > 0 && (tty == 0 || proc.Key.Start < shellStart) {
				tty, foreground, shellStart = proc.TTY, proc.Tpgid, proc.Key.Start
			}
		}
		var best *processUsage
		for i := range live.items {
			proc := &live.items[i]
			if proc.Pane != id || proc.TTY <= 0 || proc.Pgrp <= 0 || proc.Pgrp != proc.Tpgid || proc.Cmd == "" || proc.Idle ||
				(tty != 0 && (proc.TTY != tty || proc.Pgrp != foreground)) {
				continue
			}
			if best == nil || (proc.Key.PID == proc.Pgrp && best.Key.PID != best.Pgrp) ||
				(proc.Key.PID != proc.Pgrp && best.Key.PID != best.Pgrp && proc.Key.Start < best.Key.Start) {
				best = proc
			}
		}
		if best != nil {
			keep[id] = best.Cmd
			if s.RunningCommand == "" {
				s.RunningCommand = best.Cmd
				s.PaneID = id
			}
		}
	}
	if s.RunningCommand != "" {
		s.LastCommand = ""
	}
	h.Commands = keep
}

func populateMetrics(sessions []Session) {
	populateSessionMetrics("/proc", stateRoot(), sessions, time.Now)
}

// The timer and dashboard are distinct processes. Lock before scanning so a
// later snapshot cannot use an older baseline or overwrite newer history.
func populateSessionMetrics(procRoot, dir string, sessions []Session, clock func() time.Time) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	// Older dashboards still write and prune metrics-v2.json. Keep the new
	// history under a separate lock so they cannot truncate the 24h window.
	lock, err := os.OpenFile(filepath.Join(dir, "metrics-v3.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX) != nil {
		return
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	names := make(map[string]bool)
	conversations := make(map[string]t3ProcessSession)
	for _, s := range sessions {
		if !s.Saved {
			names[s.Name] = true
			if s.T3 != nil && t3UUID.MatchString(s.T3.Conversation) {
				id := strings.ToLower(s.T3.Conversation)
				owner := t3ProcessSession{Name: s.Name, Provider: s.T3.Provider}
				if _, duplicate := conversations[id]; duplicate {
					owner.Name = "" // Do not count a shared conversation twice.
				}
				conversations[id] = owner
			}
		}
	}
	found, err := scanWorkspaceProcesses(procRoot, names, conversations)
	if err != nil {
		return
	}
	now := clock()
	path := filepath.Join(dir, "metrics-v3.json")
	var saved metricHistory
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data, err = os.ReadFile(filepath.Join(dir, "metrics-v2.json"))
	}
	if err == nil {
		_ = json.Unmarshal(data, &saved)
	}
	if saved.Sessions == nil {
		saved.Sessions = make(map[string]*sessionHistory)
	}
	for name := range saved.Sessions {
		if !names[name] {
			delete(saved.Sessions, name)
		}
	}
	for i := range sessions {
		s := &sessions[i]
		if s.Saved {
			continue
		}
		s.Metrics = SessionMetrics{}
		s.RunningCommand, s.LastCommand = "", ""
		live := found[s.Name]
		if live == nil {
			continue
		}
		h := saved.Sessions[s.Name]
		if h == nil {
			h = &sessionHistory{}
			saved.Sessions[s.Name] = h
		}
		if live.server.PID != 0 && h.Server.PID != 0 && live.server != h.Server {
			*h = sessionHistory{}
		}
		if live.server.PID != 0 {
			h.Server = live.server
		}
		fresh := h.Samples[:0]
		for _, sample := range h.Samples {
			if !sample.At.After(now) && now.Sub(sample.At) <= metricWindow {
				fresh = append(fresh, sample)
			}
		}
		h.Samples = fresh
		if len(h.Samples) > metricLimit {
			h.Samples = h.Samples[len(h.Samples)-metricLimit:]
		}
		shellCommands(s, live, h)
		s.Metrics.History = append([]metricSample(nil), h.Samples...)
		if live.bad || len(live.items) == 0 {
			h.Prior = nil
			h.Last = time.Time{}
			continue
		}
		m := SessionMetrics{Available: true}
		current := make([]processUsage, 0, len(live.items))
		prior := make(map[processKey]uint64, len(h.Prior))
		for _, p := range h.Prior {
			prior[p.Key] = p.Ticks
		}
		var ticks uint64
		matched := 0
		for _, p := range live.items {
			m.RAM += p.RSS
			current = append(current, p)
			if old, ok := prior[p.Key]; ok && p.Ticks >= old {
				ticks += p.Ticks - old
				matched++
			}
		}
		elapsed := now.Sub(h.Last).Seconds()
		if matched == len(live.items) && len(h.Prior) == len(live.items) && elapsed >= 1 && elapsed <= 45 {
			m.CPUKnown = true
			m.CPU = float64(ticks) / linuxClockTicks / elapsed * 100
		}
		h.Prior, h.Last = current, now
		if len(h.Samples) == 0 || now.Sub(h.Samples[len(h.Samples)-1].At) >= metricInterval {
			h.Samples = append(h.Samples, metricSample{At: now, CPU: m.CPU, CPUKnown: m.CPUKnown, RAM: m.RAM})
			if len(h.Samples) > metricLimit {
				h.Samples = h.Samples[len(h.Samples)-metricLimit:]
			}
		}
		m.History = append([]metricSample(nil), h.Samples...)
		metricSummary(&m, h.Samples)
		s.Metrics = m
	}
	data, err = json.Marshal(saved)
	if err != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".metrics-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return
	}
	if err = file.Close(); err != nil {
		return
	}
	_ = os.Rename(file.Name(), path)
}

func metricSummary(m *SessionMetrics, samples []metricSample) {
	m.Samples = len(samples)
	if m.Samples == 0 {
		return
	}
	m.SpanSeconds = int(samples[len(samples)-1].At.Sub(samples[0].At).Seconds())
	var ramSum uint64
	var cpuSum float64
	cpuCount := 0
	for _, sample := range samples {
		ramSum += sample.RAM
		if sample.RAM > m.MaxRAM {
			m.MaxRAM = sample.RAM
		}
		if sample.CPUKnown {
			cpuSum += sample.CPU
			cpuCount++
			if sample.CPU > m.MaxCPU {
				m.MaxCPU = sample.CPU
			}
		}
	}
	m.AvgRAM = ramSum / uint64(len(samples))
	m.CPUSamples = cpuCount
	if cpuCount > 0 {
		m.AvgCPU = cpuSum / float64(cpuCount)
	}
}
