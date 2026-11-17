package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga/internal/application/deployment"
	"github.com/hasankaradmir/karga/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga/internal/application/releases"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/config"
	"github.com/hasankaradmir/karga/internal/infrastructure/renderer"
	"github.com/hasankaradmir/karga/internal/ports"
)

// withService opens the state database and, only when a use case asks for it, the runtime.
func (a *app) withService(ctx context.Context, fn func(*releases.Service) error) error {
	if err := config.EnsureHome(a.cfg); err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, filepath.Join(a.cfg.Home, "karga.db"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var rt ports.Runtime
	var closeRT func()
	defer func() {
		if closeRT != nil {
			closeRT()
		}
	}()
	open := func(ctx context.Context) (ports.Runtime, error) {
		if rt == nil {
			var err error
			if rt, closeRT, err = a.newRuntime(ctx); err != nil {
				return nil, err
			}
		}
		return rt, nil
	}
	return fn(releases.New(store, open, a.cfg.Home, a.redactor.Add, a.log))
}

type commonFlags struct {
	dryRun     bool
	verbose    bool
	timeout    time.Duration
	historyMax int
	force      bool
	// deleteVolumes allows an upgrade or rollback to delete volumes with retention delete.
	deleteVolumes bool
}

func addDeleteVolumesFlag(cmd *cobra.Command, c *commonFlags) {
	cmd.Flags().BoolVar(&c.deleteVolumes, "delete-volumes", false, "allow deleting volumes the new revision no longer has and that have retention: delete (their data is lost)")
}

func addCommonFlags(cmd *cobra.Command, c *commonFlags, dryRun bool) {
	f := cmd.Flags()
	if dryRun {
		f.BoolVar(&c.dryRun, "dry-run", false, "show the deployment plan without changing anything")
	}
	f.BoolVarP(&c.verbose, "verbose", "v", false, "show every changed field of the plan")
	f.DurationVar(&c.timeout, "timeout", deployment.DefaultTimeout, "how long to wait for workloads to become ready")
	f.IntVar(&c.historyMax, "history-max", releases.DefaultHistoryMax, "revisions to keep per release (0 = unlimited)")
	f.BoolVar(&c.force, "force", false, "continue after an interrupted operation whose process is gone")
}

func (c commonFlags) options() releases.CommonOptions {
	return releases.CommonOptions{DryRun: c.dryRun, Timeout: c.timeout, HistoryMax: c.historyMax, Force: c.force, DeleteVolumes: c.deleteVolumes}
}

// optionsFor adds live progress on stderr, except for JSON output and dry runs.
func (a *app) optionsFor(c commonFlags) (releases.CommonOptions, func()) {
	o := c.options()
	if a.output == outputJSON || c.dryRun {
		return o, func() {}
	}
	p := newProgressPrinter(a.stderr)
	o.Progress = p.event
	return o, p.finish
}

// printResult reports a mutating command. A failed deployment still has a result worth showing.
func (a *app) printResult(verb string, res *releases.Result, verbose bool) error {
	if a.output == outputJSON {
		return writeJSON(a.stdout, res)
	}
	for _, w := range res.Warnings {
		if _, err := fmt.Fprintf(a.stderr, "Warning: %s\n", w); err != nil {
			return err
		}
	}
	switch {
	case res.DryRun && verbose:
		return res.Plan.Print(a.stdout, res.Release, res.From, res.Revision)
	case res.DryRun:
		return res.Plan.Summary(a.stdout, res.Release, res.From, res.Revision)
	case res.NoOp:
		_, err := fmt.Fprintf(a.stdout, "Release %q is already installed with the same package and values (revision %d).\n", res.Release, res.Revision)
		return err
	}
	var changed []string
	for _, c := range res.Plan.Changes {
		if c.Action == deployment.ActionUnchanged {
			continue
		}
		line := fmt.Sprintf("  %s %s %s", c.Action, strings.ToLower(c.Kind), c.Name)
		if c.Strategy != "" {
			line += " (" + c.Label() + ")"
		}
		if n := c.Note(); n != "" {
			line += " (" + n + ")"
		}
		changed = append(changed, line)
	}
	if _, err := fmt.Fprintf(a.stdout, "Release %q %s (revision %d).\n%s\n", res.Release, verb, res.Revision, strings.Join(changed, "\n")); err != nil {
		return err
	}
	if verbose {
		if err := res.Plan.Print(a.stdout, res.Release, res.From, res.Revision); err != nil {
			return err
		}
	}
	if res.Notes != "" {
		_, err := fmt.Fprintf(a.stdout, "\nNOTES:\n%s\n", res.Notes)
		return err
	}
	return nil
}

func newInstallCmd(a *app) *cobra.Command {
	var vals valueOpts
	var common commonFlags
	cmd := &cobra.Command{
		Use:   "install <release> <package>",
		Short: "Install a package as a new release",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				opts, done := a.optionsFor(common)
				res, err := s.Install(cmd.Context(), releases.InstallRequest{
					CommonOptions: opts, Name: args[0], Package: args[1], Values: vals.options(args[0]),
				})
				done()
				if err != nil {
					return err
				}
				return a.printResult("installed", res, common.verbose)
			})
		},
	}
	addValueFlags(cmd, &vals)
	addCommonFlags(cmd, &common, true)
	return cmd
}

