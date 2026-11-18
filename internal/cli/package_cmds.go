package cli

import (
	"fmt"
	"os"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/packages"
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

// input reads the values files: karga-server cannot read the client's files.
func (v *valueOpts) input() (api.ValuesInput, error) {
	in := api.ValuesInput{Set: v.sets, SetString: v.setStrings, Strict: v.strict}
	for _, f := range v.files {
		data, err := os.ReadFile(f)
		if err != nil {
			return in, err
		}
		in.Files = append(in.Files, data)
	}
	return in, nil
}

func newTemplateCmd(a *app) *cobra.Command {
	var vals valueOpts
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "template <release> <package>",
		Short: "Render a package locally without touching the runtime",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := packages.Load(args[1])
			if err != nil {
				return err
			}
			m, err := packages.Render(p, vals.options(args[0]))
			if err != nil {
				return err
			}
			docs := m.Documents
			if !showSecrets {
				if docs, err = m.RedactedDocuments(); err != nil {
					return err
				}
			}
			if a.structured() {
				out := make([]map[string]any, len(docs))
				for i, d := range docs {
					var doc map[string]any
					if err := yaml.Unmarshal([]byte(d.Content), &doc); err != nil {
						return fmt.Errorf("templates/%s: %w", d.Source, err)
					}
					out[i] = map[string]any{"source": "templates/" + d.Source, "resource": doc}
				}
				return a.write(out)
			}
			for _, d := range docs {
				if _, err := fmt.Fprintf(a.stdout, "---\n# Source: %s/templates/%s\n%s\n", m.Package.Name, d.Source, d.Content); err != nil {
					return err
				}
			}
			return nil
		},
	}
	addValueFlags(cmd, &vals)
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "print secret values (Secret resources and writeOnly values) instead of <redacted>")
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
			if a.structured() {
				if err := a.write(results); err != nil {
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
				if a.structured() {
					return a.write(p.Metadata)
				}
				out, err := toYAML(p.Metadata)
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
				if a.structured() {
					return a.write(p.Values)
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
				if a.structured() {
					return a.write(map[string]string{"readme": string(p.Readme)})
				}
				_, err = a.stdout.Write(p.Readme)
				return err
			},
		},
	)
	return cmd
}

func newCreateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "create <dir>",
		Short: "Create a new package to start from (a web service with health checks and notes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := createPackage(args[0]); err != nil {
				return err
			}
			return a.report(map[string]string{"package": args[0]}, "Created package %s. Try: karga lint %s && karga install <release> %s\n", args[0], args[0], args[0])
		},
	}
}
