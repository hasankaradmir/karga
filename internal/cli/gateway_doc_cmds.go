package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/api"
)

// addDocumentCmds adds the commands that read and apply the gateway settings as a document (kind
// Gateway), kept by the user wherever they like; karga keeps the revisions applied.
func addDocumentCmds(a *app, cmd *cobra.Command) {
	var rev int
	get := &cobra.Command{
		Use:   "get [--revision N]",
		Short: "Print the gateway settings in force (or revision N) as a document to edit and apply",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := a.backend().ExportGateway(cmd.Context(), api.GatewayExportRequest{Revision: rev})
			if err != nil {
				return err
			}
			if a.output == outputJSON {
				var v any
				if err := yaml.Unmarshal(doc.Content, &v); err != nil {
					return err
				}
				return a.write(v)
			}
			_, err = a.stdout.Write(doc.Content)
			return err
		},
	}
	get.Flags().IntVar(&rev, "revision", 0, "revision to print (default: the one in force)")

	var file string
	diff := &cobra.Command{
		Use:   "diff -f <file|->",
		Short: "Show what applying a gateway document changes; changes nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			content, err := readDocument(cmd, file)
			if err != nil {
				return err
			}
			plan, err := a.backend().DiffGateway(cmd.Context(), api.GatewayDocument{Content: content})
			if err != nil {
				return err
			}
			return a.printGatewayPlan(plan)
		},
	}
	diff.Flags().StringVarP(&file, "file", "f", "", "gateway document (- for stdin)")
	_ = diff.MarkFlagRequired("file")

	apply := &cobra.Command{
		Use:   "apply -f <file|->",
		Short: "Apply a gateway document as a new revision; if the gateway does not route with it, the revision in force stays",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			content, err := readDocument(cmd, file)
			if err != nil {
				return err
			}
			return a.applyDocument(cmd, content, "apply")
		},
	}
	apply.Flags().StringVarP(&file, "file", "f", "", "gateway document (- for stdin)")
	_ = apply.MarkFlagRequired("file")

	cmd.AddCommand(get, diff, apply, &cobra.Command{
		Use:   "history",
		Short: "List the gateway settings revisions kept (the last 10)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			revs, err := a.backend().GatewayHistory(cmd.Context())
			if err != nil {
				return err
			}
			if a.structured() {
				return a.write(revs)
			}
			var rows [][]string
			for i, r := range revs {
				mark := ""
				if i == len(revs)-1 {
					mark = " (in force)"
				}
				rows = append(rows, []string{strconv.Itoa(r.Revision) + mark, r.Time.Local().Format("2006-01-02 15:04:05"), r.Actor, r.Source})
			}
			return a.table("REVISION\tTIME\tACTOR\tSOURCE", rows)
		},
	}, &cobra.Command{
		Use:   "rollback [revision]",
		Short: "Apply an earlier revision of the gateway settings again (default: the one before the one in force)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := a.backend()
			var n int
			if len(args) == 1 {
				var err error
				if n, err = strconv.Atoi(args[0]); err != nil || n < 1 {
					return usageError{fmt.Errorf("revision %q: want a number from karga gateway history", args[0])}
				}
			} else {
				revs, err := s.GatewayHistory(cmd.Context())
				if err != nil {
					return err
				}
				if len(revs) < 2 {
					return fmt.Errorf("no earlier gateway settings revision to roll back to")
				}
				n = revs[len(revs)-2].Revision
			}
			doc, err := s.ExportGateway(cmd.Context(), api.GatewayExportRequest{Revision: n})
			if err != nil {
				return err
			}
			return a.applyDocument(cmd, doc.Content, "rollback "+strconv.Itoa(n))
		},
	})
}

// applyDocument shows what a gateway document changes, asks for its risk, and applies it.
func (a *app) applyDocument(cmd *cobra.Command, content []byte, source string) error {
	s := a.backend()
	plan, err := s.DiffGateway(cmd.Context(), api.GatewayDocument{Content: content})
	if err != nil {
		return err
	}
	if err := a.printGatewayPlan(plan); err != nil || len(plan.Changes) == 0 {
		return err
	}
	if err := a.confirm(cmd, plan.Risk); err != nil {
		return err
	}
	rev, err := s.ApplyGateway(cmd.Context(), api.GatewayApplyRequest{GatewayDocument: api.GatewayDocument{Content: content}, Source: source, Confirmed: true})
	if err != nil {
		return err
	}
	return a.report(rev, "gateway settings applied (revision %d)\n", rev.Revision)
}

// readDocument reads a gateway document from path, or stdin for "-".
func readDocument(cmd *cobra.Command, path string) ([]byte, error) {
	r := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, api.MaxGatewayDocument+1))
	if err == nil && len(data) > api.MaxGatewayDocument {
		err = fmt.Errorf("%s: more than %d bytes", path, api.MaxGatewayDocument)
	}
	return data, err
}
