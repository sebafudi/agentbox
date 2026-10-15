// agentbox-facts records machine inventory for the agent context, not the login hot path.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type request struct {
	key  string
	args []string
}

var requests = []request{
	{"failed", []string{"systemctl", "--failed", "--no-legend", "--plain"}},
	{"ufailed", []string{"systemctl", "--user", "--failed", "--no-legend", "--plain"}},
	{"cloudflared", []string{"systemctl", "is-active", "cloudflared"}},
	{"services", []string{"systemctl", "is-active", "sshd", "docker", "tailscaled", "cloudflared", "qemu-guest-agent", "systemd-networkd", "systemd-resolved"}},
	{"units", []string{"systemctl", "--user", "list-units", "--type=service", "--state=running", "--no-legend", "--plain"}},
	{"timers", []string{"systemctl", "--user", "list-timers", "--no-legend", "--plain"}},
	{"desktop", []string{"systemctl", "--user", "is-active", "xdesktop.target"}},
	{"tailscale", []string{"tailscale", "status", "--json"}},
	{"sockets", []string{"ss", "-ltnpH"}},
	{"route", []string{"ip", "-4", "route", "show", "default"}},
	{"agents", []string{"agent", "status"}},
	{"containers", []string{"docker", "ps", "--format", "{{.Names}} ({{.Image}}) {{.Ports}}"}},
	{"docker-disk", []string{"docker", "system", "df", "--format", "{{.Type}} {{.Size}}"}},
	{"sessions", []string{"zellij", "ls", "--no-formatting"}},
	{"omp-services", []string{"omp", "ps", "--all", "--json"}},
	{"mise-global", []string{"mise", "ls", "--global", "--json"}},
	{"omp-version", []string{"omp", "--version"}},
	{"mise-version", []string{"mise", "--version"}},
	{"git-version", []string{"git", "--version"}},
	{"docker-version", []string{"docker", "--version"}},
	{"chromium-version", []string{"chromium", "--version"}},
	{"zellij-version", []string{"zellij", "--version"}},
	{"tailscale-version", []string{"tailscale", "version"}},
	{"cloudflared-version", []string{"cloudflared", "--version"}},
	{"claude-version", []string{"claude", "--version"}},
}

