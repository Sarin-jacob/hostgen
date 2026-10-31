package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTraefikHosts(t *testing.T) {
	sets := []map[string]string{
		{"traefik.enable": "true", "traefik.http.routers.api.rule": "Host(`tfk.jell0.online`)"},
		{"traefik.enable": "True", "traefik.http.routers.m.rule": "Host(`a.jell0.online`, `b.jell0.online`) && PathPrefix(`/x`)"},
		{"traefik.enable": "true", "traefik.http.routers.alt.rule": "Host(`c.jell0.online`) || Host(\"D.JELL0.ONLINE.\")"},
		{"traefik.enable": "true", "traefik.http.routers.err.rule": "HostRegexp(`.+`)"},
		{"traefik.enable": "true", "traefik.http.routers.err2.rule": "HostRegexp(`^.+\\.jell0\\.online$`)"},
		{"traefik.enable": "false", "traefik.http.routers.off.rule": "Host(`off.jell0.online`)"},
		{"traefik.enable": "true", "traefik.http.routers.evil.rule": "Host(`x.jell0.online\n1.2.3.4 evil.com`)"},
		{"traefik.http.routers.nolabel.rule": "Host(`nolabel.jell0.online`)"},
		{"traefik.enable": "true", "traefik.http.services.s.loadbalancer.server.port": "80"},
		nil,
	}
	got := selectHosts(traefikHosts(sets, false), "jell0.online", nil)
	want := []string{"a.jell0.online", "b.jell0.online", "c.jell0.online", "d.jell0.online", "tfk.jell0.online"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exposedByDefault=false: got %v want %v", got, want)
	}
	got = selectHosts(traefikHosts(sets, true), "jell0.online", nil)
	want = []string{"a.jell0.online", "b.jell0.online", "c.jell0.online", "d.jell0.online", "nolabel.jell0.online", "tfk.jell0.online"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exposedByDefault=true: got %v want %v", got, want)
	}
}

func TestSelectHosts(t *testing.T) {
	raw := map[string]bool{
		"a.jell0.online": true, "jell0.online": true, "x.example.com": true,
		"notjell0.online": true, "ntfy.jell0.online": true, "deep.sub.jell0.online": true,
	}
	got := selectHosts(raw, "jell0.online", map[string]bool{"ntfy.jell0.online": true})
	want := []string{"a.jell0.online", "deep.sub.jell0.online", "jell0.online"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRender(t *testing.T) {
	got := render("10.0.0.1", []string{"a.x", "b.x"})
	want := header +
		"host-record=a.x,10.0.0.1\nlocal=/a.x/\n" +
		"host-record=b.x,10.0.0.1\nlocal=/b.x/\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if render("10.0.0.1", nil) != header {
		t.Fatal("empty host list should render only the header")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("TARGETS", "/out/lan/h=192.168.1.33, /out/ts/h=100.101.188.115")
	t.Setenv("EXCLUDE", "NTFY.jell0.online.")
	t.Setenv("RESTART", "a, b")
	t.Setenv("EXPOSED_BY_DEFAULT", "")
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.targets) != 2 || c.targets[1] != (target{"/out/ts/h", "100.101.188.115"}) {
		t.Fatalf("targets: %+v", c.targets)
	}
	if !c.exclude["ntfy.jell0.online"] || !reflect.DeepEqual(c.restart, []string{"a", "b"}) {
		t.Fatalf("exclude=%v restart=%v", c.exclude, c.restart)
	}
	if c.exposedAll {
		t.Fatal("EXPOSED_BY_DEFAULT should default to false")
	}
	if c.interval != 5*time.Second || c.domain != "jell0.online" {
		t.Fatalf("defaults: %v %v", c.interval, c.domain)
	}

	for _, bad := range []string{"", "/out/x", "=1.2.3.4", "/out/x=notanip"} {
		t.Setenv("TARGETS", bad)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("TARGETS=%q should be rejected", bad)
		}
	}
}

// ---------------------------------------------------------------- fake docker

type fakeDocker struct {
	mu         sync.Mutex
	containers []map[string]any
	restarts   []string
	status     map[string]int
	inspect    map[string]map[string]any
	failN      int
}

