// Package config parses and validates services.yaml, the per-host description
// of every service shiftgate routes through the shared HAProxy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Color is one of the two slots of a blue-green service.
type Color string

const (
	Blue  Color = "blue"
	Green Color = "green"
)

// Other returns the opposite colour.
func (c Color) Other() Color {
	if c == Blue {
		return Green
	}
	return Blue
}

// Valid reports whether c is blue or green.
func (c Color) Valid() bool { return c == Blue || c == Green }

// Duration is a time.Duration that unmarshals from "30s"-style strings.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// File is the whole services.yaml.
type File struct {
	StateDir    string     `yaml:"state_dir"`
	KeepDigests int        `yaml:"keep_digests"`
	HAProxy     HAProxy    `yaml:"haproxy"`
	Services    []*Service `yaml:"services"`
}

// HAProxy describes the one shared HAProxy instance on this host.
type HAProxy struct {
	// Config is the generated configuration file shiftgate owns.
	Config string `yaml:"config"`
	// Socket is the admin socket path shiftgate dials (host view).
	Socket string `yaml:"socket"`
	// SocketInConfig is the socket path written into the "stats socket" line,
	// when HAProxy sees a different path (for example inside a container).
	SocketInConfig string `yaml:"socket_in_config"`
	// Check validates a candidate file; "{config}" is replaced by its path.
	Check []string `yaml:"check"`
	// Reload makes HAProxy load the installed file.
	Reload []string `yaml:"reload"`
	// AllowedBinds lists every address a frontend may bind. Render refuses
	// any other address, and wildcard addresses unless AllowWildcardBind.
	AllowedBinds      []string `yaml:"allowed_binds"`
	AllowWildcardBind bool     `yaml:"allow_wildcard_bind"`
	Log               string   `yaml:"log"`
	Timeouts          Timeouts `yaml:"timeouts"`
}

// Timeouts are rendered into the defaults section.
type Timeouts struct {
	Connect     Duration `yaml:"connect"`
	Client      Duration `yaml:"client"`
	Server      Duration `yaml:"server"`
	Tunnel      Duration `yaml:"tunnel"`
	HTTPRequest Duration `yaml:"http_request"`
}

// Service is one routed service. A service with Upstream set is a static
// route (render only); every other service is blue-green.
type Service struct {
	Name     string   `yaml:"name"`
	Mode     string   `yaml:"mode"` // http (default) or tcp
	Binds    []string `yaml:"binds"`
	Frontend string   `yaml:"frontend"`
	Backend  string   `yaml:"backend"`
	Health   Health   `yaml:"health"`
	Check    Check    `yaml:"check"`

	// Static route.
	Upstream string `yaml:"upstream"`

	// Blue-green.
	Image         string            `yaml:"image"`
	Ports         Ports             `yaml:"ports"`
	ContainerPort int               `yaml:"container_port"`
	Network       string            `yaml:"network"` // "" (published on loopback) or "host"
	EnvFiles      []string          `yaml:"env_files"`
	Env           map[string]string `yaml:"env"`
	RunArgs       []string          `yaml:"run_args"`
	Args          []string          `yaml:"args"`
	DrainTimeout  Duration          `yaml:"drain_timeout"`
	StopTimeout   Duration          `yaml:"stop_timeout"`
}

// Ports holds the host port of each colour.
type Ports struct {
	Blue  int `yaml:"blue"`
	Green int `yaml:"green"`
}

// Health is how shiftgate decides a freshly started colour may take traffic.
type Health struct {
	HTTP         string   `yaml:"http"` // path, e.g. /healthz
	TCP          bool     `yaml:"tcp"`
	ExpectStatus int      `yaml:"expect_status"`
	Timeout      Duration `yaml:"timeout"`
	Interval     Duration `yaml:"interval"`
}

// Check is HAProxy's own server health check.
type Check struct {
	Inter Duration `yaml:"inter"`
	Fall  int      `yaml:"fall"`
	Rise  int      `yaml:"rise"`
}

// Static reports whether s is a render-only route.
func (s *Service) Static() bool { return s.Upstream != "" }

// Port returns the host port of colour c.
func (s *Service) Port(c Color) int {
	if c == Blue {
		return s.Ports.Blue
	}
	return s.Ports.Green
}

// ContainerName is the Docker name of colour c.
func (s *Service) ContainerName(c Color) string { return "shiftgate-" + s.Name + "-" + string(c) }

// Service returns the named service.
func (f *File) Service(name string) (*Service, error) {
	for _, s := range f.Services {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("unknown service %q", name)
}

// Load reads, defaults and validates path.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes services.yaml strictly (unknown keys are errors).
func Parse(b []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("services.yaml: %w", err)
	}
	f.defaults()
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("services.yaml: %w", err)
	}
	return &f, nil
}

