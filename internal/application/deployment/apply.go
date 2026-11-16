package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"encoding/json"

	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

// DefaultTimeout bounds how long Execute waits for workloads to become ready.
const DefaultTimeout = 5 * time.Minute

// Step statuses reported to ExecRequest.OnStep.
const (
	StepDone   = "done"
	StepFailed = "failed"
)

// ExecRequest applies Plan, computed from Resources against the applied state, as revision Revision.
type ExecRequest struct {
	Home      string
	Release   string
	Revision  int
	Supported []string
	// Resources is the full desired manifest (pre-flight and materialization need all of it).
	Resources []resource.Resource
	Plan      Plan
	// Redact receives every secret value before anything is logged.
	Redact  func(string)
	Timeout time.Duration
	// OnStep is called after every runtime step so the caller can record applied state and progress.
	OnStep func(Change, release.Step) error
}

// RuntimeName is the runtime object name of a release-local resource name.
func RuntimeName(release, name string) string { return release + "-" + name }

func labels(release string, revision int, kind, name string) map[string]string {
	return map[string]string{
		ports.LabelManaged:  "true",
		ports.LabelRelease:  release,
		ports.LabelKind:     kind,
		ports.LabelName:     name,
		ports.LabelRevision: strconv.Itoa(revision),
	}
}

// Execute applies a plan with the recreate strategy (D2):
//  1. pre-flight, materialize files, and ensure every needed image before touching anything running;
//  2. delete removed workloads (frees their ports);
//  3. create and update in dependency order; a workload update is stop, delete, create, start;
//  4. wait for changed workloads; ones that do not become ready are stopped, not removed (D14);
//  5. delete removed networks and volumes (volumes only with retention delete).
func Execute(ctx context.Context, rt ports.Runtime, req ExecRequest) error {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.Redact == nil {
		req.Redact = func(string) {}
	}
	if req.OnStep == nil {
		req.OnStep = func(Change, release.Step) error { return nil }
	}
	report := func(c Change, action string, err error) error {
		st := release.Step{Kind: c.Kind, Name: c.Name, Action: action, Status: StepDone}
		if err != nil {
			st.Status, st.Error = StepFailed, err.Error()
		}
		if serr := req.OnStep(c, st); serr != nil {
			return errors.Join(err, serr)
		}
		if err != nil {
			return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
		}
		return nil
	}

	info, err := rt.Info(ctx)
	if err != nil {
		return err
	}
	if err := Preflight(ctx, rt, info, req.Release, req.Supported, req.Resources); err != nil {
		return err
	}
	paths, err := Materialize(req.Home, req.Release, req.Revision, req.Resources, req.Redact)
	if err != nil {
		return err
	}
	for _, c := range req.Plan.Changes {
		if w, ok := workloadOf(c); ok && (c.Action == ActionCreate || c.Action == ActionUpdate) {
			if err := rt.EnsureImage(ctx, w.Image, w.PullPolicy); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
	}
	owned, err := managedObjects(ctx, rt, req.Release)
	if err != nil {
		return err
	}

	for _, c := range req.Plan.Changes {
		if c.Action == ActionDelete && c.Kind == resource.KindWorkload {
			if err := report(c, ActionDelete, removeWorkload(ctx, rt, c.Applied.RuntimeName)); err != nil {
				return err
			}
		}
	}

	var changed []Change
	for _, c := range req.Plan.Changes {
		if c.Action != ActionCreate && c.Action != ActionUpdate {
			continue
		}
		name := RuntimeName(req.Release, c.Name)
		lbl := labels(req.Release, req.Revision, c.Kind, c.Name)
		var err error
		switch s := c.Resource.Spec.(type) {
		case *resource.NetworkSpec:
			err = ownedOK(owned, ports.ObjectNetwork, name,
				rt.CreateNetwork(ctx, ports.NetworkRequest{Name: name, Labels: lbl, Driver: s.Driver, Internal: s.Internal}))
		case *resource.VolumeSpec:
			lbl[ports.LabelRetention] = s.Retention
			err = ownedOK(owned, ports.ObjectVolume, name,
				rt.CreateVolume(ctx, ports.VolumeRequest{Name: name, Labels: lbl, Driver: s.Driver}))
		case *resource.WorkloadSpec:
			err = replaceWorkload(ctx, rt, c, owned, req.Release, s, lbl, paths)
			changed = append(changed, c)
		}
		// Config and Secret have no runtime object: they were materialized above.
		if err := report(c, c.Action, err); err != nil {
			return err
		}
	}

	if err := waitChanged(ctx, rt, req.Release, changed, req.Timeout, report); err != nil {
		return err
	}

	for _, c := range Reverse(req.Plan.Changes) {
		if c.Action != ActionDelete || c.Kind == resource.KindWorkload {
			continue
		}
		var err error
		switch c.Kind {
		case resource.KindNetwork:
			err = rt.DeleteNetwork(ctx, c.Applied.RuntimeName)
		case resource.KindVolume:
			if volumeRetention(c.Applied.Spec) == "delete" {
				err = rt.DeleteVolume(ctx, c.Applied.RuntimeName)
			}
		}
		if err := report(c, ActionDelete, err); err != nil {
			return err
		}
	}
	return nil
}

func workloadOf(c Change) (*resource.WorkloadSpec, bool) {
	if c.Resource == nil {
		return nil, false
	}
	w, ok := c.Resource.Spec.(*resource.WorkloadSpec)
	return w, ok
}

func volumeRetention(spec []byte) string {
	var v struct {
		Spec resource.VolumeSpec `json:"spec"`
	}
	_ = json.Unmarshal(spec, &v)
	return v.Spec.Retention
}

// managedObjects returns "kind/name" of every runtime object labelled as belonging to release.
func managedObjects(ctx context.Context, rt ports.Runtime, rel string) (map[string]bool, error) {
	objs, err := rt.ListManaged(ctx, rel)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range objs {
		out[o.Kind+"/"+o.Name] = true
	}
	return out, nil
}

// ownedOK accepts an existing network or volume only if it already belongs to this release
// (volumes are reused across reinstalls on purpose). Anything else with the name is an error.
func ownedOK(owned map[string]bool, kind, name string, err error) error {
	if !errors.Is(err, ports.ErrConflict) {
		return err
	}
	if owned[kind+"/"+name] {
		return nil
	}
	return fmt.Errorf("%s %s already exists and is not managed by this release: %w", kind, name, err)
}

func removeWorkload(ctx context.Context, rt ports.Runtime, name string) error {
	if err := rt.StopWorkload(ctx, name, stopTimeout); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return err
	}
	return rt.DeleteWorkload(ctx, name)
}

