package haproxy

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Server admin-state bits from HAProxy's `show servers state` (srv_admin_state).
const (
	adminFMaint = 0x01
	adminIMaint = 0x02
	adminFDrain = 0x08
	adminIDrain = 0x10
	adminRMaint = 0x20
	adminHMaint = 0x40
	maintBits   = adminFMaint | adminIMaint | adminRMaint | adminHMaint
	drainBits   = adminFDrain | adminIDrain
	opRunning   = 2
)

// ServerState is one server's line in `show servers state`.
type ServerState struct {
	Name    string
	OpState int // 0 stopped, 1 starting, 2 running, 3 stopping
	Admin   int // bitfield, see admin* constants
}

// Maint reports administrative maintenance.
func (s ServerState) Maint() bool { return s.Admin&maintBits != 0 }

// Draining reports drain mode (and not maintenance).
func (s ServerState) Draining() bool { return !s.Maint() && s.Admin&drainBits != 0 }

// Ready reports neither maintenance nor drain.
func (s ServerState) Ready() bool { return s.Admin&(maintBits|drainBits) == 0 }

// Up reports that HAProxy's own health checks consider the server running.
func (s ServerState) Up() bool { return s.OpState == opRunning }

// Serving reports that new traffic may reach the server.
func (s ServerState) Serving() bool { return s.Ready() && s.Up() }

func (s ServerState) String() string {
	mode := "ready"
	if s.Maint() {
		mode = "maint"
	} else if s.Draining() {
		mode = "drain"
	}
	op := map[int]string{0: "down", 1: "starting", 2: "up", 3: "stopping"}[s.OpState]
	return fmt.Sprintf("%s(%s,%s)", s.Name, mode, op)
}

// Router is the runtime-API surface the deploy logic needs.
type Router interface {
	SetState(ctx context.Context, backend, server, state string) error
	Servers(ctx context.Context, backend string) (map[string]ServerState, error)
	Sessions(ctx context.Context, backend, server string) (int, error)
}

// Runtime talks to HAProxy's admin socket, one command per connection
// (non-interactive mode: HAProxy answers and closes).
type Runtime struct {
	Socket  string
	Timeout time.Duration
}

// Command sends one runtime-API command and returns the full answer.
func (r Runtime) Command(ctx context.Context, line string) (string, error) {
	if strings.ContainsAny(line, "\n\r;") {
		return "", fmt.Errorf("runtime command must be a single command: %q", line)
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", r.Socket)
	if err != nil {
		return "", fmt.Errorf("haproxy admin socket: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		return "", fmt.Errorf("haproxy admin socket write: %w", err)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		return string(out), fmt.Errorf("haproxy admin socket read: %w", err)
	}
	return string(out), nil
}

// SetState sends `set server <backend>/<server> state <state>`. HAProxy
// answers nothing on success; any text is an error message.
func (r Runtime) SetState(ctx context.Context, backend, server, st string) error {
	switch st {
	case "ready", "drain", "maint":
	default:
		return fmt.Errorf("unknown server state %q", st)
	}
	out, err := r.Command(ctx, fmt.Sprintf("set server %s/%s state %s", backend, server, st))
	if err != nil {
		return err
	}
	if msg := strings.TrimSpace(out); msg != "" {
		return fmt.Errorf("set server %s/%s state %s: %s", backend, server, st, msg)
	}
	return nil
}

// Servers parses `show servers state <backend>`.
func (r Runtime) Servers(ctx context.Context, backend string) (map[string]ServerState, error) {
	out, err := r.Command(ctx, "show servers state "+backend)
	if err != nil {
		return nil, err
	}
	return ParseServersState(out, backend)
}

// ParseServersState parses the version-1 `show servers state` format.
func ParseServersState(out, backend string) (map[string]ServerState, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	var cols map[string]int
	res := map[string]ServerState{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line == "1" && cols == nil:
			continue
		case strings.HasPrefix(line, "# "):
			cols = map[string]int{}
			for i, c := range strings.Fields(line[2:]) {
				cols[c] = i
			}
			continue
		case cols == nil:
			return nil, fmt.Errorf("show servers state %s: unexpected answer %q", backend, line)
		}
		f := strings.Fields(line)
		get := func(name string) (string, error) {
			i, ok := cols[name]
			if !ok || i >= len(f) {
				return "", fmt.Errorf("show servers state: missing column %s", name)
			}
			return f[i], nil
		}
		be, err := get("be_name")
		if err != nil {
			return nil, err
		}
		if be != backend {
			continue
		}
		name, err := get("srv_name")
		if err != nil {
			return nil, err
		}
		ops, err := get("srv_op_state")
		if err != nil {
			return nil, err
		}
		adm, err := get("srv_admin_state")
		if err != nil {
			return nil, err
		}
		op, err1 := strconv.Atoi(ops)
		ad, err2 := strconv.Atoi(adm)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("show servers state: bad state numbers in %q", line)
		}
		res[name] = ServerState{Name: name, OpState: op, Admin: ad}
	}
	if cols == nil {
		return nil, fmt.Errorf("show servers state %s: %s", backend, strings.TrimSpace(out))
	}
	return res, nil
}

// Sessions returns the current session count (scur) of one server.
func (r Runtime) Sessions(ctx context.Context, backend, server string) (int, error) {
	out, err := r.Command(ctx, "show stat "+backend+" 4 -1")
	if err != nil {
		return 0, err
	}
	return ParseSessions(out, backend, server)
}

// ParseSessions reads scur for backend/server from `show stat` CSV.
func ParseSessions(out, backend, server string) (int, error) {
	out = strings.TrimPrefix(strings.TrimSpace(out), "# ")
	rd := csv.NewReader(strings.NewReader(out))
	rd.FieldsPerRecord = -1
	recs, err := rd.ReadAll()
	if err != nil || len(recs) == 0 {
		return 0, fmt.Errorf("show stat %s: unexpected answer", backend)
	}
	idx := map[string]int{}
	for i, c := range recs[0] {
		idx[c] = i
	}
	px, sv, sc := idx["pxname"], idx["svname"], idx["scur"]
	if _, ok := idx["scur"]; !ok {
		return 0, fmt.Errorf("show stat %s: no scur column", backend)
	}
	for _, r := range recs[1:] {
		if len(r) > sc && r[px] == backend && r[sv] == server {
			return strconv.Atoi(r[sc])
		}
	}
	return 0, fmt.Errorf("show stat: %s/%s not found", backend, server)
}
