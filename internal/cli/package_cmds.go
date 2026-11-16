package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/application/packages"
	"github.com/hasankaradmir/karga/internal/infrastructure/renderer"
)

// valueOpts holds the value override flags shared by template, lint, install and upgrade.
type valueOpts struct {
	files      []string
	sets       []string
	setStrings []string
	strict     bool
}

func addValueFlags(cmd *cobra.Command, v *valueOpts) {
	f := cmd.Flags()
	f.StringArrayVarP(&v.files, "values", "f", nil, "values file (repeatable, later files win)")
	f.StringArrayVar(&v.sets, "set", nil, "set values on the command line (a.b=c, repeatable)")
	f.StringArrayVar(&v.setStrings, "set-string", nil, "set string values on the command line (repeatable)")
	f.BoolVar(&v.strict, "strict", false, "reject values not declared in values.schema.json")
}

func (v *valueOpts) options(release string) packages.Options {
	return packages.Options{ReleaseName: release, ValueFiles: v.files, Sets: v.sets, SetStrings: v.setStrings, Strict: v.strict}
}

func newTemplateCmd(a *app) *cobra.Command {
	var vals valueOpts
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "template <release> <package>",
		Short: "Render a package locally without touching the runtime",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if a.output == outputJSON {
				return errors.New("template output is YAML only; -o json is not supported")
			}
			p, err := packages.Load(args[1])
			if err != nil {
				return err
			}
			m, err := packages.Render(p, vals.options(args[0]))
			if err != nil {
				return err
			}
			return m.Print(a.stdout, showSecrets)
		},
	}
	addValueFlags(cmd, &vals)
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "print Secret values instead of <redacted>")
	return cmd
}

type lintResult struct {
	Path   string   `json:"path"`
	Errors []string `json:"errors"`
}

func newLintCmd(a *app) *cobra.Command {
	var vals valueOpts
	cmd := &cobra.Command{
		Use:   "lint <package>...",
		Short: "Check that packages are valid and render",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			results := make([]lintResult, 0, len(args))
			failed := 0
			for _, path := range args {
				issues := packages.Lint(path, vals.options(""))
				if issues == nil {
					issues = []string{}
				}
				if len(issues) > 0 {
					failed++
				}
				results = append(results, lintResult{Path: path, Errors: issues})
			}
			if a.output == outputJSON {
				if err := writeJSON(a.stdout, results); err != nil {
					return err
				}
			} else {
				for _, r := range results {
					_, _ = fmt.Fprintf(a.stdout, "==> Linting %s\n", r.Path)
					for _, e := range r.Errors {
						_, _ = fmt.Fprintf(a.stdout, "[ERROR] %s\n", e)
					}
				}
				_, _ = fmt.Fprintf(a.stdout, "\n%d package(s) linted, %d failed\n", len(results), failed)
			}
			if failed > 0 {
				return fmt.Errorf("lint failed for %d package(s)", failed)
			}
			return nil
		},
	}
	addValueFlags(cmd, &vals)
	return cmd
}

func newShowCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show package information",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "package <package>",
			Short: "Show Package.yaml",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				p, err := packages.Load(args[0])
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, p.Metadata)
				}
				out, err := renderer.ToYAML(p.Metadata)
				if err != nil {
					return err
				}
				_, err = a.stdout.Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "values <package>",
			Short: "Show the package's default values.yaml",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				p, err := packages.Load(args[0])
				if err != nil {
					return err
				}
				if a.output == outputJSON {
					return writeJSON(a.stdout, p.Values)
				}
				_, err = a.stdout.Write(p.RawValues)
				return err
			},
		},
		&cobra.Command{
			Use:   "readme <package>",
			Short: "Show the package's README.md",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				p, err := packages.Load(args[0])
				if err != nil {
					return err
				}
				_, err = a.stdout.Write(p.Readme)
				return err
			},
		},
	)
	return cmd
}