// replaceWorkload recreates a workload: the old container goes first, so ports and names are free.
func replaceWorkload(ctx context.Context, rt ports.Runtime, c Change, owned map[string]bool, rel string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string) error {
	name := RuntimeName(rel, c.Name)
	if c.Applied != nil {
		if err := removeWorkload(ctx, rt, c.Applied.RuntimeName); err != nil {
			return err
		}
	}
	wr, err := workloadRequest(rel, c.Name, s, lbl, paths)
	if err != nil {
		return err
	}
	err = rt.CreateWorkload(ctx, wr)
	if errors.Is(err, ports.ErrConflict) && owned[ports.ObjectWorkload+"/"+name] {
		// Left over from an interrupted operation of this release.
		if err = removeWorkload(ctx, rt, name); err == nil {
			err = rt.CreateWorkload(ctx, wr)
		}
	}
	if err != nil {
		return err
	}
	return rt.StartWorkload(ctx, name)
}

// waitChanged waits for every changed workload. On failure the ones that are not ready are stopped
// (kept for inspection) and reported as failed, so the next plan recreates them.
func waitChanged(ctx context.Context, rt ports.Runtime, rel string, changed []Change, timeout time.Duration, report func(Change, string, error) error) error {
	names := make([]string, len(changed))
	for i, c := range changed {
		names[i] = RuntimeName(rel, c.Name)
	}
	werr := waitReady(ctx, rt, names, timeout)
	if werr == nil {
		return nil
	}
	errs := []error{werr}
	for _, c := range changed {
		name := RuntimeName(rel, c.Name)
		st, err := rt.GetWorkload(ctx, name)
		if err == nil && st.State == ports.StateRunning && st.Health != ports.HealthUnhealthy && st.Health != ports.HealthStarting {
			continue
		}
		if err := rt.StopWorkload(ctx, name, stopTimeout); err != nil && !errors.Is(err, ports.ErrNotFound) {
			errs = append(errs, err)
		}
		_ = report(c, "wait", werr)
	}
	return errors.Join(errs...)
}

