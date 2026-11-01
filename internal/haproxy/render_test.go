package haproxy_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/haproxy/haproxytest"
)

func load(t *testing.T, path string) *config.File {
	t.Helper()
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// haproxyCheck returns the argv that validates a config with a real HAProxy,
// or skips. CI sets SHIFTGATE_REQUIRE_HAPROXY=1 so a missing binary fails.
func haproxyCheck(t *testing.T) []string {
	t.Helper()
	if p, err := exec.LookPath("haproxy"); err == nil {
		return []string{p, "-c", "-f", "{config}"}
	}
	if os.Getenv("SHIFTGATE_REQUIRE_HAPROXY") == "1" {
		t.Fatal("SHIFTGATE_REQUIRE_HAPROXY=1 but haproxy is not on PATH")
	}
	t.Skip("haproxy not installed (CI installs it)")
	return nil
}

func TestRenderEnablesOnlyActiveColour(t *testing.T) {
	f := load(t, "../../examples/services.yaml")
	out, err := haproxy.Render(f, map[string]config.Color{"web": config.Green}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"stats socket /run/haproxy/admin.sock mode 600 level admin",
		"frontend ft_web\n    mode http\n    bind 127.0.0.1:8080\n    bind 192.0.2.10:8080\n    default_backend bk_web",
		"option httpchk GET /healthz\n    http-check expect status 200",
		"server blue 127.0.0.1:18080 check disabled\n",
		"server green 127.0.0.1:18081 check\n",
		// never deployed: both colours disabled
		"server blue 127.0.0.1:19000 check disabled\n    server green 127.0.0.1:19001 check disabled\n",
		"backend bk_worker_api\n    mode tcp\n    default-server",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("render lacks %q:\n%s", want, s)
		}
	}
}

func TestRenderNeverRoundsDurationToZero(t *testing.T) {
	f := load(t, "../../examples/services.yaml")
	f.HAProxy.Timeouts.Connect = config.Duration(500 * time.Microsecond)
	f.HAProxy.Timeouts.Client = config.Duration(1500 * time.Millisecond)
	out, err := haproxy.Render(f, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"timeout connect 1ms\n", "timeout client 1500ms\n"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("render lacks %q", want)
		}
	}
}

// Acceptance 5: render refuses 0.0.0.0 without the explicit flag.
func TestRenderRefusesWildcardWithoutFlag(t *testing.T) {
	f := load(t, "../../examples/services.yaml")
	f.Services[0].Binds = []string{"0.0.0.0:8080"}
	if _, err := haproxy.Render(f, nil, false); !errors.Is(err, haproxy.ErrWildcardBind) {
		t.Fatalf("want ErrWildcardBind, got %v", err)
	}
	f.Services[0].Binds = []string{"[::]:8080"}
	if _, err := haproxy.Render(f, nil, false); !errors.Is(err, haproxy.ErrWildcardBind) {
		t.Fatalf("IPv6 wildcard: want ErrWildcardBind, got %v", err)
	}
	out, err := haproxy.Render(f, nil, true)
	if err != nil || !strings.Contains(string(out), "bind [::]:8080") {
		t.Fatalf("explicit flag should allow the wildcard: %v", err)
	}
	f.HAProxy.AllowWildcardBind = true
	if _, err := haproxy.Render(f, nil, false); err != nil {
		t.Fatalf("allow_wildcard_bind in services.yaml should allow: %v", err)
	}
}

func TestRenderRefusesBindOutsideAllowlist(t *testing.T) {
	f := load(t, "../../examples/services.yaml")
	f.Services[0].Binds = []string{"203.0.113.9:8080"} // edited after validation
	if _, err := haproxy.Render(f, nil, true); err == nil || !strings.Contains(err.Error(), "allowed_binds") {
		t.Fatalf("want allowlist refusal even with the wildcard flag, got %v", err)
	}
}

