package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/config"
	"github.com/hasankaradmir/karga/internal/ports"
)

// Dynamic shell completion reads the state database only (no container runtime) and never creates
// it: on a host without karga state, completion just offers nothing.

// completer offers values for one argument position; args are the arguments before it.
type completer func(a *app, args []string) ([]string, cobra.ShellCompDirective)

func (a *app) withStore(fn func(ports.StateStore) []string) []string {
	cfg, err := config.Resolve(a.home, os.Getenv)
	if err != nil {
		return nil
	}
	path := filepath.Join(cfg.Home, "karga.db")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		return nil
	}
	defer func() { _ = store.Close() }()
	return fn(store)
}

func completeRelease(a *app, _ []string) ([]string, cobra.ShellCompDirective) {
	return a.withStore(func(s ports.StateStore) []string {
		rels, _ := s.ListReleases(context.Background())
		var out []string
		for _, r := range rels {
			out = append(out, r.Name)
		}
		return out
	}), cobra.ShellCompDirectiveNoFileComp
}

// completeResource offers the applied resources of kind of the release in args[0].
func completeResource(kind string) completer {
	return func(a *app, args []string) ([]string, cobra.ShellCompDirective) {
		return a.withStore(func(s ports.StateStore) []string {
			applied, _ := s.ListResources(context.Background(), args[0])
			var out []string
			for _, r := range applied {
				if r.Kind == kind {
					out = append(out, r.Name)
				}
			}
			return out
		}), cobra.ShellCompDirectiveNoFileComp
	}
}

func completeRevision(a *app, args []string) ([]string, cobra.ShellCompDirective) {
	return a.withStore(func(s ports.StateStore) []string {
		revs, _ := s.ListRevisions(context.Background(), args[0])
		var out []string
		for i := len(revs) - 1; i >= 0; i-- {
			out = append(out, strconv.Itoa(revs[i].Number)+"\t"+string(revs[i].Status)+" "+revs[i].PackageVersion)
		}
		return out
	}), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveKeepOrder
}

func completeDir(*app, []string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}

func completeFile(*app, []string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveDefault
}

func completeNothing(*app, []string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// positional completes argument i with byPos[i]; the last completer repeats when repeat is set.
func positional(a *app, repeat bool, byPos ...completer) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		i := len(args)
		if i >= len(byPos) {
			if !repeat {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			i = len(byPos) - 1
		}
		vals, dir := byPos[i](a, args)
		var out []string
		for _, v := range vals {
			if strings.HasPrefix(v, prefix) {
				out = append(out, v)
			}
		}
		return out, dir
	}
}

// addCompletion attaches dynamic completion to the command tree.
func addCompletion(a *app, root *cobra.Command) {
	set := func(path string, repeat bool, byPos ...completer) {
		if cmd, _, err := root.Find(strings.Fields(path)); err == nil && cmd != root {
			cmd.ValidArgsFunction = positional(a, repeat, byPos...)
		}
	}
	workload := completeResource(resource.KindWorkload)
	volume := completeResource(resource.KindVolume)
	set("install", false, completeNothing, completeDir)
	set("upgrade", false, completeRelease, completeDir)
	set("template", false, completeNothing, completeDir)
	set("lint", true, completeDir)
	set("create", false, completeDir)
	set("rollback", false, completeRelease, completeRevision)
	set("diff", false, completeRelease, completeRevision, completeRevision)
	for _, c := range []string{"uninstall", "history", "status", "get values", "get manifest", "get notes"} {
		set(c, false, completeRelease)
	}
	set("logs", true, completeRelease, workload)
	set("exec", false, completeRelease, workload)
	set("volume backup", false, completeRelease, volume, completeFile)
	set("volume restore", false, completeRelease, volume, completeFile)
}
