// Package haproxytest provides a fake HAProxy admin socket: a Unix-socket
// server that speaks the runtime-API subset shiftgate uses, records every
// command, and lets tests inject failures.
package haproxytest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Action overrides how the fake answers one command.
type Action struct {
	Reply string // answer text instead of the normal one
	Apply bool   // still apply the command's effect
	Hang  bool   // never answer (the client times out); effect per Apply
}

// Server is one fake backend server.
type Server struct {
	Admin   int // admin bits as in `show servers state`
	Op      int // 0 down, 2 up
	Healthy bool
	Scur    int
}

// Fake is a running fake admin socket.
type Fake struct {
	Path string

	mu       sync.Mutex
	backends map[string]map[string]*Server
	commands []string
	hook     func(cmd string) *Action
	ln       net.Listener
	wg       sync.WaitGroup
}

// New starts a fake with the given backends; each server starts disabled
// (maintenance, as `disabled` in the generated config) and healthy.
func New(t testing.TB, backends map[string][]string) *Fake {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes; t.TempDir is too long on macOS.
	dir, err := os.MkdirTemp("/tmp", "sgfake")
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{Path: filepath.Join(dir, "admin.sock"), backends: map[string]map[string]*Server{}}
	for be, servers := range backends {
		f.backends[be] = map[string]*Server{}
		for _, s := range servers {
			// As rendered "disabled": FMAINT|CMAINT (observed on HAProxy 2.8 and 3.1).
			f.backends[be][s] = &Server{Admin: 0x05, Op: 0, Healthy: true}
		}
	}
	ln, err := net.Listen("unix", f.Path)
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
		os.RemoveAll(dir)
	})
	return f
}

// Hook installs a function that may override the answer to a command.
func (f *Fake) Hook(h func(cmd string) *Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

// Commands returns every command received so far.
func (f *Fake) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// Set changes a server directly (test setup).
func (f *Fake) Set(backend, server string, fn func(*Server)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.backends[backend][server])
}

// Get returns a copy of a server.
func (f *Fake) Get(backend, server string) Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.backends[backend][server]
}

func (f *Fake) serve() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			line, err := bufio.NewReader(c).ReadString('\n')
			if err != nil {
				return
			}
			reply, hang := f.handle(strings.TrimSpace(line))
			if hang {
				time.Sleep(2 * time.Second)
				return
			}
			_, _ = c.Write([]byte(reply))
		}()
	}
}

func (f *Fake) handle(cmd string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	var act *Action
	if f.hook != nil {
		act = f.hook(cmd)
	}
	if act != nil && !act.Apply {
		return act.Reply, act.Hang
	}
	reply := f.apply(cmd)
	if act != nil {
		return act.Reply, act.Hang
	}
	return reply, false
}

func (f *Fake) apply(cmd string) string {
	fields := strings.Fields(cmd)
	switch {
	case len(fields) == 5 && fields[0] == "set" && fields[1] == "server" && fields[3] == "state":
		be, sv, ok := strings.Cut(fields[2], "/")
		s := f.backends[be][sv]
		if !ok || s == nil {
			return "No such server.\n"
		}
		switch fields[4] {
		case "ready":
			// Real HAProxy keeps the informational CMAINT bit (0x04) of a
			// server that started "disabled"; only FMAINT/FDRAIN clear.
			s.Admin &= 0x04
			s.Op = map[bool]int{true: 2, false: 0}[s.Healthy]
		case "drain":
			s.Admin = s.Admin&0x04 | 0x08
		case "maint":
			s.Admin = s.Admin&0x04 | 0x01
			s.Op = 0
		default:
			return "'set server <srv> state' expects 'ready', 'drain' and 'maint'.\n"
		}
		return ""
	case len(fields) == 4 && fields[0] == "show" && fields[1] == "servers" && fields[2] == "state":
		srv := f.backends[fields[3]]
		if srv == nil {
			return "Can't find backend.\n"
		}
		var b strings.Builder
		b.WriteString("1\n# be_id be_name srv_id srv_name srv_addr srv_op_state srv_admin_state srv_uweight\n")
		for i, name := range sortedNames(srv) {
			s := srv[name]
			fmt.Fprintf(&b, "3 %s %d %s 127.0.0.1 %d %d 1\n", fields[3], i+1, name, s.Op, s.Admin)
		}
		return b.String()
	case len(fields) == 5 && fields[0] == "show" && fields[1] == "stat":
		srv := f.backends[fields[2]]
		var b strings.Builder
		b.WriteString("# pxname,svname,qcur,qmax,scur,smax,\n")
		for _, name := range sortedNames(srv) {
			fmt.Fprintf(&b, "%s,%s,0,0,%d,0,\n", fields[2], name, srv[name].Scur)
		}
		return b.String()
	}
	return "Unknown command.\n"
}

func sortedNames(m map[string]*Server) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
