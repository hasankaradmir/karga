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

	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

// DefaultTimeout bounds how long Apply waits for workloads to become ready.
const DefaultTimeout = 5 * time.Minute

// Request describes one deployment of a rendered manifest.
type Request struct {
	Home      string
	Release   string
	Revision  int
	Supported []string
	Resources []resource.Resource
	// Redact receives every secret value before anything is logged.
	Redact  func(string)
	Timeout time.Duration
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

// Apply creates the manifest's resources in dependency order and waits for workloads to be ready.
// Images are ensured before anything is created, so a bad image fails without side effects.
// shortcut: create-only; an existing workload is ErrConflict. Recreate and diff arrive with M3 upgrade.
func Apply(ctx context.Context, rt ports.Runtime, req Request) error {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.Redact == nil {
		req.Redact = func(string) {}
	}
	info, err := rt.Info(ctx)
	if err != nil {
		return err
	}
	if err := Preflight(ctx, rt, info, req.Release, req.Supported, req.Resources); err != nil {
		return err
	}
	ordered, err := Order(req.Resources)
	if err != nil {
		return err
	}
	paths, err := Materialize(req.Home, req.Release, req.Revision, ordered, req.Redact)
	if err != nil {
		return err
	}
	for _, r := range ordered {
		if w, ok := r.Spec.(*resource.WorkloadSpec); ok {
			if err := rt.EnsureImage(ctx, w.Image, w.PullPolicy); err != nil {
				return fmt.Errorf("%s %q: %w", r.Kind, r.Metadata.Name, err)
			}
		}
	}

	var workloads []string
	for _, r := range ordered {
		name := RuntimeName(req.Release, r.Metadata.Name)
		lbl := labels(req.Release, req.Revision, r.Kind, r.Metadata.Name)
		switch s := r.Spec.(type) {
		case *resource.NetworkSpec:
			err = existingOK(rt.CreateNetwork(ctx, ports.NetworkRequest{Name: name, Labels: lbl, Driver: s.Driver, Internal: s.Internal}))
		case *resource.VolumeSpec:
			lbl[ports.LabelRetention] = s.Retention
			err = existingOK(rt.CreateVolume(ctx, ports.VolumeRequest{Name: name, Labels: lbl, Driver: s.Driver}))
		case *resource.WorkloadSpec:
			var wr ports.WorkloadRequest
			if wr, err = workloadRequest(req.Release, r.Metadata.Name, s, lbl, paths); err == nil {
				if err = rt.CreateWorkload(ctx, wr); err == nil {
					err = rt.StartWorkload(ctx, name)
				}
			}
			workloads = append(workloads, name)
		}
		if err != nil {
			return fmt.Errorf("%s %q: %w", r.Kind, r.Metadata.Name, err)
		}
	}
	return waitReady(ctx, rt, workloads, req.Timeout)
}

// existingOK treats an existing network or volume as created: volumes are reused across installs on purpose.
// shortcut: ownership of an existing object is not checked; M3 compares labels before reusing.
func existingOK(err error) error {
	if errors.Is(err, ports.ErrConflict) {
		return nil
	}
	return err
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
			wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountVolume, Source: RuntimeName(release, target), Target: m.MountPath})
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