func (f *fakeDocker) setStatus(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[name] = code
}

// setInspect stores what GET /containers/<name>/json returns.
func (f *fakeDocker) setInspect(name, id, status string, started time.Time, netMode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspect[name] = map[string]any{
		"Id":         id,
		"Name":       "/" + name,
		"State":      map[string]any{"Status": status, "StartedAt": started.Format(time.RFC3339Nano)},
		"HostConfig": map[string]any{"NetworkMode": netMode},
	}
}

func (f *fakeDocker) setContainers(c []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = c
}

func (f *fakeDocker) restartLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.restarts...)
}

func startFakeDocker(t *testing.T) (string, *fakeDocker) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{containers: []map[string]any{}, status: map[string]int{}, inspect: map[string]map[string]any{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			if r.URL.Query().Get("all") != "1" {
				http.Error(w, "test fake: expected all=1", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(f.containers)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
			info, ok := f.inspect[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(info)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/restart"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/restart")
			f.restarts = append(f.restarts, name)
			code := f.status[name]
			if code == 0 {
				code = http.StatusNoContent
			}
			if code == http.StatusNoContent {
				if info, ok := f.inspect[name]; ok {
					info["State"].(map[string]any)["StartedAt"] = time.Now().Format(time.RFC3339Nano)
				}
			}
			w.WriteHeader(code)
			if code == http.StatusInternalServerError {
				// like Docker: a fresh endpoint id in every error message
				f.failN++
				fmt.Fprintf(w, `{"message":"failed to set up endpoint (%064x): cannot assign requested address"}`, f.failN)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return sock, f
}

func lbl(m map[string]string) map[string]any { return map[string]any{"Labels": m, "State": "running"} }

func stopped(m map[string]string) map[string]any {
	return map[string]any{"Labels": m, "State": "exited"}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testTargets(dir string) []target {
	return []target{
		{filepath.Join(dir, "lan", "traefik.conf"), "192.168.1.33"},
		{filepath.Join(dir, "zt", "traefik.conf"), "10.144.25.2"},
		{filepath.Join(dir, "ts", "traefik.conf"), "100.101.188.115"},
	}
}

func checkAll(t *testing.T, targets []target, hosts ...string) {
	t.Helper()
	for _, tg := range targets {
		if got, want := readFile(t, tg.path), render(tg.ip, hosts); got != want {
			t.Fatalf("%s:\n%s\nwant:\n%s", tg.path, got, want)
		}
	}
}

// ---------------------------------------------------------------- end to end

func TestTickLifecycle(t *testing.T) {
	sock, dk := startFakeDocker(t)
	targets := testTargets(t.TempDir())
	cfg := config{
		domain:     "jell0.online",
		dockerSock: sock,
		targets:    targets,
		exclude:    map[string]bool{"ntfy.jell0.online": true},
		interval:   time.Second,
	}
	base := []map[string]any{
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.api.rule": "Host(`tfk.jell0.online`)"}),
		// the catch-all error page router must not pull every name to Traefik
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.error-router.rule": "HostRegexp(`.+`)"}),
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.app.rule": "Host(`a.jell0.online`, `b.jell0.online`)"}),
		// no traefik.enable=true: Traefik does not route it, so it is not published
		lbl(map[string]string{"traefik.http.routers.x.rule": "Host(`notrouted.jell0.online`)"}),
		// excluded: keeps resolving through the upstream although a label exists
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.ntfy.rule": "Host(`ntfy.jell0.online`)"}),
		lbl(map[string]string{"traefik.enable": "false", "traefik.http.routers.off.rule": "Host(`off.jell0.online`)"}),
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.other.rule": "Host(`example.com`)"}),
		lbl(nil),
	}
	dk.setContainers(base)
	a := newApp(cfg)
	ctx := context.Background()

	// 1. files appear, one per target, each with its own IP
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	checkAll(t, targets, "a.jell0.online", "b.jell0.online", "tfk.jell0.online")
	for _, tg := range targets {
		if m, _ := filepath.Glob(filepath.Join(filepath.Dir(tg.path), ".*")); len(m) != 0 {
			t.Fatalf("temp files left behind: %v", m)
		}
	}

	// 2. unchanged input: no rewrite
	st1, _ := os.Stat(targets[0].path)
	time.Sleep(20 * time.Millisecond)
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(targets[0].path)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("file rewritten although content is identical")
	}

	// 3. a new service shows up
	withNew := append(append([]map[string]any(nil), base...),
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.n.rule": "Host(`new.jell0.online`)"}))
	dk.setContainers(withNew)
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	checkAll(t, targets, "a.jell0.online", "b.jell0.online", "new.jell0.online", "tfk.jell0.online")

	// 4. it goes away again
	dk.setContainers(base)
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	checkAll(t, targets, "a.jell0.online", "b.jell0.online", "tfk.jell0.online")

	// 5. restart is off by default
	if n := len(dk.restartLog()); n != 0 {
		t.Fatalf("unexpected restarts: %d", n)
	}

	// 6. Docker unreachable: error, files untouched
	a.dk = newDocker("/nonexistent/docker.sock")
	dk.setContainers(nil)
	if err := a.tick(ctx); err == nil {
		t.Fatal("expected an error")
	}
	checkAll(t, targets, "a.jell0.online", "b.jell0.online", "tfk.jell0.online")
}

func TestEmptyListWritesHeaderOnly(t *testing.T) {
	sock, dk := startFakeDocker(t)
	targets := testTargets(t.TempDir())
	dk.setContainers([]map[string]any{lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.e.rule": "HostRegexp(`.+`)"})})
	a := newApp(config{domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second})
	if err := a.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkAll(t, targets)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestRestartBackoff(t *testing.T) {
	sock, dk := startFakeDocker(t)
	logs := captureLog(t)
	targets := testTargets(t.TempDir())
	dk.setContainers([]map[string]any{lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`a.jell0.online`)"})})
	dk.setStatus("ghost", http.StatusNotFound)
	dk.setStatus("dnsmasq-lan", http.StatusInternalServerError)
	cfg := config{
		domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second,
		restart: []string{"dnsmasq-lan", "dnsmasq-ts", "ghost"},
	}
	a := newApp(cfg)
	clk := &clock{t: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	a.now = clk.now
	ctx := context.Background()
	tick := func() {
		t.Helper()
		if err := a.tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	attempts := func() int {
		n := 0
		for _, r := range dk.restartLog() {
			if r == "dnsmasq-lan" {
				n++
			}
		}
		return n
	}

	// first pass: three files changed, every container is restarted once
	tick()
	if got := dk.restartLog(); !reflect.DeepEqual(got, []string{"dnsmasq-lan", "dnsmasq-ts", "ghost"}) {
		t.Fatalf("restarts: %v", got)
	}
	if len(a.pending) != 1 || a.pending["dnsmasq-lan"] == nil {
		t.Fatalf("pending: %v", a.pending)
	}

	// retries back off: due at +5s, then +10s more, then +20s more
	want := []struct {
		at       time.Duration
		attempts int
	}{
		{1 * time.Second, 1}, {4 * time.Second, 1}, {5 * time.Second, 2},
		{10 * time.Second, 2}, {15 * time.Second, 3},
		{30 * time.Second, 3}, {35 * time.Second, 4},
	}
	start := clk.t
	for _, w := range want {
		clk.t = start.Add(w.at)
		tick()
		if got := attempts(); got != w.attempts {
			t.Fatalf("at +%s: %d attempts, want %d", w.at, got, w.attempts)
		}
	}

	// the delay is capped
	a.pending["dnsmasq-lan"].delay = 4 * time.Minute
	clk.t = a.pending["dnsmasq-lan"].next
	tick()
	if d := a.pending["dnsmasq-lan"].delay; d != maxBackoff {
		t.Fatalf("delay %s, want cap %s", d, maxBackoff)
	}

	// four failures with different endpoint ids, one log line
	if n := strings.Count(logs.String(), "cannot assign requested address"); n != 1 {
		t.Fatalf("error logged %d times, want 1:\n%s", n, logs.String())
	}

	// recovery: next due retry succeeds and the queue empties
	dk.setStatus("dnsmasq-lan", 0)
	clk.t = a.pending["dnsmasq-lan"].next
	tick()
	if len(a.pending) != 0 {
		t.Fatalf("pending after recovery: %v", a.pending)
	}
	if !strings.Contains(logs.String(), "restarted dnsmasq-lan after earlier failures") {
		t.Fatalf("missing recovery line:\n%s", logs.String())
	}

	// idle pass: nothing restarted
	before := len(dk.restartLog())
	clk.advance(time.Hour)
	tick()
	if n := len(dk.restartLog()); n != before {
		t.Fatalf("restarts after idle pass: %d, want %d", n, before)
	}
}

func TestNetnsPairs(t *testing.T) {
	sock, dk := startFakeDocker(t)
	logs := captureLog(t)
	targets := testTargets(t.TempDir())
	cfg := config{
		domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second,
		pairs: []pair{{"tailscale_proxy", "traefik"}},
	}
	a := newApp(cfg)
	ctx := context.Background()
	tick := func() {
		t.Helper()
		if err := a.tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	traefikRestarts := func() int {
		n := 0
		for _, r := range dk.restartLog() {
			if r == "traefik" {
				n++
			}
		}
		return n
	}
	now := time.Now()

	// 1. healthy: traefik started after tailscale, nothing to do
	dk.setInspect("tailscale_proxy", "P1", "running", now.Add(-17*time.Hour), "default")
	dk.setInspect("traefik", "T1", "running", now.Add(-17*time.Hour+time.Second), "container:P1")
	tick()
	if n := traefikRestarts(); n != 0 {
		t.Fatalf("healthy pair restarted traefik %d times", n)
	}

	// 2. tailscale restarted (same id, newer start): traefik restarted once,
	// and not again once it is newer
	dk.setInspect("tailscale_proxy", "P1", "running", now.Add(-4*time.Hour), "default")
	tick()
	tick()
	tick()
	if n := traefikRestarts(); n != 1 {
		t.Fatalf("traefik restarts after tailscale restart: %d, want 1", n)
	}

	// 3. tailscale crash looping: left alone
	dk.setInspect("tailscale_proxy", "P1", "restarting", time.Now().Add(time.Second), "default")
	tick()
	if n := traefikRestarts(); n != 1 {
		t.Fatalf("restarted traefik while tailscale was restarting")
	}

	// 4. tailscale recreated (new id): no restart, one warning
	dk.setInspect("tailscale_proxy", "P2", "running", time.Now().Add(time.Second), "default")
	tick()
	tick()
	if n := traefikRestarts(); n != 1 {
		t.Fatalf("restarted traefik although it points at a removed container")
	}
	if n := strings.Count(logs.String(), "attached to an old tailscale_proxy container"); n != 1 {
		t.Fatalf("recreate warning logged %d times, want 1", n)
	}

	// 5. network mode by name instead of id is accepted
	dk.setInspect("traefik", "T1", "running", now.Add(-time.Hour), "container:tailscale_proxy")
	dk.setInspect("tailscale_proxy", "P3", "running", now, "default")
	tick()
	if n := traefikRestarts(); n != 2 {
		t.Fatalf("name based network mode: %d traefik restarts, want 2", n)
	}

	// 6. one side missing: nothing, no error
	b := newApp(config{domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second,
		pairs: []pair{{"ghost", "traefik"}, {"tailscale_proxy", "ghost"}}})
	if err := b.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := traefikRestarts(); n != 2 {
		t.Fatalf("missing container caused a restart")
	}
}

func TestLoadConfigPairs(t *testing.T) {
	t.Setenv("TARGETS", "/out/h=10.0.0.1")
	t.Setenv("NETNS_PAIRS", " tailscale_proxy:traefik , a:b")
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.pairs, []pair{{"tailscale_proxy", "traefik"}, {"a", "b"}}) {
		t.Fatalf("pairs: %+v", c.pairs)
	}
	for _, bad := range []string{"novalue", ":b", "a:", "a:a"} {
		t.Setenv("NETNS_PAIRS", bad)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("NETNS_PAIRS=%q should be rejected", bad)
		}
	}
}

func TestStoppedContainers(t *testing.T) {
	sock, dk := startFakeDocker(t)
	targets := testTargets(t.TempDir())
	dk.setContainers([]map[string]any{
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.up.rule": "Host(`up.jell0.online`)"}),
		// Sablier style: stopped, but Traefik keeps the router
		stopped(map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.bin.rule":        "Host(`bin.jell0.online`)",
			"traefik.http.routers.bin.tls":         "true",
			"traefik.docker.allownonrunning":       "true",
			"traefik.http.routers.bin.middlewares": "bin-sablier",
		}),
		// stopped without the label: Traefik drops it, so must we
		stopped(map[string]string{"traefik.enable": "true", "traefik.http.routers.old.rule": "Host(`old.jell0.online`)"}),
		// created but never started, with the label
		map[string]any{"State": "created", "Labels": map[string]string{
			"traefik.enable": "true", "traefik.docker.allownonrunning": "True",
			"traefik.http.routers.c.rule": "Host(`created.jell0.online`)",
		}},
		// label present but traefik.enable=false still wins
		stopped(map[string]string{"traefik.enable": "false", "traefik.docker.allownonrunning": "true",
			"traefik.http.routers.off.rule": "Host(`off.jell0.online`)"}),
	})
	a := newApp(config{domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second})
	if err := a.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkAll(t, targets, "bin.jell0.online", "created.jell0.online", "up.jell0.online")
}

// ---------------------------------------------------------------- serve and source

func getHosts(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hosts", nil))
	return rec.Code, rec.Body.String()
}

func TestServe(t *testing.T) {
	sock, dk := startFakeDocker(t)
	targets := testTargets(t.TempDir())
	a := newApp(config{domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second})
	ctx := context.Background()

	// nothing read yet: 503, so a backup never mistakes it for an empty list
	if code, _ := getHosts(t, a); code != http.StatusServiceUnavailable {
		t.Fatalf("before first read: %d, want 503", code)
	}

	// an empty list after a good read is served as such
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := getHosts(t, a); code != http.StatusOK || body != strings.TrimSpace(header)+"\n" {
		t.Fatalf("empty list: %d %q", code, body)
	}

	dk.setContainers([]map[string]any{
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`b.jell0.online`, `a.jell0.online`)"}),
	})
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(header) + "\na.jell0.online\nb.jell0.online\n"
	if code, body := getHosts(t, a); code != http.StatusOK || body != want {
		t.Fatalf("got %d %q, want %q", code, body, want)
	}

	// Docker down: the last good list stays available
	a.dk = newDocker("/nonexistent/docker.sock")
	if err := a.tick(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if code, body := getHosts(t, a); code != http.StatusOK || body != want {
		t.Fatalf("after Docker failure: %d %q", code, body)
	}

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other path: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hosts", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

// fakeSource serves a fixed response at /hosts.
type fakeSource struct {
	mu   sync.Mutex
	code int
	body string
}

func (f *fakeSource) set(code int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code, f.body = code, body
}

func (f *fakeSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.WriteHeader(f.code)
	io.WriteString(w, f.body)
}

func TestSource(t *testing.T) {
	sock, dk := startFakeDocker(t)
	src := &fakeSource{code: http.StatusOK}
	srv := httptest.NewServer(src)
	t.Cleanup(srv.Close)
	tg := []target{{filepath.Join(t.TempDir(), "traefik.conf"), "100.101.188.115"}}
	// Docker labels must be ignored in SOURCE mode
	dk.setContainers([]map[string]any{
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.l.rule": "Host(`local.jell0.online`)"}),
	})
	a := newApp(config{
		domain: "jell0.online", dockerSock: sock, targets: tg, interval: time.Second,
		exclude: map[string]bool{"ex.jell0.online": true},
		source:  srv.URL + "/hosts", restart: []string{"dnsmasq-backup"},
	})
	ctx := context.Background()

	src.set(http.StatusOK, "# generated by hostgen\nB.jell0.online.\n\na.jell0.online\nex.jell0.online\nother.example.com\n")
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	checkAll(t, tg, "a.jell0.online", "b.jell0.online")
	if got := dk.restartLog(); !reflect.DeepEqual(got, []string{"dnsmasq-backup"}) {
		t.Fatalf("restarts: %v", got)
	}

	// anything wrong with the source: error, file untouched, no restart
	for _, bad := range []struct {
		code int
		body string
	}{
		{http.StatusServiceUnavailable, "no successful read yet\n"},
		{http.StatusOK, "a.jell0.online\nx.jell0.online\n1.2.3.4 evil.com\n"},
		{http.StatusOK, "host-record=evil.jell0.online,6.6.6.6\n"},
		{http.StatusNotFound, ""},
	} {
		src.set(bad.code, bad.body)
		if err := a.tick(ctx); err == nil {
			t.Fatalf("%d %q: expected an error", bad.code, bad.body)
		}
		checkAll(t, tg, "a.jell0.online", "b.jell0.online")
	}
	srv.Close()
	if err := a.tick(ctx); err == nil {
		t.Fatal("source down: expected an error")
	}
	checkAll(t, tg, "a.jell0.online", "b.jell0.online")
	if n := len(dk.restartLog()); n != 1 {
		t.Fatalf("restarts while the source was bad: %d, want 1", n)
	}
}

func TestSourceDownStillWatchesNetns(t *testing.T) {
	sock, dk := startFakeDocker(t)
	tg := []target{{filepath.Join(t.TempDir(), "traefik.conf"), "100.101.188.115"}}
	a := newApp(config{
		domain: "jell0.online", dockerSock: sock, targets: tg, interval: time.Second,
		source: "http://127.0.0.1:1/hosts",
		pairs:  []pair{{"ts", "dnsmasq"}},
	})
	now := time.Now()
	dk.setInspect("ts", "P1", "running", now, "default")
	dk.setInspect("dnsmasq", "D1", "running", now.Add(-time.Hour), "container:P1")
	if err := a.tick(context.Background()); err == nil {
		t.Fatal("expected a source error")
	}
	if got := dk.restartLog(); !reflect.DeepEqual(got, []string{"dnsmasq"}) {
		t.Fatalf("restarts: %v, want dnsmasq restarted although the source is down", got)
	}
}

// TestPrimaryToBackup chains a primary serving /hosts to a backup reading it.
func TestPrimaryToBackup(t *testing.T) {
	sock, dk := startFakeDocker(t)
	dk.setContainers([]map[string]any{
		lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`lfs.jell0.online`)"}),
	})
	primary := newApp(config{domain: "jell0.online", dockerSock: sock, targets: testTargets(t.TempDir()), interval: time.Second})
	srv := httptest.NewServer(primary)
	t.Cleanup(srv.Close)
	tg := []target{{filepath.Join(t.TempDir(), "traefik.conf"), "100.101.188.115"}}
	backup := newApp(config{domain: "jell0.online", dockerSock: sock, targets: tg, interval: time.Second, source: srv.URL + "/hosts"})
	ctx := context.Background()

	// primary not ready yet: backup writes nothing
	if err := backup.tick(ctx); err == nil {
		t.Fatal("expected an error before the primary has read Docker")
	}
	if _, err := os.Stat(tg[0].path); !os.IsNotExist(err) {
		t.Fatalf("backup wrote a file from a primary that was not ready: %v", err)
	}
	if err := primary.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := backup.tick(ctx); err != nil {
		t.Fatal(err)
	}
	checkAll(t, tg, "lfs.jell0.online")
}

func TestLoadConfigSource(t *testing.T) {
	t.Setenv("TARGETS", "/out/h=100.101.188.115")
	t.Setenv("SOURCE", "http://100.111.232.103:8053/hosts")
	t.Setenv("SERVE", ":8053")
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.source != "http://100.111.232.103:8053/hosts" || c.serve != ":8053" {
		t.Fatalf("source=%q serve=%q", c.source, c.serve)
	}
	for _, bad := range []string{"100.111.232.103:8053/hosts", "ftp://x/hosts", "http:///hosts"} {
		t.Setenv("SOURCE", bad)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("SOURCE=%q should be rejected", bad)
		}
	}
}