func (f *File) defaults() {
	if f.StateDir == "" {
		f.StateDir = "/var/lib/shiftgate"
	}
	if f.KeepDigests == 0 {
		f.KeepDigests = 3
	}
	h := &f.HAProxy
	if h.Config == "" {
		h.Config = "/etc/haproxy/haproxy.cfg"
	}
	if h.Socket == "" {
		h.Socket = "/run/haproxy/admin.sock"
	}
	if h.SocketInConfig == "" {
		h.SocketInConfig = h.Socket
	}
	if len(h.Check) == 0 {
		h.Check = []string{"haproxy", "-c", "-f", "{config}"}
	}
	if len(h.Reload) == 0 {
		h.Reload = []string{"systemctl", "reload", "haproxy"}
	}
	if h.Log == "" {
		h.Log = "stdout format raw local0"
	}
	def := func(d *Duration, v time.Duration) {
		if *d == 0 {
			*d = Duration(v)
		}
	}
	def(&h.Timeouts.Connect, 5*time.Second)
	def(&h.Timeouts.Client, time.Minute)
	def(&h.Timeouts.Server, time.Minute)
	def(&h.Timeouts.HTTPRequest, 10*time.Second)
	for _, s := range f.Services {
		if s.Mode == "" {
			s.Mode = "http"
		}
		if s.Frontend == "" {
			s.Frontend = "ft_" + identifier(s.Name)
		}
		if s.Backend == "" {
			s.Backend = "bk_" + identifier(s.Name)
		}
		if s.Health.HTTP != "" && s.Health.ExpectStatus == 0 {
			s.Health.ExpectStatus = 200
		}
		def(&s.Health.Timeout, 60*time.Second)
		def(&s.Health.Interval, time.Second)
		def(&s.Check.Inter, 2*time.Second)
		if s.Check.Fall == 0 {
			s.Check.Fall = 2
		}
		if s.Check.Rise == 0 {
			s.Check.Rise = 1
		}
		def(&s.DrainTimeout, 30*time.Second)
		def(&s.StopTimeout, 10*time.Second)
		if !s.Static() && s.ContainerPort == 0 && s.Network != "host" {
			s.ContainerPort = 8080
		}
	}
}

func identifier(name string) string { return strings.ReplaceAll(name, "-", "_") }

