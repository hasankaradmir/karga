package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/hasankaradmir/karga-sdk/api"
)

// planCounts returns how many changes create, update and delete resources, and how many are unchanged.
func planCounts(p api.Plan) (create, update, del, unchanged int) {
	for _, c := range p.Changes {
		switch c.Action {
		case api.ActionCreate:
			create++
		case api.ActionUpdate:
			update++
		case api.ActionDelete:
			del++
		default:
			unchanged++
		}
	}
	return
}

var actionSymbol = map[string]string{api.ActionCreate: "+", api.ActionUpdate: "~", api.ActionDelete: "-"}

// printPlanSummary writes one line per change (+ create, ~ update, - delete; unchanged resources are left
// out) and the counts. printPlan shows the changed fields too.
func printPlanSummary(w io.Writer, p api.Plan, releaseName string, from, to int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Deployment Plan\n\nRelease:  %s\n", releaseName)
	if from == 0 {
		fmt.Fprintf(&b, "Revision: %d\n\n", to)
	} else {
		fmt.Fprintf(&b, "Revision: %d -> %d\n\n", from, to)
	}
	for _, c := range p.Changes {
		sym, ok := actionSymbol[c.Action]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "  %s %s %s", sym, strings.ToUpper(c.Kind), c.Name)
		if c.Strategy != "" {
			fmt.Fprintf(&b, " (%s)", c.Label())
		}
		if n := c.Notice; n != "" {
			fmt.Fprintf(&b, " (%s)", n)
		}
		b.WriteString("\n")
	}
	create, update, del, unchanged := planCounts(p)
	if !p.HasChanges() {
		b.WriteString("  No changes.\n")
	}
	fmt.Fprintf(&b, "\n%d to create, %d to update, %d to delete, %d unchanged.", create, update, del, unchanged)
	if update > 0 {
		b.WriteString(" --verbose shows the changed fields.")
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// printPlan writes the plan with every changed field.
func printPlan(w io.Writer, p api.Plan, releaseName string, from, to int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Deployment Plan\n\nRelease: %s\n", releaseName)
	if from == 0 {
		fmt.Fprintf(&b, "Revision: %d\n", to)
	} else {
		fmt.Fprintf(&b, "Revision: %d -> %d\n", from, to)
	}
	b.WriteString("\nChanges:\n")
	for _, c := range p.Changes {
		if c.Strategy != "" {
			fmt.Fprintf(&b, "\n  %s %s (%s)\n", strings.ToUpper(c.Kind), c.Name, c.Label())
		} else {
			fmt.Fprintf(&b, "\n  %s %s\n", strings.ToUpper(c.Kind), c.Name)
		}
		switch c.Action {
		case api.ActionUpdate:
			for _, f := range c.Fields {
				writeField(&b, "    ", f)
			}
		default:
			if n := c.Notice; n != "" {
				fmt.Fprintf(&b, "    %s (%s)\n", c.Action, n)
			} else {
				fmt.Fprintf(&b, "    %s\n", c.Action)
			}
		}
	}
	if !p.HasChanges() {
		b.WriteString("\nNo changes.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeField writes one changed field: "path: new", "path: old -> new" or "path: old -> (removed)".
func writeField(b *strings.Builder, indent string, f api.FieldChange) {
	switch {
	case f.Old == "" && f.New == "":
		fmt.Fprintf(b, "%s%s\n", indent, f.Path)
	case f.Old == "":
		fmt.Fprintf(b, "%s%s: %s\n", indent, f.Path, f.New)
	case f.New == "":
		fmt.Fprintf(b, "%s%s: %s -> (removed)\n", indent, f.Path, f.Old)
	default:
		fmt.Fprintf(b, "%s%s: %s -> %s\n", indent, f.Path, f.Old, f.New)
	}
}