// Acceptance 5: the rendered config passes `haproxy -c`.
func TestRenderedConfigPassesHAProxyCheck(t *testing.T) {
	argv := haproxyCheck(t)
	for _, ex := range []string{"../../examples/services.yaml", "../../examples/adopter-ncp.services.yaml"} {
		t.Run(filepath.Base(ex), func(t *testing.T) {
			f := load(t, ex)
			dir := t.TempDir()
			f.HAProxy.Config = filepath.Join(dir, "haproxy.cfg")
			f.HAProxy.Check = argv
			f.HAProxy.SocketInConfig = filepath.Join(dir, "admin.sock")
			enabled := map[string]config.Color{}
			for _, s := range f.Services {
				if !s.Static() {
					enabled[s.Name] = config.Blue
				}
			}
			data, err := haproxy.Render(f, enabled, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := haproxy.Install(context.Background(), f, data); err != nil {
				t.Fatalf("haproxy -c rejected the rendered config: %v\n%s", err, data)
			}
		})
	}
}

func TestInstallRejectsInvalidAndKeepsCurrent(t *testing.T) {
	f := load(t, "../../examples/services.yaml")
	dir := t.TempDir()
	f.HAProxy.Config = filepath.Join(dir, "haproxy.cfg")
	if err := os.WriteFile(f.HAProxy.Config, []byte("current\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.HAProxy.Check = []string{"sh", "-c", "exit 1", "check", "{config}"}
	if err := haproxy.Install(context.Background(), f, []byte("bad\n")); err == nil {
		t.Fatal("a failing check must refuse the install")
	}
	if b, _ := os.ReadFile(f.HAProxy.Config); string(b) != "current\n" {
		t.Fatalf("live config changed after a failed check: %q", b)
	}
	f.HAProxy.Check = []string{"sh", "-c", "test -s \"$1\"", "check", "{config}"}
	st, _ := os.Stat(f.HAProxy.Config)
	if err := haproxy.Install(context.Background(), f, []byte("next\n")); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(f.HAProxy.Config)
	if !os.SameFile(st, st2) {
		t.Fatal("install must keep the inode of an existing file (bind mounts)")
	}
	if b, _ := os.ReadFile(f.HAProxy.Config + ".previous"); string(b) != "current\n" {
		t.Fatalf(".previous backup = %q", b)
	}
	// A live file that fails the check (e.g. left partial by a crash) must
	// not replace the last known-good backup.
	if err := os.WriteFile(f.HAProxy.Config, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.HAProxy.Check = []string{"sh", "-c", "grep -q next \"$1\" || grep -q good \"$1\"", "check", "{config}"}
	if err := haproxy.Install(context.Background(), f, []byte("good\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.HAProxy.Config + ".previous"); string(b) != "current\n" {
		t.Fatalf("invalid live file overwrote the known-good backup: %q", b)
	}
	if b, _ := os.ReadFile(f.HAProxy.Config); string(b) != "good\n" {
		t.Fatalf("live config = %q", b)
	}
}

// The adopter layout renders the same proxies, binds and checks as its
// hand-written template (docs/design.md mapping table).
func TestAdopterLayoutRendersEquivalentProxies(t *testing.T) {
	f := load(t, "../../examples/adopter-ncp.services.yaml")
	out, err := haproxy.Render(f, map[string]config.Color{"api": config.Blue, "mcp": config.Green}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"log stdout format raw local0", "master-worker",
		"timeout client 1h", "timeout server 1h", "timeout tunnel 1h", "timeout http-request 10s", "timeout connect 5s",
		"bind 127.0.0.1:8000\n    bind 192.0.2.10:8000\n    default_backend bk_api",
		"bind 127.0.0.1:8765\n    bind 192.0.2.10:8765\n    default_backend bk_mcp",
		"backend bk_api\n    mode http\n    option httpchk GET /healthz\n    http-check expect status 200\n    default-server inter 5s fall 2 rise 1\n    server blue 127.0.0.1:8001 check\n    server green 127.0.0.1:8002 check disabled",
		"option httpchk GET /health\n    http-check expect status 200\n    default-server inter 5s fall 2 rise 1\n    server blue 127.0.0.1:8766 check disabled\n    server green 127.0.0.1:8767 check\n",
		"bind 192.0.2.10:8768\n    default_backend bk_mcp_unit_1",
		"server mcp_unit_5 127.0.0.1:8772 check",
		"stats socket /var/run/haproxy/admin.sock",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("adopter render lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "0.0.0.0") {
		t.Fatal("adopter render must not bind a wildcard")
	}
}

// Acceptance 5: the runtime API client sends `set server … state …` to the
// admin socket (fake socket) and reads state back; no reload is involved.
func TestRuntimeSetStateAndReadBack(t *testing.T) {
	fake := haproxytest.New(t, map[string][]string{"bk_web": {"blue", "green"}})
	r := haproxy.Runtime{Socket: fake.Path}
	ctx := context.Background()
	if err := r.SetState(ctx, "bk_web", "green", "ready"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetState(ctx, "bk_web", "blue", "drain"); err != nil {
		t.Fatal(err)
	}
	st, err := r.Servers(ctx, "bk_web")
	if err != nil {
		t.Fatal(err)
	}
	if !st["green"].Serving() || !st["blue"].Draining() || st["blue"].Ready() {
		t.Fatalf("read-back: %v", st)
	}
	if err := r.SetState(ctx, "bk_web", "nope", "ready"); err == nil {
		t.Fatal("an error answer must be an error")
	}
	if err := r.SetState(ctx, "bk_web", "green", "ready; shutdown"); err == nil {
		t.Fatal("state values are a closed set")
	}
	want := []string{"set server bk_web/green state ready", "set server bk_web/blue state drain", "show servers state bk_web", "set server bk_web/nope state ready"}
	got := fake.Commands()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestParseRealServersStateAnswer(t *testing.T) {
	// Shape of HAProxy 2.8 `show servers state bk_web` (columns trimmed).
	out := "1\n# be_id be_name srv_id srv_name srv_addr srv_op_state srv_admin_state srv_uweight srv_iweight srv_time_since_last_change srv_check_status srv_check_result srv_check_health srv_check_state srv_agent_state bk_f_forced_id srv_f_forced_id srv_fqdn srv_port srvrecord srv_use_ssl srv_check_port srv_check_addr srv_agent_addr srv_agent_port\n" +
		"3 bk_web 1 blue 127.0.0.1 2 0 1 1 12 6 3 4 6 0 0 0 - 18080 - 0 0 - - 0\n" +
		"3 bk_web 2 green 127.0.0.1 0 5 1 1 12 1 0 0 14 0 0 0 - 18081 - 0 0 - - 0\n"
	st, err := haproxy.ParseServersState(out, "bk_web")
	if err != nil {
		t.Fatal(err)
	}
	if !st["blue"].Serving() || !st["green"].Maint() || st["green"].Up() {
		t.Fatalf("parsed %v", st)
	}
	// Admin values observed on real HAProxy 2.8.31 and 3.1.17 for a server
	// rendered "disabled": 5 before, 4 after `ready`, 12 drain, 5 maint.
	for admin, want := range map[int]string{5: "maint", 4: "ready", 12: "drain", 8: "drain", 1: "maint", 0: "ready"} {
		s := haproxy.ServerState{Name: "x", Admin: admin, OpState: 2}
		got := map[bool]string{true: "ready"}[s.Ready()]
		if s.Maint() {
			got = "maint"
		} else if s.Draining() {
			got = "drain"
		}
		if got != want {
			t.Fatalf("admin %d: got %s want %s", admin, got, want)
		}
	}
	n, err := haproxy.ParseSessions("# pxname,svname,qcur,qmax,scur,smax\nbk_web,blue,0,0,7,9\nbk_web,green,0,0,0,0\n", "bk_web", "blue")
	if err != nil || n != 7 {
		t.Fatalf("scur = %d %v", n, err)
	}
}