var (
	nameRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)
	proxyRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,62}$`)
	envKeyRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	imageRE   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	pathRE    = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)
	errConfig = errors.New("invalid configuration")
)

// managedRunFlags are docker run flags shiftgate sets itself; run_args may
// not override them.
var managedRunFlags = []string{
	"--name", "--label", "-l", "--network", "--net", "-p", "--publish", "-P", "--publish-all",
	"--env-file", "-e", "--env", "--rm", "-d", "--detach", "--restart", "-i", "-t",
	"--interactive", "--tty", "--privileged", "--pid", "--ipc", "--userns", "--uts", "--cap-add",
	"--security-opt", "--device", "--entrypoint",
}

func (f *File) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !strings.HasPrefix(f.StateDir, "/") {
		bad("state_dir must be absolute")
	}
	if f.KeepDigests < 1 {
		bad("keep_digests must be at least 1")
	}
	h := f.HAProxy
	for _, p := range []string{h.Config, h.Socket} {
		if !strings.HasPrefix(p, "/") {
			bad("haproxy paths must be absolute: %q", p)
		}
	}
	if !slices.ContainsFunc(h.Check, func(a string) bool { return strings.Contains(a, "{config}") }) {
		bad("haproxy.check must contain {config}")
	}
	if strings.ContainsAny(h.Log, "\n\r") || strings.ContainsAny(h.SocketInConfig, " \n\r\t") {
		bad("haproxy.log and haproxy.socket_in_config must be a single token/line")
	}
	if len(h.AllowedBinds) == 0 {
		bad("haproxy.allowed_binds must list every address a frontend may bind")
	}
	allowed := map[netip.Addr]bool{}
	for _, a := range h.AllowedBinds {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			bad("haproxy.allowed_binds: %q is not an IP address", a)
			continue
		}
		if ip.IsUnspecified() && !h.AllowWildcardBind {
			bad("haproxy.allowed_binds: wildcard %q needs haproxy.allow_wildcard_bind", a)
		}
		allowed[ip] = true
	}
	names := map[string]bool{}
	proxies := map[string]bool{}
	binds := map[string]string{}
	ports := map[int]string{}
	for _, s := range f.Services {
		where := "service " + s.Name
		if !nameRE.MatchString(s.Name) {
			bad("service name %q must match %s", s.Name, nameRE)
			continue
		}
		if names[s.Name] {
			bad("%s: duplicate name", where)
		}
		names[s.Name] = true
		for _, p := range []string{s.Frontend, s.Backend} {
			if !proxyRE.MatchString(p) {
				bad("%s: proxy name %q is not a safe identifier", where, p)
			}
			if proxies[p] {
				bad("%s: proxy name %q is used twice", where, p)
			}
			proxies[p] = true
		}
		if s.Mode != "http" && s.Mode != "tcp" {
			bad("%s: mode must be http or tcp", where)
		}
		if len(s.Binds) == 0 {
			bad("%s: binds must not be empty", where)
		}
		for _, b := range s.Binds {
			ip, err := BindAddr(b)
			if err != nil {
				bad("%s: bind %q: %v", where, b, err)
				continue
			}
			if prev, ok := binds[b]; ok {
				bad("%s: bind %s already used by %s", where, b, prev)
			}
			binds[b] = s.Name
			// Wildcard binds are decided at render time, where the explicit
			// --allow-wildcard-bind flag is available.
			if !ip.IsUnspecified() && !allowed[ip] {
				bad("%s: bind %s uses an address not in haproxy.allowed_binds", where, b)
			}
		}
		if (s.Health.HTTP == "") == !s.Health.TCP {
			bad("%s: health needs exactly one of http (path) or tcp: true", where)
		}
		if s.Health.HTTP != "" {
			if !pathRE.MatchString(s.Health.HTTP) {
				bad("%s: health.http must be an absolute path without spaces", where)
			}
			if s.Mode == "tcp" {
				bad("%s: an http health check needs mode http", where)
			}
		}
		if s.Health.ExpectStatus != 0 && (s.Health.ExpectStatus < 100 || s.Health.ExpectStatus > 599) {
			bad("%s: health.expect_status out of range", where)
		}
		if s.Health.Interval.D() <= 0 || s.Health.Timeout.D() < s.Health.Interval.D() {
			bad("%s: health.timeout must be at least health.interval", where)
		}
		if s.Static() {
			if _, err := netip.ParseAddrPort(s.Upstream); err != nil {
				bad("%s: upstream must be ip:port: %v", where, err)
			}
			if s.Image != "" || s.Ports != (Ports{}) {
				bad("%s: a static upstream cannot also set image or ports", where)
			}
			continue
		}
		if !imageRE.MatchString(s.Image) {
			bad("%s: image must be a repository without tag or digest (got %q)", where, s.Image)
		}
		for _, p := range []int{s.Ports.Blue, s.Ports.Green} {
			if p < 1 || p > 65535 {
				bad("%s: ports.blue and ports.green must be 1-65535", where)
				continue
			}
			if prev, ok := ports[p]; ok {
				bad("%s: port %d already used by %s", where, p, prev)
			}
			ports[p] = s.Name
		}
		if s.Ports.Blue == s.Ports.Green {
			bad("%s: ports.blue and ports.green must differ", where)
		}
		switch s.Network {
		case "":
			if s.ContainerPort < 1 || s.ContainerPort > 65535 {
				bad("%s: container_port out of range", where)
			}
		case "host":
			if s.ContainerPort != 0 {
				bad("%s: container_port is meaningless with network: host (the colour port is PORT)", where)
			}
		default:
			bad("%s: network must be empty or host", where)
		}
		for _, e := range s.EnvFiles {
			if !strings.HasPrefix(e, "/") {
				bad("%s: env_files must be absolute paths", where)
			}
		}
		for k := range s.Env {
			if !envKeyRE.MatchString(k) || k == "PORT" {
				bad("%s: env key %q is invalid or reserved", where, k)
			}
		}
		for _, a := range s.RunArgs {
			if flag := managedFlag(a); flag != "" {
				bad("%s: run_args may not set %s (shiftgate manages it)", where, flag)
			}
		}
		if s.DrainTimeout.D() < 0 || s.StopTimeout.D() < 0 {
			bad("%s: timeouts must not be negative", where)
		}
	}
	for b, svc := range binds {
		host, port, _ := net.SplitHostPort(b)
		ip, _ := BindAddr(b)
		if !ip.IsLoopback() && !ip.IsUnspecified() && host != "" {
			continue // colour ports are published on loopback only
		}
		if n, _ := strconv.Atoi(port); ports[n] != "" {
			bad("service %s: bind %s collides with a colour port of %s", svc, b, ports[n])
		}
	}
	if len(errs) > 0 {
		return errors.Join(append([]error{errConfig}, errs...)...)
	}
	return nil
}

// managedFlag returns the managed docker flag that arg sets, or "".
// Single-dash clusters such as "-dp" or "-p8080:80" are checked letter by
// letter so a short flag cannot hide inside them.
func managedFlag(arg string) string {
	flag, _, _ := strings.Cut(arg, "=")
	if slices.Contains(managedRunFlags, flag) {
		return flag
	}
	if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
		for _, r := range arg[1:] {
			if short := "-" + string(r); slices.Contains(managedRunFlags, short) {
				return short
			}
			if r < 'a' || r > 'z' {
				break
			}
		}
	}
	return ""
}

// BindAddr parses "addr:port" and returns the address. An empty host or "*"
// counts as the IPv4 wildcard.
func BindAddr(b string) (netip.Addr, error) {
	host, port, err := net.SplitHostPort(b)
	if err != nil {
		return netip.Addr{}, err
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return netip.Addr{}, fmt.Errorf("port %q out of range", port)
	}
	if host == "" || host == "*" {
		return netip.IPv4Unspecified(), nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("host must be an IP address: %w", err)
	}
	return ip, nil
}
