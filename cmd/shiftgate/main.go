// Command shiftgate runs blue-green container deploys behind one shared
// HAProxy per host. See README.md and docs/design.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
	"github.com/mgh3326/shiftgate/internal/deploy"
	"github.com/mgh3326/shiftgate/internal/docker"
	"github.com/mgh3326/shiftgate/internal/haproxy"
	"github.com/mgh3326/shiftgate/internal/health"
	"github.com/mgh3326/shiftgate/internal/state"
)

var version = "dev"

const usage = `usage: shiftgate [--config FILE] <command> [flags]

commands:
  deploy <service> --digest sha256:…   pull, start the idle colour, health-check, flip, drain, stop the old colour
  rollback <service>                   flip back to the previous colour's kept container
  recover <service>                    resolve an operation interrupted mid-flip
  status [--json]                      committed state, HAProxy servers and containers
  render [--no-reload] [--dry-run] [--allow-wildcard-bind]
                                       generate, validate (haproxy -c), install and reload the HAProxy config
  prune [<service>]                    remove old local images, keeping keep_digests per service
  version

exit codes: 0 ok, 1 failed (old colour serving), 2 usage, 3 recovery required, 75 locked, 78 config refused
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("shiftgate", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	cfgPath := global.String("config", "/etc/shiftgate/services.yaml", "services.yaml path")
	if err := global.Parse(args); err != nil {
		return deploy.CodeUsage
	}
	rest := global.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage)
		return deploy.CodeUsage
	}
	cmd, rest := rest[0], rest[1:]
	if cmd == "version" {
		fmt.Fprintln(stdout, "shiftgate", version)
		return 0
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "shiftgate:", err)
		return deploy.CodeConfig
	}
	g := &deploy.Gate{
		Cfg:        cfg,
		Store:      state.Store{Dir: cfg.StateDir},
		Docker:     docker.CLI{},
		Router:     haproxy.Runtime{Socket: cfg.HAProxy.Socket},
		Health:     health.Probe{},
		Log:        stderr,
		SyncConfig: true,
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var err2 error
	switch cmd {
	case "deploy":
		digest := fs.String("digest", "", "image digest sha256:… to promote")
		name, ok := oneService(fs, rest, stderr)
		if !ok {
			return deploy.CodeUsage
		}
		if *digest == "" {
			fmt.Fprintln(stderr, "shiftgate: deploy needs --digest sha256:…")
			return deploy.CodeUsage
		}
		var res *deploy.Result
		res, err2 = g.Deploy(ctx, name, *digest)
		printResult(stdout, res)
	case "rollback":
		name, ok := oneService(fs, rest, stderr)
		if !ok {
			return deploy.CodeUsage
		}
		var res *deploy.Result
		res, err2 = g.Rollback(ctx, name)
		printResult(stdout, res)
	case "recover":
		name, ok := oneService(fs, rest, stderr)
		if !ok {
			return deploy.CodeUsage
		}
		var msg string
		msg, err2 = g.Recover(ctx, name)
		if msg != "" {
			fmt.Fprintln(stdout, msg)
		}
	case "status":
		asJSON := fs.Bool("json", false, "print JSON")
		if fs.Parse(rest) != nil || fs.NArg() != 0 {
			return deploy.CodeUsage
		}
		printStatus(stdout, g.Status(ctx), *asJSON)
	case "render":
		var o deploy.RenderOptions
		fs.BoolVar(&o.NoReload, "no-reload", false, "install the file but do not reload HAProxy")
		fs.BoolVar(&o.DryRun, "dry-run", false, "print the config; write nothing")
		fs.BoolVar(&o.AllowWildcard, "allow-wildcard-bind", false, "permit 0.0.0.0/:: binds")
		if fs.Parse(rest) != nil || fs.NArg() != 0 {
			return deploy.CodeUsage
		}
		var data []byte
		data, err2 = g.Render(ctx, o)
		if err2 == nil && o.DryRun {
			stdout.Write(data)
		} else if err2 == nil {
			fmt.Fprintf(stdout, "rendered %s (validated)%s\n", cfg.HAProxy.Config, map[bool]string{true: "", false: " and reloaded"}[o.NoReload])
		}
	case "prune":
		if fs.Parse(rest) != nil || fs.NArg() > 1 {
			return deploy.CodeUsage
		}
		var res []deploy.PruneResult
		res, err2 = g.Prune(ctx, fs.Arg(0))
		for _, r := range res {
			fmt.Fprintf(stdout, "%s: removed %d, kept %d\n", r.Service, len(r.Removed), len(r.Kept))
			for _, w := range r.Warnings {
				fmt.Fprintf(stdout, "  warning: %s\n", w)
			}
		}
	default:
		fmt.Fprint(stderr, usage)
		return deploy.CodeUsage
	}
	if err2 != nil {
		fmt.Fprintln(stderr, "shiftgate:", err2)
	}
	return deploy.ExitCode(err2)
}

func oneService(fs *flag.FlagSet, args []string, stderr io.Writer) (string, bool) {
	// Accept the service before or after flags.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		name = ""
	}
	if name == "" {
		fmt.Fprintf(stderr, "shiftgate: %s needs exactly one service name\n", fs.Name())
		return "", false
	}
	return name, true
}

func printResult(w io.Writer, r *deploy.Result) {
	if r == nil {
		return
	}
	from := "nothing"
	if r.From != nil {
		from = fmt.Sprintf("%s@%s", r.From.Color, r.From.Digest)
	}
	fmt.Fprintf(w, "%s %s: %s → %s@%s (op %s)\n", r.Op, r.Service, from, r.To.Color, r.To.Digest, r.OpID)
}

func printStatus(w io.Writer, all []deploy.ServiceStatus, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(all)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tACTIVE\tDIGEST\tPREVIOUS\tLAST SWAP\tRESULT\tHAPROXY\tCONTAINERS")
	for _, s := range all {
		active, digest, prev, swap, result := "-", "-", "-", "-", "-"
		if s.Static {
			active = "static"
		}
		if st := s.State; st != nil {
			if st.Active != nil {
				active, digest = string(st.Active.Color), shortDigest(st.Active.Digest)
			}
			if st.Previous != nil {
				prev = string(st.Previous.Color) + "@" + shortDigest(st.Previous.Digest)
				if !st.RollbackAvailable {
					prev += " (gone)"
				}
			}
			if !st.LastSwapAt.IsZero() {
				swap = st.LastSwapAt.Format(time.RFC3339)
			}
			if st.LastResult != "" {
				result = st.LastResult
			}
			if st.Pending != nil {
				result = "PENDING " + st.Pending.Op + "/" + string(st.Pending.Phase)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, active, digest, prev, swap, result,
			joinMap(func(v deploy.ServerStatus) string { return v.Admin + "/" + v.Op }, s.Servers), joinMap(func(v string) string { return v }, s.Containers))
		for _, e := range s.Errors {
			fmt.Fprintf(tw, "  error: %s\t\t\t\t\t\t\t\n", e)
		}
	}
	tw.Flush()
}

func joinMap[V any](f func(V) string, m map[string]V) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + f(m[k])
	}
	return strings.Join(parts, ",")
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}
