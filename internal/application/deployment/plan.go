package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
)

// Actions in a plan.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionDelete    = "delete"
	ActionUnchanged = "unchanged"
)

// Change is what a plan does to one resource.
type Change struct {
	Kind   string        `json:"kind"`
	Name   string        `json:"name"`
	Action string        `json:"action"`
	Fields []FieldChange `json:"fields,omitempty"`
	// Strategy is how a workload update is applied (recreate or replace); StrategyReason says why
	// a requested replace fell back to recreate.
	Strategy       string `json:"strategy,omitempty"`
	StrategyReason string `json:"strategyReason,omitempty"`
	// Resource is the desired resource (nil for delete).
	Resource *resource.Resource `json:"-"`
	// Spec and Digest describe the desired state to record once applied.
	Spec   json.RawMessage `json:"-"`
	Digest string          `json:"-"`
	// Applied is the current state (nil for create).
	Applied *release.AppliedResource `json:"-"`
}

// FieldChange is one changed field, as JSON values.
type FieldChange struct {
	Path string `json:"path"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
}

// Plan is the ordered set of changes from the applied state to a manifest.
type Plan struct {
	Changes []Change `json:"changes"`
}

// HasChanges reports whether applying the plan would touch the runtime.
func (p Plan) HasChanges() bool {
	for _, c := range p.Changes {
		if c.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// redactedSecret marks secret values in stored specs; only a digest of the value is kept.
func redactedSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Redacted returns r with secret values replaced by digests, for storage and display.
func Redacted(r resource.Resource) resource.Resource {
	s, ok := r.Spec.(*resource.SecretSpec)
	if !ok {
		return r
	}
	masked := &resource.SecretSpec{Values: map[string]string{}}
	for k, v := range s.Values {
		masked.Values[k] = redactedSecret(v)
	}
	r.Spec = masked
	return r
}

// withoutStrategy drops updateStrategy: it decides how a change is applied, not what runs.
func withoutStrategy(r resource.Resource) resource.Resource {
	if w, ok := r.Spec.(*resource.WorkloadSpec); ok && w.UpdateStrategy != nil {
		c := *w
		c.UpdateStrategy = nil
		r.Spec = &c
	}
	return r
}

// Specs returns the canonical redacted JSON and digest of every resource. A workload's digest also
// covers the Config and Secret resources it mounts, so changing their content recreates it.
func Specs(rs []resource.Resource) (map[Key]release.AppliedResource, error) {
	out := map[Key]release.AppliedResource{}
	for _, pass := range []bool{false, true} { // non-workloads first, workloads need their digests
		for _, r := range rs {
			w, isWorkload := r.Spec.(*resource.WorkloadSpec)
			if isWorkload != pass {
				continue
			}
			js, err := json.Marshal(withoutStrategy(Redacted(r)))
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			h.Write(js)
			if isWorkload {
				for _, m := range w.Mounts {
					kind, name := m.Target()
					if kind != resource.KindVolume {
						_, _ = fmt.Fprintf(h, "\n%s/%s=%s", kind, name, out[Key{kind, name}].Digest) // hash writes never fail
					}
				}
			}
			out[keyOf(r)] = release.AppliedResource{
				Kind: r.Kind, Name: r.Metadata.Name, Spec: js, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)),
			}
		}
	}
	return out, nil
}

// Diff compares the desired manifest with what is applied. Changing a Network or Volume is refused:
// it would mean recreating it, which breaks attached workloads or destroys data. strategy overrides
// the workloads' updateStrategy when not empty.
func Diff(releaseName string, desired []resource.Resource, applied []release.AppliedResource, strategy string) (Plan, error) {
	if strategy != "" && strategy != resource.StrategyRecreate && strategy != resource.StrategyReplace {
		return Plan{}, fmt.Errorf("unknown strategy %q (want recreate or replace)", strategy)
	}
	specs, err := Specs(desired)
	if err != nil {
		return Plan{}, err
	}
	current := map[Key]release.AppliedResource{}
	for _, a := range applied {
		current[Key{a.Kind, a.Name}] = a
	}
	ordered, err := Order(desired)
	if err != nil {
		return Plan{}, err
	}

	var plan Plan
	var errs []string
	for _, r := range ordered {
		r := r
		k := keyOf(r)
		want := specs[k]
		c := Change{Kind: k.Kind, Name: k.Name, Resource: &r, Spec: want.Spec, Digest: want.Digest}
		have, exists := current[k]
		switch {
		case !exists:
			c.Action = ActionCreate
		case have.Digest == want.Digest:
			c.Action = ActionUnchanged
		default:
			c.Action, c.Applied = ActionUpdate, &have
			c.Fields = fieldChanges(k.Kind, have.Spec, want.Spec)
			if len(c.Fields) == 0 {
				c.Fields = []FieldChange{{Path: "(mounted Config/Secret content or previous failure)"}}
			}
			if w, ok := r.Spec.(*resource.WorkloadSpec); ok {
				c.Strategy, c.StrategyReason = strategyFor(w, strategy)
			}
			if k.Kind == resource.KindNetwork || k.Kind == resource.KindVolume {
				errs = append(errs, fmt.Sprintf("changing %s %q is not supported (uninstall the release or rename the resource)", k.Kind, k.Name))
			}
		}
		plan.Changes = append(plan.Changes, c)
		delete(current, k)
	}
	var removed []release.AppliedResource
	for _, a := range current {
		removed = append(removed, a)
	}
	sort.Slice(removed, func(i, j int) bool {
		if kindRank[removed[i].Kind] != kindRank[removed[j].Kind] {
			return kindRank[removed[i].Kind] > kindRank[removed[j].Kind] // workloads first
		}
		return removed[i].Name < removed[j].Name
	})
	for _, a := range removed {
		a := a
		plan.Changes = append(plan.Changes, Change{Kind: a.Kind, Name: a.Name, Action: ActionDelete, Applied: &a})
	}
	if len(errs) > 0 {
		return plan, fmt.Errorf("release %s: %s", releaseName, strings.Join(errs, "; "))
	}
	return plan, nil
}

// strategyFor decides how a workload update is applied. replace runs the new container next to the
// old one, which is only safe without host ports (two containers cannot bind one), without
// read-write volumes (two writers on the same data) and with a healthcheck (to know it is ready).
func strategyFor(w *resource.WorkloadSpec, override string) (string, string) {
	requested, _ := w.Strategy()
	if override != "" {
		requested = override
	}
	if requested != resource.StrategyReplace {
		return resource.StrategyRecreate, ""
	}
	for _, p := range w.Ports {
		if p.HostPort != 0 {
			return resource.StrategyRecreate, fmt.Sprintf("publishes hostPort %d", p.HostPort)
		}
	}
	for _, m := range w.Mounts {
		if m.Volume != "" && !m.ReadOnly {
			return resource.StrategyRecreate, fmt.Sprintf("mounts volume %s read-write", m.Volume)
		}
	}
	if w.HealthCheck == nil {
		return resource.StrategyRecreate, "no healthCheck to verify readiness"
	}
	return resource.StrategyReplace, ""
}

// Label returns the strategy shown next to a change, e.g. "replace" or "recreate: mounts volume x read-write".
func (c Change) Label() string {
	if c.StrategyReason != "" {
		return c.Strategy + ": " + c.StrategyReason
	}
	return c.Strategy
}

// fieldChanges lists leaf differences between two resource JSON documents. Lists compare as a whole.
func fieldChanges(kind string, oldJS, newJS json.RawMessage) []FieldChange {
	var o, n any
	_ = json.Unmarshal(oldJS, &o)
	_ = json.Unmarshal(newJS, &n)
	flatOld, flatNew := map[string]string{}, map[string]string{}
	flatten("", o, flatOld)
	flatten("", n, flatNew)
	paths := map[string]bool{}
	for p := range flatOld {
		paths[p] = true
	}
	for p := range flatNew {
		paths[p] = true
	}
	var out []FieldChange
	for p := range paths {
		if flatOld[p] == flatNew[p] {
			continue
		}
		fc := FieldChange{Path: p, Old: flatOld[p], New: flatNew[p]}
		if kind == resource.KindSecret && strings.HasPrefix(p, "spec.values.") {
			fc.Old, fc.New = "", "(changed)"
		}
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func flatten(prefix string, v any, out map[string]string) {
	if m, ok := v.(map[string]any); ok {
		for k, c := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, c, out)
		}
		return
	}
	js, _ := json.Marshal(v)
	out[prefix] = string(js)
}

// Print writes the plan in the tech design §10.2 format.
func (p Plan) Print(w io.Writer, releaseName string, from, to int) error {
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
		case ActionUpdate:
			for _, f := range c.Fields {
				switch {
				case f.Old == "" && f.New == "":
					fmt.Fprintf(&b, "    %s\n", f.Path)
				case f.Old == "":
					fmt.Fprintf(&b, "    %s: %s\n", f.Path, f.New)
				default:
					fmt.Fprintf(&b, "    %s: %s -> %s\n", f.Path, f.Old, f.New)
				}
			}
		default:
			fmt.Fprintf(&b, "    %s\n", c.Action)
		}
	}
	if !p.HasChanges() {
		b.WriteString("\nNo changes.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
