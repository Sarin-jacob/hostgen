package main

import (
	"context"
	"encoding/json"
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
	if want := header + "10.0.0.1 a.x\n10.0.0.1 b.x\n"; got != want {
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
	f := &fakeDocker{containers: []map[string]any{}, status: map[string]int{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			json.NewEncoder(w).Encode(f.containers)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/restart"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/restart")
			f.restarts = append(f.restarts, name)
			code := f.status[name]
			if code == 0 {
				code = http.StatusNoContent
			}
			w.WriteHeader(code)
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

func lbl(m map[string]string) map[string]any { return map[string]any{"Labels": m} }

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
		{filepath.Join(dir, "lan", "traefik.hosts"), "192.168.1.33"},
		{filepath.Join(dir, "zt", "traefik.hosts"), "10.144.25.2"},
		{filepath.Join(dir, "ts", "traefik.hosts"), "100.101.188.115"},
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

func TestRestartFallbackAndRetry(t *testing.T) {
	sock, dk := startFakeDocker(t)
	targets := testTargets(t.TempDir())
	dk.setContainers([]map[string]any{lbl(map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`a.jell0.online`)"})})
	dk.status["ghost"] = http.StatusNotFound
	dk.status["dnsmasq-ts"] = http.StatusInternalServerError
	cfg := config{
		domain: "jell0.online", dockerSock: sock, targets: targets, interval: time.Second,
		restart: []string{"dnsmasq-lan", "dnsmasq-ts", "ghost"},
	}
	a := newApp(cfg)
	ctx := context.Background()

	// first pass: three files changed, but every container is restarted once
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dk.restartLog(); !reflect.DeepEqual(got, []string{"dnsmasq-lan", "dnsmasq-ts", "ghost"}) {
		t.Fatalf("restarts: %v", got)
	}
	if len(a.pending) != 1 || !a.pending["dnsmasq-ts"] {
		t.Fatalf("pending: %v", a.pending)
	}

	// the failed one is retried alone, then the queue is empty
	dk.status["dnsmasq-ts"] = 0
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dk.restartLog()[3:]; !reflect.DeepEqual(got, []string{"dnsmasq-ts"}) {
		t.Fatalf("retry restarted %v", got)
	}
	if len(a.pending) != 0 {
		t.Fatalf("pending: %v", a.pending)
	}

	// nothing changed: nothing restarted
	if err := a.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(dk.restartLog()); n != 4 {
		t.Fatalf("restarts after idle pass: %d", n)
	}
}