func addStrategyFlag(cmd *cobra.Command, strategy *string) {
	cmd.Flags().StringVar(strategy, "strategy", "", "override every workload's updateStrategy: recreate|replace")
}

func validStrategy(s string) error {
	if s != "" && s != resource.StrategyRecreate && s != resource.StrategyReplace {
		return fmt.Errorf("invalid --strategy %q: must be recreate or replace", s)
	}
	return nil
}

func newUpgradeCmd(a *app) *cobra.Command {
	var vals valueOpts
	var common commonFlags
	var reuse, install bool
	var strategy string
	cmd := &cobra.Command{
		Use:   "upgrade <release> <package>",
		Short: "Upgrade a release to a new package version or new values",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validStrategy(strategy); err != nil {
				return err
			}
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				opts, done := a.optionsFor(common)
				res, err := s.Upgrade(cmd.Context(), releases.UpgradeRequest{
					CommonOptions: opts, Name: args[0], Package: args[1], Values: vals.options(args[0]),
					ReuseValues: reuse, Install: install, Strategy: strategy,
				})
				done()
				if err != nil {
					return err
				}
				return a.printResult("upgraded", res, common.verbose)
			})
		},
	}
	addValueFlags(cmd, &vals)
	addCommonFlags(cmd, &common, true)
	cmd.Flags().BoolVar(&reuse, "reuse-values", false, "start from the current revision's values")
	cmd.Flags().BoolVarP(&install, "install", "i", false, "install the release if it does not exist")
	addStrategyFlag(cmd, &strategy)
	addDeleteVolumesFlag(cmd, &common)
	return cmd
}

func newRollbackCmd(a *app) *cobra.Command {
	var common commonFlags
	var strategy string
	cmd := &cobra.Command{
		Use:   "rollback <release> [revision]",
		Short: "Re-apply an earlier revision as a new revision",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validStrategy(strategy); err != nil {
				return err
			}
			rev := 0
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 1 {
					return fmt.Errorf("invalid revision %q", args[1])
				}
				rev = n
			}
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				opts, done := a.optionsFor(common)
				res, err := s.Rollback(cmd.Context(), releases.RollbackRequest{CommonOptions: opts, Name: args[0], Revision: rev, Strategy: strategy})
				done()
				if err != nil {
					return err
				}
				return a.printResult("rolled back", res, common.verbose)
			})
		},
	}
	addCommonFlags(cmd, &common, true)
	addStrategyFlag(cmd, &strategy)
	addDeleteVolumesFlag(cmd, &common)
	return cmd
}

func newUninstallCmd(a *app) *cobra.Command {
	var req releases.UninstallRequest
	cmd := &cobra.Command{
		Use:   "uninstall <release>",
		Short: "Remove a release (volumes are kept unless --delete-volumes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.Uninstall(cmd.Context(), req); err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, map[string]any{"release": req.Name, "uninstalled": true})
				}
				_, err := fmt.Fprintf(a.stdout, "Release %q uninstalled.\n", req.Name)
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&req.DeleteVolumes, "delete-volumes", false, "also delete volumes (data is lost)")
	cmd.Flags().BoolVar(&req.KeepHistory, "keep-history", false, "keep revision history; the release is marked uninstalled")
	cmd.Flags().BoolVar(&req.Force, "force", false, "continue after an interrupted operation whose process is gone")
	return cmd
}

