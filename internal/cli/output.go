package cli

import (
	"encoding/json"
	"io"

	"github.com/hasankaradmir/karga/internal/infrastructure/renderer"
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
