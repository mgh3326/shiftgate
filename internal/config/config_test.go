package config_test

import (
	"strings"
	"testing"

	"github.com/mgh3326/shiftgate/internal/config"
)

const base = `
haproxy:
  allowed_binds: ["127.0.0.1", "192.0.2.10"]
services:
  - name: web
    image: ghcr.io/example/web
    binds: ["127.0.0.1:8080"]
    ports: {blue: 18080, green: 18081}
    health: {http: /healthz}
`

func TestDefaults(t *testing.T) {
	f, err := config.Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	s := f.Services[0]
	if s.Frontend != "ft_web" || s.Backend != "bk_web" || s.ContainerPort != 8080 || s.Health.ExpectStatus != 200 || s.Mode != "http" {
		t.Fatalf("defaults not applied: %+v", s)
	}
	if f.HAProxy.Check[len(f.HAProxy.Check)-1] != "{config}" || f.StateDir != "/var/lib/shiftgate" {
		t.Fatalf("haproxy defaults: %+v", f.HAProxy)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"bind outside allowlist": {`binds: ["127.0.0.1:8080"]`, `binds: ["10.1.2.3:8080"]`, "not in haproxy.allowed_binds"},
		"wildcard in allowlist":  {`allowed_binds: ["127.0.0.1", "192.0.2.10"]`, `allowed_binds: ["0.0.0.0"]`, "allow_wildcard_bind"},
		"hostname bind":          {`binds: ["127.0.0.1:8080"]`, `binds: ["localhost:8080"]`, "must be an IP"},
		"tag in image":           {`image: ghcr.io/example/web`, `image: ghcr.io/example/web:latest`, "without tag"},
		"same colour ports":      {`{blue: 18080, green: 18081}`, `{blue: 18080, green: 18080}`, "must differ"},
		"both health kinds":      {`health: {http: /healthz}`, `health: {http: /healthz, tcp: true}`, "exactly one"},
		"no health":              {`health: {http: /healthz}`, `health: {}`, "exactly one"},
		"bad name":               {`name: web`, `name: Web_1`, "must match"},
		"unknown key":            {`image: ghcr.io/example/web`, "image: ghcr.io/example/web\n    imagee: x", "imagee"},
		"publish in run_args":    {`health: {http: /healthz}`, "health: {http: /healthz}\n    run_args: [\"-p\", \"80:80\"]", "may not set -p"},
		"clustered short flag":   {`health: {http: /healthz}`, "health: {http: /healthz}\n    run_args: [\"-dp8080:80\"]", "may not set -d"},
		"network in run_args":    {`health: {http: /healthz}`, "health: {http: /healthz}\n    run_args: [\"--network=host\"]", "may not set --network"},
		"relative env file":      {`health: {http: /healthz}`, "health: {http: /healthz}\n    env_files: [web.env]", "absolute"},
		"PORT env reserved":      {`health: {http: /healthz}`, "health: {http: /healthz}\n    env: {PORT: \"1\"}", "reserved"},
		"bind on colour port":    {`binds: ["127.0.0.1:8080"]`, `binds: ["127.0.0.1:18080"]`, "collides"},
		"keep_digests over cap":  {`haproxy:`, "keep_digests: 51\nhaproxy:", "keep_digests must be 1-50"},
		"negative timeout":       {`haproxy:`, "haproxy:\n  timeouts: {client: -1s}", "must not be negative"},
		"sub-ms check interval":  {`health: {http: /healthz}`, "health: {http: /healthz}\n    check: {inter: 500us}", "at least 1ms"},
		"empty service entry":    {`services:`, "services:\n  - ~", "services[0] is empty"},
		"health path injection":  {`health: {http: /healthz}`, `health: {http: "/x\nbind 0.0.0.0:1"}`, "absolute path"},
		"log size":               {`haproxy:`, "log: {max_size: 10MB}\nhaproxy:", "log.max_size"},
		"log files":              {`health: {http: /healthz}`, "health: {http: /healthz}\n    log: {max_file: -1}", "log.max_file"},
		"log on static upstream": {`health: {http: /healthz}`, "health: {http: /healthz}\n  - name: legacy\n    binds: [\"127.0.0.1:9000\"]\n    upstream: 127.0.0.1:9001\n    health: {tcp: true}\n    log: {max_file: -1}", "cannot set pre_deploy, registry_auth_file or log"},
		"bad log driver":         {`haproxy:`, "log: {driver: journald}\nhaproxy:", "log.driver"},
		"log-opt in run_args":    {`health: {http: /healthz}`, "health: {http: /healthz}\n    run_args: [\"--log-opt=max-size=1g\"]", "may not set --log-opt"},
		"empty pre_deploy":       {`health: {http: /healthz}`, "health: {http: /healthz}\n    pre_deploy: {timeout: 1m}", "pre_deploy.args"},
		"relative registry auth": {`health: {http: /healthz}`, "health: {http: /healthz}\n    registry_auth_file: reg.env", "registry_auth_file"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(base, c.from) {
				t.Fatalf("fixture lacks %q", c.from)
			}
			_, err := config.Parse([]byte(strings.Replace(base, c.from, c.to, 1)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestWildcardBindParsesButIsDecidedAtRender(t *testing.T) {
	f, err := config.Parse([]byte(strings.Replace(base, `binds: ["127.0.0.1:8080"]`, `binds: ["0.0.0.0:8080"]`, 1)))
	if err != nil {
		t.Fatalf("wildcard bind should parse (render refuses it): %v", err)
	}
	ip, _ := config.BindAddr(f.Services[0].Binds[0])
	if !ip.IsUnspecified() {
		t.Fatal("0.0.0.0 must be recognised as wildcard")
	}
	for _, b := range []string{":8080", "*:8080", "[::]:8080"} {
		if ip, err := config.BindAddr(b); err != nil || !ip.IsUnspecified() {
			t.Fatalf("%s should be a wildcard, got %v %v", b, ip, err)
		}
	}
}

func TestExamplesParse(t *testing.T) {
	for _, p := range []string{"../../examples/services.yaml", "../../examples/adopter-ncp.services.yaml"} {
		if _, err := config.Load(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestLogAndPreDeployDefaults(t *testing.T) {
	f, err := config.Parse([]byte(strings.Replace(base, "health: {http: /healthz}",
		"health: {http: /healthz}\n    log: {max_size: 50m}\n    pre_deploy: {args: [migrate]}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Log != (config.LogOptions{Driver: "json-file", MaxSize: "10m", MaxFile: 3}) {
		t.Fatalf("top-level log default: %+v", f.Log)
	}
	s := f.Services[0]
	if s.Log != (config.LogOptions{Driver: "json-file", MaxSize: "50m", MaxFile: 3}) {
		t.Fatalf("service log override: %+v", s.Log)
	}
	if s.PreDeploy == nil || s.PreDeploy.Timeout.D().Minutes() != 10 {
		t.Fatalf("pre_deploy default timeout: %+v", s.PreDeploy)
	}
}

// log.driver: daemon leaves logging to the Docker daemon, so run_args may
// set the log flags again (configs from before log limits keep loading).
func TestLogDriverDaemonAllowsRunArgs(t *testing.T) {
	f, err := config.Parse([]byte(strings.Replace(base, "health: {http: /healthz}",
		"health: {http: /healthz}\n    log: {driver: daemon}\n    run_args: [\"--log-driver=journald\"]", 1)))
	if err != nil || f.Services[0].Log.Driver != config.LogDaemon || f.Log.Driver != config.LogJSONFile {
		t.Fatalf("%v %+v", err, f)
	}
}