func ago(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

func newHistoryCmd(a *app) *cobra.Command {
	var withDiff bool
	cmd := &cobra.Command{
		Use:   "history <release>",
		Short: "Show the revisions of a release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				revs, err := s.History(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, revs)
				}
				header := "REVISION\tSTATUS\tPACKAGE\tVERSION\tCREATED\tDESCRIPTION"
				if withDiff {
					header += "\tCHANGES"
				}
				var rows [][]string
				for i, r := range revs {
					row := []string{strconv.Itoa(r.Number), string(r.Status), r.PackageName, r.PackageVersion, ago(r.CreatedAt), r.Description}
					if withDiff {
						row = append(row, changeSummary(revs, i))
					}
					rows = append(rows, row)
				}
				return a.table(header, rows)
			})
		},
	}
	cmd.Flags().BoolVar(&withDiff, "diff", false, "add what changed since the previous revision")
	return cmd
}

// changeSummary counts the differences of revision i from the one before it.
func changeSummary(revs []release.Revision, i int) string {
	if i == 0 {
		return "first"
	}
	res, err := deployment.CompareManifests(revs[i-1].Manifest, revs[i].Manifest)
	if err != nil {
		return "?"
	}
	n := map[string]int{}
	for _, r := range res {
		n[r.Action]++
	}
	values := len(deployment.CompareValues(revs[i-1].ValuesAll, revs[i].ValuesAll))
	return fmt.Sprintf("+%d ~%d -%d, %d values", n["create"], n["update"], n["delete"], values)
}

func newListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List releases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				rels, err := s.List(cmd.Context())
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					if rels == nil {
						return writeJSON(a.stdout, []any{})
					}
					return writeJSON(a.stdout, rels)
				}
				var rows [][]string
				for _, r := range rels {
					rows = append(rows, []string{r.Name, strconv.Itoa(r.CurrentRevision), string(r.Status), r.PackageName, r.PackageVersion, ago(r.UpdatedAt)})
				}
				return a.table("NAME\tREVISION\tSTATUS\tPACKAGE\tVERSION\tUPDATED", rows)
			})
		},
	}
}

func newStatusCmd(a *app) *cobra.Command {
	var watch bool
	var interval time.Duration
	var count int
	cmd := &cobra.Command{
		Use:   "status <release>",
		Short: "Compare a release's desired state with the runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if !watch {
					return a.printStatus(cmd.Context(), s, args[0])
				}
				tty := isTerminal(a.stdout)
				for n := 1; ; n++ {
					if tty {
						_, _ = io.WriteString(a.stdout, "\x1b[H\x1b[2J") // clear and redraw
					} else if n > 1 {
						_, _ = io.WriteString(a.stdout, "\n")
					}
					_, _ = fmt.Fprintf(a.stdout, "Every %s: karga status %s    %s\n\n", interval, args[0], time.Now().Format("15:04:05"))
					if err := a.printStatus(cmd.Context(), s, args[0]); err != nil {
						return err
					}
					if count > 0 && n >= count {
						return nil
					}
					select {
					case <-cmd.Context().Done():
						return nil
					case <-time.After(interval):
					}
				}
			})
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh until interrupted")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "time between refreshes with --watch")
	cmd.Flags().IntVar(&count, "count", 0, "with --watch, stop after this many refreshes (0: until interrupted)")
	return cmd
}

func (a *app) printStatus(ctx context.Context, s *releases.Service, name string) error {
	rep, err := s.Status(ctx, name)
	if err != nil {
		return err
	}
	if a.output == outputJSON {
		return writeJSON(a.stdout, rep)
	}
	r := rep.Release
	_, _ = fmt.Fprintf(a.stdout, "Release:  %s\nStatus:   %s\nRevision: %d\nPackage:  %s %s\nUpdated:  %s\n\n",
		r.Name, a.paint(string(r.Status)), r.CurrentRevision, r.PackageName, r.PackageVersion, ago(r.UpdatedAt))
	var rows [][]string
	for _, x := range rep.Resources {
		health := x.Health
		if x.Backend != "" {
			health = "-> " + x.Backend
		}
		if x.OOMKilled {
			health = strings.TrimPrefix(health+", oom-killed", ", ")
		}
		restarts := ""
		if x.Kind == resource.KindWorkload || x.Kind == deployment.KindProxy {
			restarts = strconv.Itoa(x.Restarts)
		}
		rows = append(rows, []string{x.Kind, x.Name, x.RuntimeName, strconv.Itoa(x.Revision), x.Desired, x.Actual, restarts, health})
	}
	if err := a.table("KIND\tNAME\tRUNTIME NAME\tREVISION\tDESIRED\tACTUAL\tRESTARTS\tHEALTH/BACKEND", rows); err != nil {
		return err
	}
	if len(rep.Orphans) > 0 {
		_, _ = fmt.Fprintf(a.stdout, "\nNot tracked by karga: %s\n", strings.Join(rep.Orphans, ", "))
	}
	if rep.Interrupted != "" {
		_, _ = fmt.Fprintf(a.stdout, "\nWARNING: operation still marked running: %s\n", rep.Interrupted)
	}
	return nil
}

func newGetCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "get", Short: "Show stored information about a release"}
	var revision int
	var all bool
	values := &cobra.Command{
		Use:   "values <release>",
		Short: "Show the values a revision was deployed with (user overrides unless --all)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				v, err := s.GetValues(cmd.Context(), args[0], revision, all)
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, v)
				}
				out, err := renderer.ToYAML(v)
				if err != nil {
					return err
				}
				_, err = a.stdout.Write(out)
				return err
			})
		},
	}
	values.Flags().IntVar(&revision, "revision", 0, "revision (default: current)")
	values.Flags().BoolVar(&all, "all", false, "show the merged values including package defaults")

	var manifestRev int
	manifest := &cobra.Command{
		Use:   "manifest <release>",
		Short: "Show the resources a revision deployed (secret values redacted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				rs, err := s.GetManifest(cmd.Context(), args[0], manifestRev)
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, rs)
				}
				for _, r := range rs {
					out, err := toYAMLViaJSON(r)
					if err != nil {
						return err
					}
					if _, err := fmt.Fprintf(a.stdout, "---\n%s", out); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	manifest.Flags().IntVar(&manifestRev, "revision", 0, "revision (default: current)")
	var notesRev int
	notes := &cobra.Command{
		Use:   "notes <release>",
		Short: "Show the package's notes (NOTES.txt) for a revision",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				n, err := s.GetNotes(cmd.Context(), args[0], notesRev)
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, map[string]string{"notes": n})
				}
				if n == "" {
					n = "(the package has no NOTES.txt)"
				}
				_, err = fmt.Fprintln(a.stdout, n)
				return err
			})
		},
	}
	notes.Flags().IntVar(&notesRev, "revision", 0, "revision (default: current)")
	cmd.AddCommand(values, manifest, notes)
	return cmd
}

func newGatewayCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Configure the host gateway: where it listens and on which listeners",
		Long: "The gateway is the host's single entry point. Its administrator decides where it listens\n" +
			"(set listen, default 127.0.0.1) and on which ports (listeners); packages only attach their\n" +
			"Services to listeners by name and can open nothing themselves.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the gateway's health and the listeners and routes it serves",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				health, err := s.GatewayHealth(cmd.Context())
				if err != nil {
					return err
				}
				routes, err := s.Gateway(cmd.Context())
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					if routes == nil {
						routes = []gateway.Exposed{}
					}
					return writeJSON(a.stdout, map[string]any{"gateway": health, "routes": routes})
				}
				state := health.Container
				switch {
				case health.Container == "missing":
					state = "not running (nothing is exposed)"
				case health.Ready:
					state += ", ready (Envoy " + health.Version + ")"
				case health.Error != "":
					state += ", admin API unreachable: " + health.Error
				default:
					state += ", not ready"
				}
				access := "off"
				if health.AccessLog {
					access = "on (karga logs --gateway)"
				}
				_, _ = fmt.Fprintf(a.stdout, "Gateway:    %s\n", state)
				if health.Image != "" {
					_, _ = fmt.Fprintf(a.stdout, "Image:      %s\n", health.Image)
				}
				_, _ = fmt.Fprintf(a.stdout, "Listen:     %s\n", strings.Join(health.Listen, ", "))
				_, _ = fmt.Fprintf(a.stdout, "Access log: %s\n\n", access)
				var rows [][]string
				for _, r := range routes {
					rows = append(rows, []string{r.Listener, r.Route, r.Release, r.Service, r.Upstream})
				}
				return a.table("LISTENER\tROUTE\tRELEASE\tSERVICE\tUPSTREAM", rows)
			})
		},
	})
	set := &cobra.Command{Use: "set", Short: "Change a gateway setting"}
	set.AddCommand(&cobra.Command{
		Use:       "access-log on|off",
		Short:     "Log every request and connection the gateway handles (off by default)",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.SetGatewayAccessLog(cmd.Context(), args[0] == "on"); err != nil {
					return err
				}
				_, err := fmt.Fprintf(a.stdout, "gateway access log %s\n", args[0])
				return err
			})
		},
	})
	set.AddCommand(&cobra.Command{
		Use:   "listen <ip>[,<ip>...]",
		Short: "Set the host addresses every listener binds (default 127.0.0.1; 0.0.0.0 is every interface)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addrs := strings.Split(args[0], ",")
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.SetGatewayListen(cmd.Context(), addrs); err != nil {
					return err
				}
				_, err := fmt.Fprintf(a.stdout, "gateway listens on %s\n", strings.Join(addrs, ", "))
				return err
			})
		},
	})
	cmd.AddCommand(set, newListenerCmd(a))
	return cmd
}

func newListenerCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "listener", Short: "Define the ports services can be exposed on"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the listeners and the services using them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				settings, users, err := s.GatewaySettings(cmd.Context())
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, map[string]any{"listen": settings.Listen, "listeners": settings.Listeners, "users": users})
				}
				var rows [][]string
				for _, l := range settings.Listeners {
					rows = append(rows, []string{l.Name, strconv.Itoa(l.Port), l.Protocol, strings.Join(users[l.Name], ", ")})
				}
				return a.table("NAME\tPORT\tPROTOCOL\tSERVICES", rows)
			})
		},
	})
	var l gateway.Listener
	add := &cobra.Command{
		Use:   "add <name> --port <port> --protocol tcp|http|https",
		Short: "Define a listener (a tcp listener belongs to one service; http and https are shared by host)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l.Name = args[0]
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.AddGatewayListener(cmd.Context(), l); err != nil {
					return err
				}
				_, err := fmt.Fprintf(a.stdout, "listener %s added (%s on port %d)\n", l.Name, l.Protocol, l.Port)
				return err
			})
		},
	}
	add.Flags().IntVar(&l.Port, "port", 0, "host port")
	add.Flags().StringVar(&l.Protocol, "protocol", "", "tcp, http or https")
	_ = add.MarkFlagRequired("port")
	_ = add.MarkFlagRequired("protocol")
	cmd.AddCommand(add, &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a listener no service uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.RemoveGatewayListener(cmd.Context(), args[0]); err != nil {
					return err
				}
				_, err := fmt.Fprintf(a.stdout, "listener %s removed\n", args[0])
				return err
			})
		},
	})
	return cmd
}

func newStateCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Manage karga's state database",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "backup <file>",
		Short: "Write a consistent copy of the state database (safe while karga runs)",
		Long: "Write a consistent copy of the state database to <file> (mode 0600, must not exist).\n" +
			"To restore, stop using karga and copy the file over $KARGA_HOME/karga.db; proxy and gateway\n" +
			"configuration is rebuilt from the state on the next upgrade.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.EnsureHome(a.cfg); err != nil {
				return err
			}
			store, err := sqlite.Open(cmd.Context(), filepath.Join(a.cfg.Home, "karga.db"))
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			if err := store.Backup(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "state written to %s\n"+
				"The backup does not hold %s, which keys the digests of secret values: copy it too (and keep\n"+
				"both safe). Restored without it, each release applies its Secrets once more on its next upgrade.\n",
				args[0], filepath.Join(a.cfg.Home, "state.key"))
			return err
		},
	})
	return cmd
}

func newLogsCmd(a *app) *cobra.Command {
	var req releases.LogsRequest
	cmd := &cobra.Command{
		Use:   "logs <release> [workload...]",
		Short: "Show the logs of a release's workloads, its proxy or the gateway",
		Example: "  karga logs shop            # every workload, prefixed by name\n" +
			"  karga logs shop web -f     # follow one workload\n" +
			"  karga logs shop --proxy    # the release proxy (Linux)\n" +
			"  karga logs --gateway       # the host gateway (Linux)",
		Args: func(_ *cobra.Command, args []string) error {
			if req.Gateway != (len(args) == 0) {
				return errors.New("give a release, or --gateway without one")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				req.Release, req.Workloads = args[0], args[1:]
			}
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				return s.Logs(cmd.Context(), req, a.stdout)
			})
		},
	}
	cmd.Flags().IntVar(&req.Tail, "tail", 100, "lines from the end of each log (-1 for all)")
	cmd.Flags().BoolVarP(&req.Follow, "follow", "f", false, "keep streaming new lines")
	cmd.Flags().BoolVar(&req.Proxy, "proxy", false, "the release proxy instead of workloads")
	cmd.Flags().BoolVar(&req.Gateway, "gateway", false, "the host gateway")
	return cmd
}

func newVolumeCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "volume", Short: "Back up and restore a release's volumes"}
	var backup releases.VolumeBackupRequest
	b := &cobra.Command{
		Use:   "backup <release> <volume> <file.tar.gz>",
		Short: "Copy a volume's files to a backup file (0600, must not exist)",
		Long: "Copy a volume's files to a gzipped tar through a workload that mounts it. By default the copy is\n" +
			"taken while the workloads run; --stop stops them meanwhile for a consistent copy. For databases\n" +
			"prefer --stop or the database's own dump tool.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			backup.Release, backup.Volume, backup.File = args[0], args[1], args[2]
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.BackupVolume(cmd.Context(), backup); err != nil {
					return err
				}
				_, err := fmt.Fprintf(a.stdout, "volume %s of %s written to %s\n", backup.Volume, backup.Release, backup.File)
				return err
			})
		},
	}
	b.Flags().BoolVar(&backup.Stop, "stop", false, "stop the workloads mounting the volume during the copy")
	var restore releases.VolumeRestoreRequest
	r := &cobra.Command{
		Use:   "restore <release> <volume> <file.tar.gz>",
		Short: "Write a backup into a volume (its workloads are stopped meanwhile)",
		Long: "Write a backup into a volume. The workloads mounting it are stopped during the copy and started\n" +
			"again. Files in the backup overwrite files of the same name; other files in the volume are kept.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			restore.Release, restore.Volume, restore.File = args[0], args[1], args[2]
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				if err := s.RestoreVolume(cmd.Context(), restore); err != nil {
					return err
				}
				_, err := fmt.Fprintf(a.stdout, "volume %s of %s restored from %s\n", restore.Volume, restore.Release, restore.File)
				return err
			})
		},
	}
	r.Flags().BoolVar(&restore.Force, "force", false, "restore a backup taken from another volume or release")
	cmd.AddCommand(b, r)
	return cmd
}

func newDiffCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <release> <revision> [revision]",
		Short: "Show how two revisions differ (the second defaults to the current one)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("revision %q is not a number", args[1])
			}
			to := 0
			if len(args) == 3 {
				if to, err = strconv.Atoi(args[2]); err != nil {
					return fmt.Errorf("revision %q is not a number", args[2])
				}
			}
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				d, err := s.Diff(cmd.Context(), args[0], from, to)
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, d)
				}
				var b strings.Builder
				fmt.Fprintf(&b, "Release %s: revision %d -> %d\n", d.Release, d.From, d.To)
				fields := func(fs []deployment.FieldChange, indent string) {
					for _, f := range fs {
						switch {
						case f.Old == "":
							fmt.Fprintf(&b, "%s%s: %s\n", indent, f.Path, f.New)
						case f.New == "":
							fmt.Fprintf(&b, "%s%s: %s -> (removed)\n", indent, f.Path, f.Old)
						default:
							fmt.Fprintf(&b, "%s%s: %s -> %s\n", indent, f.Path, f.Old, f.New)
						}
					}
				}
				if len(d.Package) > 0 {
					b.WriteString("\nPackage:\n")
					fields(d.Package, "  ")
				}
				if len(d.Values) > 0 {
					b.WriteString("\nValues:\n")
					fields(d.Values, "  ")
				}
				b.WriteString("\nResources:\n")
				if len(d.Resources) == 0 {
					b.WriteString("  no differences\n")
				}
				for _, r := range d.Resources {
					fmt.Fprintf(&b, "  %s %s %s\n", map[string]string{"create": "+", "update": "~", "delete": "-"}[r.Action], strings.ToUpper(r.Kind), r.Name)
					fields(r.Fields, "      ")
				}
				_, err = io.WriteString(a.stdout, b.String())
				return err
			})
		},
	}
}