func run(parent context.Context, args ...string) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	b, _ := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if ctx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func parallel() map[string]string {
	out := make(map[string]string, len(requests))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, r := range requests {
		wg.Add(1)
		go func(r request) {
			defer wg.Done()
			slots <- struct{}{}
			v := run(context.Background(), r.args...)
			<-slots
			mu.Lock()
			out[r.key] = v
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return out
}
func file(path string) string { b, _ := os.ReadFile(path); return string(b) }
func first(s string) string   { a := strings.SplitN(strings.TrimSpace(s), "\n", 2); return a[0] }
func words(s string) []string { return strings.Fields(s) }
func col(s string, n int) string {
	f := words(first(s))
	if len(f) > n {
		return f[n]
	}
	return ""
}
func units(s string) string {
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Fields(l)
		if len(f) > 0 {
			names = append(names, f[0])
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}
func mem() string {
	m := map[string]int64{}
	for _, line := range strings.Split(file("/proc/meminfo"), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			m[strings.TrimSuffix(f[0], ":")] = n / 1024
		}
	}
	return fmt.Sprintf("%d/%d MiB used (%d available), zram swap %d/%d MiB", m["MemTotal"]-m["MemFree"]-m["Buffers"]-m["Cached"]-m["SReclaimable"]+m["Shmem"], m["MemTotal"], m["MemAvailable"], m["SwapTotal"]-m["SwapFree"], m["SwapTotal"])
}
func disk() string {
	var s syscall.Statfs_t
	if syscall.Statfs("/", &s) != nil {
		return "unavailable"
	}
	used := (s.Blocks - s.Bfree) * uint64(s.Bsize)
	total := s.Blocks * uint64(s.Bsize)
	if total == 0 {
		return "unavailable"
	}
	// df uses available-to-user blocks in its percentage calculation.
	pct := 100 * float64(s.Blocks-s.Bfree) / float64(s.Blocks-s.Bfree+s.Bavail)
	return fmt.Sprintf("%.1fG / %.0fG (%.0f%%)", float64(used)/1073741824, float64(total)/1073741824, pct)
}
func uptime() string {
	sec, _ := strconv.ParseFloat(col(file("/proc/uptime"), 0), 64)
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%d days, %d hours, %d minutes", days, hours, mins)
	}
	return fmt.Sprintf("%d hours, %d minutes", hours, mins)
}
func lastUpgrade() string {
	latest := "never since install"
	for _, l := range strings.Split(file("/var/log/pacman.log"), "\n") {
		if strings.Contains(l, "starting full system upgrade") && len(l) > 17 {
			latest = l[1:17]
		}
	}
	return latest
}
func installDate() string {
	date := first(run(context.Background(), "stat", "-c", "%w", "/"))
	if len(date) >= 10 {
		return date[:10]
	}
	return "unknown"
}
func lan() string {
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Name == "ens18" {
			addrs, _ := iface.Addrs()
			for _, a := range addrs {
				if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil {
					return a.String()
				}
			}
		}
	}
	return "unavailable"
}
func gateway(s string) string {
	f := strings.Fields(s)
	for i, v := range f {
		if v == "via" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return "unavailable"
}
func hostnames() string {
	var names []string
	for _, line := range strings.Split(file("/etc/cloudflared/config.yml"), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "-" && f[1] == "hostname:" {
			names = append(names, f[2])
		}
	}
	return strings.Join(names, " ")
}
func sockets(s string) []string {
	seen := map[string]bool{}
	var a []string
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		addr := f[3]
		if strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "[::1]") || strings.HasPrefix(addr, "[fe80") || strings.HasPrefix(addr, "100.") || strings.HasPrefix(addr, "[fd7a:") || strings.HasSuffix(addr, ":5355") || strings.HasSuffix(addr, ":53") {
			continue
		}
		x := addr
		if len(f) > 5 {
			x += " " + f[5]
		}
		if !seen[x] {
			seen[x] = true
			a = append(a, x)
		}
	}
	sort.Strings(a)
	return a
}
func projects() []string {
	root := filepath.Join(os.Getenv("HOME"), "code")
	dirs, _ := os.ReadDir(root)
	var names []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		names = append(names, d.Name())
		// Group directories (no .git of their own) list their checkouts too; so does the agentbox repo.
		_, gitErr := os.Stat(filepath.Join(root, d.Name(), ".git"))
		if !strings.HasPrefix(d.Name(), ".") && (d.Name() == "agentbox" || gitErr != nil) {
			children, _ := os.ReadDir(filepath.Join(root, d.Name()))
			for _, child := range children {
				if child.IsDir() && !strings.HasPrefix(child.Name(), ".") {
					names = append(names, filepath.Join(d.Name(), child.Name()))
				}
			}
		}
	}
	var a []string
	for _, name := range names {
		path := filepath.Join(root, name)
		ctx := context.Background()
		top := run(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
		if top != "" && top != path {
			// A directory inside another checkout: tracked parts belong to the parent, ignored ones stand alone.
			if run(ctx, "git", "-C", top, "check-ignore", path) == "" {
				continue
			}
			top = ""
		}
		if top == "" {
			a = append(a, "- "+name+": not a git repo")
			continue
		}
		branch := run(ctx, "git", "-C", path, "branch", "--show-current")
		if branch == "" {
			branch = "detached"
		}
		status := run(ctx, "git", "-C", path, "status", "--porcelain")
		dirty := 0
		if status != "" {
			dirty = len(strings.Split(status, "\n"))
		}
		last := run(ctx, "git", "-C", path, "log", "-1", "--format=%cr")
		if last == "" {
			last = "n/a"
		}
		a = append(a, fmt.Sprintf("- %s: git %s, %d uncommitted change(s), last commit %s", name, branch, dirty, last))
	}
	if len(a) == 0 {
		return []string{"- none yet"}
	}
	return a
}
func describeTailscale(s string) (string, string) {
	var data struct {
		BackendState   string
		Self           struct{ TailscaleIPs []string }
		CurrentTailnet struct{ MagicDNSEnabled bool }
		Peer           map[string]struct {
			Online  bool
			DNSName string
		}
	}
	if json.Unmarshal([]byte(s), &data) != nil {
		return "unavailable", "none"
	}
	ip := "no IP"
	if len(data.Self.TailscaleIPs) > 0 {
		ip = data.Self.TailscaleIPs[0]
	}
	var peers []string
	for _, p := range data.Peer {
		if p.Online {
			peers = append(peers, strings.SplitN(p.DNSName, ".", 2)[0])
		}
	}
	sort.Strings(peers)
	if len(peers) == 0 {
		peers = []string{"none"}
	}
	return fmt.Sprintf("%s · %s · MagicDNS %t", data.BackendState, ip, data.CurrentTailnet.MagicDNSEnabled), strings.Join(peers, ", ")
}
func services(s string) string {
	names := []string{"sshd", "docker", "tailscaled", "cloudflared", "qemu-guest-agent", "systemd-networkd", "systemd-resolved"}
	states := strings.Fields(s)
	var a []string
	for i, n := range names {
		state := "unknown"
		if i < len(states) {
			state = states[i]
		}
		a = append(a, n+"="+state)
	}
	return strings.Join(a, " ")
}
func userUnits(s string) string {
	var a []string
	for _, l := range strings.Split(s, "\n") {
		n := col(l, 0)
		if n != "" && !strings.HasPrefix(n, "dbus") && n != "gpg-agent.service" && n != "keyboxd.service" {
			a = append(a, n)
		}
	}
	return strings.Join(a, " ")
}
func timers(s string) string {
	a := map[string]bool{}
	for _, w := range strings.Fields(s) {
		if strings.HasSuffix(w, ".timer") {
			a[w] = true
		}
	}
	var out []string
	for k := range a {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
func agentRuns(s string) string {
	var a []string
	lines := strings.Split(s, "\n")
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > 1 {
			a = append(a, f[0]+"="+f[1])
		}
	}
	if len(a) == 0 {
		return "none"
	}
	return strings.Join(a, " ")
}
func globalMise(s string) string {
	var data map[string][]struct{ Version string }
	if json.Unmarshal([]byte(s), &data) != nil {
		return "none (by design; install per project)"
	}
	var a []string
	for k, v := range data {
		if len(v) > 0 {
			a = append(a, k+"@"+v[0].Version)
		}
	}
	sort.Strings(a)
	if len(a) == 0 {
		return "none (by design; install per project)"
	}
	return strings.Join(a, " ")
}
func ompServices(s string) []string {
	var data []struct {
		Daemons []struct {
			Name   string
			ID     string
			State  string
			Status string
		}
	}
	if json.Unmarshal([]byte(s), &data) != nil {
		return nil
	}
	var a []string
	for _, app := range data {
		for _, d := range app.Daemons {
			n := d.Name
			if n == "" {
				n = d.ID
			}
			state := d.State
			if state == "" {
				state = d.Status
			}
			a = append(a, "  - "+n+" "+state)
		}
	}
	return a
}
func render(r map[string]string) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	load := words(file("/proc/loadavg"))
	for len(load) < 3 {
		load = append(load, "?")
	}
	osname := "Arch Linux"
	for _, l := range strings.Split(file("/etc/os-release"), "\n") {
		if strings.HasPrefix(l, "PRETTY_NAME=") {
			osname = strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), "\"")
		}
	}
	ts, peers := describeTailscale(r["tailscale"])
	p("# agentbox live state\n")
	p("Snapshot taken %s by `agentbox-facts` (15-minute timer plus watched local config changes; run `agentbox-facts` for an immediate snapshot). Runtime values can change between snapshots.\n", time.Now().Format("2006-01-02 15:04 MST"))
	p("## System")
	p("- Uptime: %s, load %s", uptime(), strings.Join(load[:3], " "))
	p("- Kernel: %s · %s", first(file("/proc/sys/kernel/osrelease")), osname)
	p("- Memory: %s.", mem())
	p("- Disk: %s on /", disk())
	p("- Last full `pacman -Syu`: %s (install: %s)", lastUpgrade(), installDate())
	p("- Failed units: system: %s; user: %s", units(r["failed"]), units(r["ufailed"]))
	p("\n## Network")
	p("- LAN: %s via %s · mDNS name agentbox.local", lan(), gateway(r["route"]))
	p("- Tailscale: %s", ts)
	p("- Tailscale peers online now: %s", peers)
	p("- Cloudflare tunnel (cloudflared): %s · public hostnames: %s", first(r["cloudflared"]), hostnames())
	p("- Listening TCP (non-loopback):")
	for _, a := range sockets(r["sockets"]) {
		p("  - %s", a)
	}
	p("\n## Services")
	p("- Core services: %s", services(r["services"]))
	p("- User units running: %s", userUnits(r["units"]))
	p("- User timers: %s", timers(r["timers"]))
	p("- Background agent runs (agent status): %s", agentRuns(r["agents"]))
	p("- Virtual desktop (xdesktop.target): %s", first(r["desktop"]))
	containers := r["containers"]
	if containers == "" {
		p("- Docker containers running: none")
	} else {
		p("- Docker containers running:")
		for _, l := range strings.Split(containers, "\n") {
			p("  - %s", l)
		}
	}
	p("- Docker disk: %s", strings.ReplaceAll(r["docker-disk"], "\n", ", "))
	p("- Zellij sessions:")
	if r["sessions"] == "" {
		p("  - none")
	} else {
		for _, l := range strings.Split(r["sessions"], "\n") {
			p("  - %s", l)
		}
	}
	if x := ompServices(r["omp-services"]); len(x) > 0 {
		p("- omp services:")
		for _, l := range x {
			p("%s", l)
		}
	}
	p("\n## Projects (~/code)")
	for _, l := range projects() {
		p("%s", l)
	}
	p("\n## Tool versions")
	p("- omp %s · mise %s · git %s · docker %s · chromium %s · zellij %s · tailscale %s · cloudflared %s", strings.TrimPrefix(first(r["omp-version"]), "omp/"), col(r["mise-version"], 0), col(r["git-version"], 2), strings.TrimSuffix(col(r["docker-version"], 2), ","), col(r["chromium-version"], 1), col(r["zellij-version"], 1), first(r["tailscale-version"]), col(r["cloudflared-version"], 2))
	if r["claude-version"] != "" {
		p("- Claude Code %s (native per-user installer)", col(r["claude-version"], 0))
	}
	p("- mise global runtimes: %s", globalMise(r["mise-global"]))
	return b.String()
}
func main() {
	out := os.Getenv("AGENTBOX_FACTS_OUT")
	if out == "" {
		out = filepath.Join(os.Getenv("HOME"), ".omp/agent/machine-state.md")
	}
	snapshot := render(parallel())
	dir := filepath.Dir(out)
	f, err := os.CreateTemp(dir, ".machine-state.*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(snapshot); err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) < 2 || os.Args[1] != "--quiet" {
		fmt.Print(snapshot)
	}
}