func workloadRequest(release, name string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string) (ports.WorkloadRequest, error) {
	wr := ports.WorkloadRequest{
		Name:          RuntimeName(release, name),
		Labels:        lbl,
		Image:         s.Image,
		Command:       s.Command,
		Args:          s.Args,
		Env:           s.Environment,
		Aliases:       []string{name},
		RestartPolicy: s.RestartPolicy,
	}
	for _, p := range s.Ports {
		wr.Ports = append(wr.Ports, ports.PortBinding{ContainerPort: p.ContainerPort, HostPort: p.HostPort, HostIP: p.HostIP, Protocol: p.Protocol})
	}
	for _, n := range s.Networks {
		wr.Networks = append(wr.Networks, RuntimeName(release, n))
	}
	for _, m := range s.Mounts {
		kind, target := m.Target()
		if kind == resource.KindVolume {
			wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountVolume, Source: RuntimeName(release, target), Target: m.MountPath, ReadOnly: m.ReadOnly})
			continue
		}
		dir, ok := paths[Key{kind, target}]
		if !ok {
			return wr, fmt.Errorf("mount %s: %s %q was not materialized", m.MountPath, kind, target)
		}
		wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountBind, Source: dir, Target: m.MountPath, ReadOnly: true})
	}
	if hc := s.HealthCheck; hc != nil {
		// Durations were validated when the resource was decoded.
		interval, _ := time.ParseDuration(hc.Interval)
		timeout, _ := time.ParseDuration(hc.Timeout)
		wr.HealthCheck = &ports.HealthCheck{Command: hc.Command, Interval: interval, Timeout: timeout, Retries: hc.Retries}
	}
	if r := s.Resources; r != nil {
		if r.Memory != "" {
			wr.MemoryBytes, _ = resource.ParseMemory(string(r.Memory))
		}
		if r.CPU != "" {
			wr.MilliCPU, _ = resource.ParseCPU(string(r.CPU))
		}
	}
	return wr, nil
}

// pollInterval is how often waitReady checks workload status.
var pollInterval = 500 * time.Millisecond

// waitReady waits until every workload is healthy (with a healthcheck) or running (without one).
func waitReady(ctx context.Context, rt ports.Runtime, names []string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	pending := slices.Clone(names)
	var last ports.WorkloadStatus
	for {
		var still []string
		for _, n := range pending {
			st, err := rt.GetWorkload(ctx, n)
			if err != nil {
				return err
			}
			switch {
			case st.State == ports.StateExited || st.State == "dead":
				return fmt.Errorf("workload %s exited with code %d before becoming ready", n, st.ExitCode)
			case st.Health == ports.HealthUnhealthy:
				return fmt.Errorf("workload %s is unhealthy", n)
			case st.State == ports.StateRunning && (st.Health == ports.HealthHealthy || st.Health == ports.HealthNone):
			default:
				still, last = append(still, n), st
			}
		}
		if pending = still; len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("workload %s not ready after %s (state %q, health %q)", last.Name, timeout, last.State, last.Health)
		case <-time.After(pollInterval):
		}
	}
}

// stopTimeout gives workloads a graceful shutdown before they are removed.
const stopTimeout = 10 * time.Second

// Delete removes every managed object of release: workloads first, then networks. Volumes are kept
// unless deleteVolumes is set or the volume's retention is delete. Materialized files under home are removed.
func Delete(ctx context.Context, rt ports.Runtime, home, release string, deleteVolumes bool) error {
	objs, err := rt.ListManaged(ctx, release)
	if err != nil {
		return err
	}
	var errs []error
	for _, kind := range []string{ports.ObjectWorkload, ports.ObjectNetwork, ports.ObjectVolume} {
		for _, o := range objs {
			if o.Kind != kind {
				continue
			}
			switch kind {
			case ports.ObjectWorkload:
				if err := rt.StopWorkload(ctx, o.Name, stopTimeout); err != nil && !errors.Is(err, ports.ErrNotFound) {
					errs = append(errs, err)
				}
				errs = append(errs, rt.DeleteWorkload(ctx, o.Name))
			case ports.ObjectNetwork:
				errs = append(errs, rt.DeleteNetwork(ctx, o.Name))
			case ports.ObjectVolume:
				if deleteVolumes || o.Labels[ports.LabelRetention] == "delete" {
					errs = append(errs, rt.DeleteVolume(ctx, o.Name))
				}
			}
		}
	}
	if home != "" {
		errs = append(errs, os.RemoveAll(filepath.Join(home, "releases", release)))
	}
	return errors.Join(errs...)
}
