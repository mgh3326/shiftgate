package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/state"
)

// check pings <url> on success and <url>/fail on failure, sends the report
// as the body, and never prints the URL (it is a secret).
func TestCLICheckPingsWithoutPrintingURL(t *testing.T) {
	const secret = "0f6c1d0e-secret-uuid"
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
		mu.Unlock()
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.yaml")
	yaml := fmt.Sprintf(`
state_dir: %s/state
haproxy:
  socket: /nonexistent/admin.sock
  allowed_binds: ["127.0.0.1"]
services:
  - name: web
    image: ghcr.io/example/web
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    health: {http: /healthz}
`, dir)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		cmd := exec.Command(binary, append([]string{"--config", cfgPath}, args...)...)
		cmd.Env = append(os.Environ(), "SG_TEST_PING="+srv.URL+"/"+secret)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out.String()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, out.String()
	}

	// Never deployed: skipped, exit 0, success ping.
	code, out := run("check", "--ping-url-env", "SG_TEST_PING")
	if code != 0 || !strings.Contains(out, "web: skipped: not deployed yet") || strings.Contains(out, secret) || strings.Contains(out, srv.URL) {
		t.Fatalf("skip: %d %q", code, out)
	}

	// An interrupted operation nobody is running: failure, exit 1, /fail ping.
	st := state.Store{Dir: filepath.Join(dir, "state")}
	if err := st.Save(&state.State{Service: "web", Active: &state.Slot{Color: config.Blue, Digest: "sha256:" + strings.Repeat("a", 64)},
		Pending: &state.Intent{OpID: "op1", Op: "deploy", Phase: state.PhaseFlipping, StartedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	code, out = run("check", "web", "--ping-url-env", "SG_TEST_PING")
	if code != 1 || !strings.Contains(out, "web: FAIL recovery_required:") || strings.Contains(out, secret) {
		t.Fatalf("fail: %d %q", code, out)
	}
	if lines := strings.Count(out, "\n"); lines != 2 { // one reason line + the summary error
		t.Fatalf("want one line per failure plus the summary, got %d: %q", lines, out)
	}

	// A ping endpoint that is down only warns, without the URL.
	srv.Close()
	code, out = run("check", "--ping-url-env", "SG_TEST_PING")
	if code != 1 || !strings.Contains(out, "ping failed") || strings.Contains(out, secret) || strings.Contains(out, "127.0.0.1") {
		t.Fatalf("ping down: %d %q", code, out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !strings.HasPrefix(got[0], "POST /"+secret+" web: skipped") ||
		!strings.HasPrefix(got[1], "POST /"+secret+"/fail web: FAIL recovery_required") {
		t.Fatalf("pings: %q", got)
	}
}
