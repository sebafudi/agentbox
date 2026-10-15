package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeProc(t *testing.T, root string, pid int, start, ticks uint64, pages int, env, cmd string, parent ...int) {
	t.Helper()
	path := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	if len(parent) != 0 {
		fields[1] = strconv.Itoa(parent[0])
	}
	if len(parent) >= 4 {
		fields[2] = strconv.Itoa(parent[1])
		fields[4] = strconv.Itoa(parent[2])
		fields[5] = strconv.Itoa(parent[3])
	}
	fields[11], fields[12] = strconv.FormatUint(ticks, 10), "0"
	fields[19] = strconv.FormatUint(start, 10)
	fields[21] = strconv.Itoa(pages)
	files := map[string][]byte{
		"stat":    []byte(fmt.Sprintf("%d (a process (with parentheses)) %s\n", pid, strings.Join(fields, " "))),
		"environ": []byte(env),
		"cmdline": []byte(cmd),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(path, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionMetricsAttributionAndHistory(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	name := "live"
	env := "ZELLIJ_SESSION_NAME=live\x00ZELLIJ_PANE_ID=0\x00"
	fakeProc(t, root, 101, 10, 40000, 1000, "ZELLIJ_SESSION_NAME=live\x00", "/usr/bin/zellij\x00--server\x00socket\x00")
	fakeProc(t, root, 102, 11, 100, 10, env, "omp\x00")
	fakeProc(t, root, 103, 12, 200, 20, env, "helper\x00")
	fakeProc(t, root, 104, 13, 5000, 40, "ZELLIJ_SESSION_NAME=other\x00ZELLIJ_PANE_ID=0\x00", "claude\x00")
	fakeProc(t, root, 105, 14, 5000, 60, "ZELLIJ_SESSION_NAME=live\x00", "unattributed\x00")
	at := time.Unix(1_700_000_000, 0)
	first := []Session{{Name: name}, {Name: "saved", Saved: true}}
	populateSessionMetrics(root, store, first, func() time.Time { return at })
	m := first[0].Metrics
	if !m.Available || m.CPUKnown || m.RAM != 30*uint64(os.Getpagesize()) || m.Samples != 1 || m.MaxRAM != m.RAM || m.SpanSeconds != 0 {
		t.Fatalf("first sample = %+v", m)
	}
	if first[1].Metrics.Available || first[1].Metrics.Samples != 0 {
		t.Fatalf("saved session has metrics: %+v", first[1].Metrics)
	}
	fakeProc(t, root, 102, 11, 1600, 16, env, "omp\x00")
	second := []Session{{Name: name}}
	populateSessionMetrics(root, store, second, func() time.Time { return at.Add(10 * time.Second) })
	m = second[0].Metrics
	if !m.Available || !m.CPUKnown || math.Abs(m.CPU-150) > 0.001 || m.RAM != 36*uint64(os.Getpagesize()) ||
		m.Samples != 2 || m.SpanSeconds != 10 || math.Abs(m.AvgCPU-150) > 0.001 || math.Abs(m.MaxCPU-150) > 0.001 || m.AvgRAM != 33*uint64(os.Getpagesize()) {
		t.Fatalf("second sample = %+v", m)
	}
}

func TestShellForegroundJobAndLastObservedPerPane(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const shellEnv = "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=3\x00"
	const secondEnv = "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=4\x00"
	at := time.Unix(1_700_000_000, 0)
	fakeProc(t, root, 100, 10, 90000, 100, "ZELLIJ_SESSION_NAME=x\x00", "zellij\x00--server\x00")
	fakeProc(t, root, 101, 11, 50, 2, shellEnv, "/bin/bash\x00", 100, 101, 12, 102)
	fakeProc(t, root, 102, 12, 100, 8, "", "/usr/bin/btop\x00", 101, 102, 12, 102)
	fakeProc(t, root, 103, 13, 50, 3, secondEnv, "/bin/bash\x00", 100, 103, 13, 104)
	fakeProc(t, root, 104, 14, 150, 5, secondEnv, "/usr/bin/htop\x00", 103, 104, 13, 104)
	panes := []pane{{ID: 3, Focused: true, Command: "bash", Title: "user@host:~/code"}, {ID: 4, Command: "bash"}}
	s := []Session{{Name: "x", Kind: "shell", panes: panes}}
	populateSessionMetrics(root, store, s, func() time.Time { return at })
	if s[0].RunningCommand != "btop" || s[0].LastCommand != "" || s[0].PaneID != "3" ||
		!s[0].Metrics.Available || s[0].Metrics.RAM != 18*uint64(os.Getpagesize()) {
		t.Fatalf("focused foreground command/ancestry attribution = %+v", s[0])
	}
	// Both panes become idle: the previously observed job remains available,
	// but must not be reported as still running.
	fakeProc(t, root, 101, 11, 150, 2, shellEnv, "/bin/bash\x00", 100, 101, 12, 101)
	fakeProc(t, root, 103, 13, 150, 3, secondEnv, "/bin/bash\x00", 100, 103, 13, 103)
	if err := os.RemoveAll(filepath.Join(root, "102")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "104")); err != nil {
		t.Fatal(err)
	}
	s = []Session{{Name: "x", Kind: "shell", panes: panes}}
	populateSessionMetrics(root, store, s, func() time.Time { return at.Add(10 * time.Second) })
	if s[0].RunningCommand != "" || s[0].LastCommand != "btop" || s[0].Metrics.RAM != 5*uint64(os.Getpagesize()) {
		t.Fatalf("idle shell should retain only last observed command: %+v", s[0])
	}
	if len(s[0].Metrics.History) != 2 || !s[0].Metrics.History[1].At.Equal(at.Add(10*time.Second)) {
		t.Fatalf("chart history missing: %+v", s[0].Metrics.History)
	}
	data, err := json.Marshal(s[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"prior"`) || strings.Contains(string(data), `"Key"`) {
		t.Fatalf("snapshot exposed private process baseline: %s", data)
	}
}

func TestShellForegroundRequiresTTYAndKnownPane(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	at := time.Unix(1_700_000_000, 0)
	fakeProc(t, root, 20, 10, 10, 1, "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00", "bash\x00", 0, 20, 4, 20)
	fakeProc(t, root, 21, 11, 10, 1, "", "btop\x00", 20, 21, 4, 20)        // Background job.
	fakeProc(t, root, 22, 12, 10, 1, "", "sleep\x0010\x00", 20, 22, 0, 22) // No controlling tty.
	s := []Session{{Name: "x", Kind: "shell", panes: []pane{{ID: 1, Focused: true, Command: "bash", Title: "user@host:~/code"}}}}
	populateSessionMetrics(root, store, s, func() time.Time { return at })
	if s[0].RunningCommand != "" || s[0].LastCommand != "" || !s[0].Metrics.Available ||
		s[0].Metrics.RAM != 3*uint64(os.Getpagesize()) {
		t.Fatalf("background process or prompt mistaken for running: %+v", s[0])
	}
}

func TestSessionMetricsPIDAndServerRestart(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const env = "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00"
	at := time.Unix(1_700_000_000, 0)
	fakeProc(t, root, 20, 10, 50, 100, "ZELLIJ_SESSION_NAME=x\x00", "zellij\x00--server\x00")
	fakeProc(t, root, 21, 11, 100, 2, env, "omp\x00")
	for i := range 2 {
		live := []Session{{Name: "x"}}
		populateSessionMetrics(root, store, live, func() time.Time { return at.Add(time.Duration(i*10) * time.Second) })
	}
	fakeProc(t, root, 21, 12, 5000, 3, env, "omp\x00") // Same PID, different process; no CPU delta.
	live := []Session{{Name: "x"}}
	populateSessionMetrics(root, store, live, func() time.Time { return at.Add(20 * time.Second) })
	if live[0].Metrics.CPUKnown || !live[0].Metrics.Available || live[0].Metrics.Samples != 3 {
		t.Fatalf("reused PID = %+v", live[0].Metrics)
	}
	fakeProc(t, root, 20, 30, 5, 100, "ZELLIJ_SESSION_NAME=x\x00", "zellij\x00--server\x00")
	live = []Session{{Name: "x"}}
	populateSessionMetrics(root, store, live, func() time.Time { return at.Add(30 * time.Second) })
	if live[0].Metrics.CPUKnown || live[0].Metrics.Samples != 1 || live[0].Metrics.SpanSeconds != 0 {
		t.Fatalf("recreated server retained old history: %+v", live[0].Metrics)
	}
	populateSessionMetrics(root, store, []Session{{Name: "x", Saved: true}}, func() time.Time { return at.Add(40 * time.Second) })
	data, err := os.ReadFile(filepath.Join(store, "metrics-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted metricHistory
	if err = json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Sessions) != 0 {
		t.Fatalf("retained saved session: %+v", persisted.Sessions)
	}
}

func TestSessionMetricsBoundedWindowAndUnavailable(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const env = "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00"
	at := time.Unix(1_700_000_000, 0)
	fakeProc(t, root, 20, 10, 50, 2, env, "omp\x00")
	for i := range 220 {
		live := []Session{{Name: "x"}}
		populateSessionMetrics(root, store, live, func() time.Time { return at.Add(time.Duration(i*10) * time.Second) })
		if i == 219 && (live[0].Metrics.Samples != 220 || len(live[0].Metrics.History) != 220 ||
			live[0].Metrics.SpanSeconds != 2190 || !live[0].Metrics.CPUKnown) {
			t.Fatalf("window = %+v", live[0].Metrics)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "20", "stat"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	live := []Session{{Name: "x"}}
	populateSessionMetrics(root, store, live, func() time.Time { return at.Add(2210 * time.Second) })
	if live[0].Metrics.Available || live[0].Metrics.CPUKnown || live[0].Metrics.RAM != 0 {
		t.Fatalf("broken proc stat claimed current telemetry: %+v", live[0].Metrics)
	}
	fakeProc(t, root, 20, 10, 100, 3, env, "omp\x00")
	populateSessionMetrics(root, store, live, func() time.Time { return at.Add(25 * time.Hour) })
	if !live[0].Metrics.Available || live[0].Metrics.CPUKnown || live[0].Metrics.Samples != 1 {
		t.Fatalf("expired window = %+v", live[0].Metrics)
	}
}

func TestConcurrentMetricCollectors(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	fakeProc(t, root, 20, 10, 50, 2, "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=0\x00", "omp\x00")
	at := time.Unix(1_700_000_000, 0)
	var counter atomic.Int64
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			populateSessionMetrics(root, store, []Session{{Name: "x"}}, func() time.Time { return at.Add(time.Duration(counter.Add(1)) * 10 * time.Second) })
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(store, "metrics-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state metricHistory
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions["x"].Samples) != 30 || len(state.Sessions["x"].Prior) != 1 {
		t.Fatalf("concurrent updates lost: %+v", state.Sessions["x"])
	}
}

func TestSessionMetricsUncertainDescendantAndProcessChanges(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const env = "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00"
	at := time.Unix(1_700_000_000, 0)
	fakeProc(t, root, 20, 10, 50, 100, "ZELLIJ_SESSION_NAME=x\x00", "zellij\x00--server\x00")
	fakeProc(t, root, 21, 11, 100, 2, env, "omp\x00", 20)
	s := []Session{{Name: "x"}}
	populateSessionMetrics(root, store, s, func() time.Time { return at })
	fakeProc(t, root, 22, 12, 50, 30, env, "helper\x00", 21)
	s = []Session{{Name: "x"}}
	populateSessionMetrics(root, store, s, func() time.Time { return at.Add(10 * time.Second) })
	if !s[0].Metrics.Available || s[0].Metrics.CPUKnown || s[0].Metrics.RAM != 32*uint64(os.Getpagesize()) {
		t.Fatalf("new child delta was falsely complete: %+v", s[0].Metrics)
	}
	if err := os.Chmod(filepath.Join(root, "22", "environ"), 0000); err != nil {
		t.Fatal(err)
	}
	s = []Session{{Name: "x"}}
	populateSessionMetrics(root, store, s, func() time.Time { return at.Add(20 * time.Second) })
	if !s[0].Metrics.Available || s[0].Metrics.RAM != 32*uint64(os.Getpagesize()) {
		t.Fatalf("same-user child with private environment lost parent attribution: %+v", s[0].Metrics)
	}
}

func TestSnapshotCacheKeepsNewestConcurrentResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newer := Snapshot{At: time.Unix(1_700_000_020, 0), Sessions: []Session{{Name: "current"}}}
	older := Snapshot{At: newer.At.Add(-10 * time.Second), Sessions: []Session{{Name: "stale"}}}
	if err := saveCache(newer); err != nil {
		t.Fatal(err)
	}
	if err := saveCache(older); err != nil {
		t.Fatal(err)
	}
	got := loadCache()
	if got.Sessions[0].Name != "current" || !got.At.Equal(newer.At) {
		t.Fatalf("older collector overwrote newer result: %+v", got)
	}
}

func writeMetricFixture(t *testing.T, store, name string, h *sessionHistory) {
	t.Helper()
	data, err := json.Marshal(metricHistory{Sessions: map[string]*sessionHistory{"x": h}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMetricsFullDayWindow(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	fakeProc(t, root, 20, 10, 0, 1, "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00", "omp\x00")
	now := time.Unix(1_700_000_000, 0)
	samples := []metricSample{
		{At: now.Add(-metricWindow - time.Second), CPU: 99, CPUKnown: true, RAM: 99},
		{At: now.Add(-24 * time.Hour), CPU: 10, CPUKnown: true, RAM: 10},
		{At: now.Add(-12 * time.Hour), CPU: 20, CPUKnown: true, RAM: 20},
		{At: now.Add(-6 * time.Hour), CPU: 30, CPUKnown: true, RAM: 30},
	}
	writeMetricFixture(t, store, "metrics-v3.json", &sessionHistory{Samples: samples})
	live := []Session{{Name: "x"}}
	populateSessionMetrics(root, store, live, func() time.Time { return now })
	m := live[0].Metrics
	if m.Samples != 4 || !m.History[0].At.Equal(samples[1].At) || !m.History[1].At.Equal(samples[2].At) ||
		!m.History[2].At.Equal(samples[3].At) || m.SpanSeconds != 24*60*60 ||
		m.CPUSamples != 3 || m.AvgCPU != 20 || m.MaxCPU != 30 ||
		m.AvgRAM != (60+uint64(os.Getpagesize()))/4 || m.MaxRAM != uint64(os.Getpagesize()) {
		t.Fatalf("full-day history and exact summary = %+v", m)
	}
	populateSessionMetrics(root, store, live, func() time.Time { return now.Add(time.Second) })
	m = live[0].Metrics
	if m.Samples != 3 || !m.History[0].At.Equal(samples[2].At) || m.CPUSamples != 2 ||
		m.AvgCPU != 25 || m.MaxCPU != 30 {
		t.Fatalf("expired boundary sample still contributes: %+v", m)
	}
	populateSessionMetrics(root, store, live, func() time.Time { return now.Add(metricWindow + time.Second) })
	if live[0].Metrics.Samples != 1 || live[0].Metrics.SpanSeconds != 0 {
		t.Fatalf("expired full-day history = %+v", live[0].Metrics)
	}
}

func TestSessionMetricsCapHoldsWholeDay(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	fakeProc(t, root, 20, 10, 0, 1, "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00", "omp\x00")
	now := time.Unix(1_700_000_000, 0)
	samples := make([]metricSample, metricLimit+20)
	for i := range samples {
		samples[i] = metricSample{At: now.Add(-time.Duration(len(samples)-i+1) * 7 * time.Second), RAM: uint64(i + 1)}
	}
	writeMetricFixture(t, store, "metrics-v3.json", &sessionHistory{Samples: samples})
	live := []Session{{Name: "x"}}
	populateSessionMetrics(root, store, live, func() time.Time { return now })
	m := live[0].Metrics
	if metricLimit < 24*60*60/8+1 || m.Samples != metricLimit || len(m.History) != metricLimit ||
		!m.History[0].At.Equal(samples[21].At) || !m.History[metricLimit-1].At.Equal(now) {
		t.Fatalf("cap discarded samples inside full-day window: count=%d first=%v last=%v", m.Samples, m.History[0].At, m.History[len(m.History)-1].At)
	}
}

func TestSessionMetricsMigratesLegacyAndIsolatesWriters(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const env = "ZELLIJ_SESSION_NAME=x\x00ZELLIJ_PANE_ID=1\x00"
	now := time.Unix(1_700_000_000, 0)
	fakeProc(t, root, 20, 10, 0, 1, "ZELLIJ_SESSION_NAME=x\x00", "zellij\x00--server\x00")
	fakeProc(t, root, 21, 11, 200, 2, env, "omp\x00")
	legacy := &sessionHistory{
		Server:  processKey{PID: 20, Start: 10},
		Last:    now.Add(-10 * time.Second),
		Prior:   []processUsage{{Key: processKey{PID: 21, Start: 11}, Ticks: 100}},
		Samples: []metricSample{{At: now.Add(-29 * time.Minute), CPU: 15, CPUKnown: true, RAM: 15}, {At: now.Add(-20 * time.Second), RAM: 20}},
	}
	writeMetricFixture(t, store, "metrics-v2.json", legacy)
	live := []Session{{Name: "x"}}
	populateSessionMetrics(root, store, live, func() time.Time { return now })
	m := live[0].Metrics
	if !m.CPUKnown || m.CPU != 10 || m.Samples != 3 || !m.History[0].At.Equal(legacy.Samples[0].At) ||
		m.CPUSamples != 2 || m.MaxCPU != 15 {
		t.Fatalf("migration lost samples or CPU baseline: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(store, "metrics-v3.lock")); err != nil {
		t.Fatal(err)
	}
	// A still-running v2 dashboard prunes its own file; v3 must not read it again.
	writeMetricFixture(t, store, "metrics-v2.json", &sessionHistory{Samples: []metricSample{{At: now, RAM: 999}}})
	fakeProc(t, root, 21, 11, 300, 2, env, "omp\x00")
	populateSessionMetrics(root, store, live, func() time.Time { return now.Add(10 * time.Second) })
	m = live[0].Metrics
	if !m.CPUKnown || m.CPU != 10 || m.Samples != 4 || !m.History[0].At.Equal(legacy.Samples[0].At) ||
		m.History[0].RAM != 15 || m.MaxRAM == 999 {
		t.Fatalf("legacy writer changed migrated v3 history: %+v", m)
	}
	populateSessionMetrics(root, store, live, func() time.Time { return now.Add(2 * time.Minute) })
	m = live[0].Metrics
	if m.Samples != 5 || !m.History[0].At.Equal(legacy.Samples[0].At) ||
		m.SpanSeconds <= 30*60 || m.MaxRAM == 999 {
		t.Fatalf("migrated sample vanished after v2's 30m retention: %+v", m)
	}
}

func TestSnapshotCacheMigratesAndIsolatesLegacyWriters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := stateRoot()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_000_000, 0)
	legacy := Snapshot{At: at, Sessions: []Session{{Name: "warm"}}}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadCache(); got.Sessions[0].Name != "warm" || !got.At.Equal(at) {
		t.Fatalf("cold v3 load lost warm v2 snapshot: %+v", got)
	}
	// An older dashboard may publish a later timestamp while v3 is first
	// being created; its v2 cache must not block the new format's first write.
	legacy.At = at.Add(time.Hour)
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	next := Snapshot{At: at.Add(time.Second), Sessions: []Session{{Name: "new"}}}
	if err := saveCache(next); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot-v3.lock")); err != nil {
		t.Fatal(err)
	}
	legacy.At = at.Add(time.Hour)
	legacy.Sessions[0].Name = "old-writer"
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveCache(Snapshot{At: at, Sessions: []Session{{Name: "late"}}}); err != nil {
		t.Fatal(err)
	}
	if got := loadCache(); got.Sessions[0].Name != "new" || !got.At.Equal(next.At) {
		t.Fatalf("older v3 collector or legacy writer overwrote snapshot: %+v", got)
	}
}
