package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga/internal/application/deployment"
	"github.com/hasankaradmir/karga/internal/application/releases"
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
	timeout    time.Duration
	historyMax int
	force      bool
}

func addCommonFlags(cmd *cobra.Command, c *commonFlags, dryRun bool) {
	f := cmd.Flags()
	if dryRun {
		f.BoolVar(&c.dryRun, "dry-run", false, "show the deployment plan without changing anything")
	}
	f.DurationVar(&c.timeout, "timeout", deployment.DefaultTimeout, "how long to wait for workloads to become ready")
	f.IntVar(&c.historyMax, "history-max", releases.DefaultHistoryMax, "revisions to keep per release (0 = unlimited)")
	f.BoolVar(&c.force, "force", false, "continue after an interrupted operation whose process is gone")
}

func (c commonFlags) options() releases.CommonOptions {
	return releases.CommonOptions{DryRun: c.dryRun, Timeout: c.timeout, HistoryMax: c.historyMax, Force: c.force}
}

// printResult reports a mutating command. A failed deployment still has a result worth showing.
func (a *app) printResult(verb string, res *releases.Result) error {
	if a.output == outputJSON {
		return writeJSON(a.stdout, res)
	}
	switch {
	case res.DryRun:
		return res.Plan.Print(a.stdout, res.Release, res.From, res.Revision)
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
		changed = append(changed, line)
	}
	_, err := fmt.Fprintf(a.stdout, "Release %q %s (revision %d).\n%s\n", res.Release, verb, res.Revision, strings.Join(changed, "\n"))
	return err
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
				res, err := s.Install(cmd.Context(), releases.InstallRequest{
					CommonOptions: common.options(), Name: args[0], Package: args[1], Values: vals.options(args[0]),
				})
				if err != nil {
					return err
				}
				return a.printResult("installed", res)
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
				res, err := s.Upgrade(cmd.Context(), releases.UpgradeRequest{
					CommonOptions: common.options(), Name: args[0], Package: args[1], Values: vals.options(args[0]),
					ReuseValues: reuse, Install: install, Strategy: strategy,
				})
				if err != nil {
					return err
				}
				return a.printResult("upgraded", res)
			})
		},
	}
	addValueFlags(cmd, &vals)
	addCommonFlags(cmd, &common, true)
	cmd.Flags().BoolVar(&reuse, "reuse-values", false, "start from the current revision's values")
	cmd.Flags().BoolVarP(&install, "install", "i", false, "install the release if it does not exist")
	addStrategyFlag(cmd, &strategy)
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
				res, err := s.Rollback(cmd.Context(), releases.RollbackRequest{CommonOptions: common.options(), Name: args[0], Revision: rev, Strategy: strategy})
				if err != nil {
					return err
				}
				return a.printResult("rolled back", res)
			})
		},
	}
	addCommonFlags(cmd, &common, true)
	addStrategyFlag(cmd, &strategy)
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

func (a *app) table(header string, rows [][]string) error {
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, header)
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	return w.Flush()
}

func ago(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

func newHistoryCmd(a *app) *cobra.Command {
	return &cobra.Command{
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
				var rows [][]string
				for _, r := range revs {
					rows = append(rows, []string{strconv.Itoa(r.Number), string(r.Status), r.PackageName, r.PackageVersion, ago(r.CreatedAt), r.Description})
				}
				return a.table("REVISION\tSTATUS\tPACKAGE\tVERSION\tCREATED\tDESCRIPTION", rows)
			})
		},
	}
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
	return &cobra.Command{
		Use:   "status <release>",
		Short: "Compare a release's desired state with the runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withService(cmd.Context(), func(s *releases.Service) error {
				rep, err := s.Status(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, rep)
				}
				r := rep.Release
				_, _ = fmt.Fprintf(a.stdout, "Release:  %s\nStatus:   %s\nRevision: %d\nPackage:  %s %s\nUpdated:  %s\n\n",
					r.Name, r.Status, r.CurrentRevision, r.PackageName, r.PackageVersion, ago(r.UpdatedAt))
				var rows [][]string
				for _, x := range rep.Resources {
					health := x.Health
					if x.Backend != "" {
						health = "-> " + x.Backend
					}
					rows = append(rows, []string{x.Kind, x.Name, x.RuntimeName, strconv.Itoa(x.Revision), x.Desired, x.Actual, health})
				}
				if err := a.table("KIND\tNAME\tRUNTIME NAME\tREVISION\tDESIRED\tACTUAL\tHEALTH/BACKEND", rows); err != nil {
					return err
				}
				if len(rep.Orphans) > 0 {
					_, _ = fmt.Fprintf(a.stdout, "\nNot tracked by karga: %s\n", strings.Join(rep.Orphans, ", "))
				}
				if rep.Interrupted != "" {
					_, _ = fmt.Fprintf(a.stdout, "\nWARNING: operation still marked running: %s\n", rep.Interrupted)
				}
				return nil
			})
		},
	}
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
	cmd.AddCommand(values, manifest)
	return cmd
}
