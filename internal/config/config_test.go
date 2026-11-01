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
		"health path injection":  {`health: {http: /healthz}`, `health: {http: "/x\nbind 0.0.0.0:1"}`, "absolute path"},
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
