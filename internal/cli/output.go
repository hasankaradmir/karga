package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/hasankaradmir/karga-sdk/renderer"
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// toYAMLViaJSON prints v as YAML using its JSON field names (resources only have json tags).
func toYAMLViaJSON(v any) ([]byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(js, &generic); err != nil {
		return nil, err
	}
	return renderer.ToYAML(generic)
}

// colorful reports whether tables are colored: --color always, or auto on a terminal without
// NO_COLOR (https://no-color.org).
func (a *app) colorful() bool {
	switch a.color {
	case "always":
		return true
	case "never":
		return false
	}
	return a.output != outputJSON && os.Getenv("NO_COLOR") == "" && isTerminal(a.stdout)
}

// ANSI colors used for state words.
const (
	colorReset  = "\x1b[0m"
	colorRed    = "\x1b[31m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
)

// stateColor picks a color for words karga prints in status columns ("" for none).
func stateColor(cell string) string {
	switch {
	case cell == "":
		return ""
	case strings.Contains(cell, "oom-killed") || strings.Contains(cell, "unhealthy"):
		return colorRed
	}
	switch strings.SplitN(cell, ",", 2)[0] {
	case "healthy", "running", "deployed", "applied", "present", "succeeded", "ready":
		return colorGreen
	case "failed", "missing", "exited", "dead":
		return colorRed
	case "starting", "pending", "deploying", "pending-upgrade", "pending-rollback", "created", "restarting":
		return colorYellow
	}
	return ""
}

// table writes aligned columns separated by two spaces. Cells are padded before they are colored,
// so color codes never shift the columns.
func (a *app) table(header string, rows [][]string) error {
	all := append([][]string{strings.Split(header, "\t")}, rows...)
	var width []int
	for _, r := range all {
		for i, c := range r {
			if i >= len(width) {
				width = append(width, 0)
			}
			width[i] = max(width[i], utf8.RuneCountInString(c))
		}
	}
	color := a.colorful()
	var b strings.Builder
	for n, r := range all {
		for i, c := range r {
			cell := c
			if i < len(r)-1 {
				cell += strings.Repeat(" ", width[i]-utf8.RuneCountInString(c)+2)
			}
			if code := stateColor(c); color && n > 0 && code != "" {
				cell = code + c + colorReset + cell[len(c):]
			}
			b.WriteString(cell)
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(a.stdout, b.String())
	return err
}

// paint colors one state word when tables are colored.
func (a *app) paint(word string) string {
	if code := stateColor(word); code != "" && a.colorful() {
		return code + word + colorReset
	}
	return word
}
