package deployment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"encoding/json"

	"github.com/hasankaradmir/karga/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// DefaultTimeout bounds how long Execute waits for workloads to become ready.
const DefaultTimeout = 5 * time.Minute

// Step statuses reported to ExecRequest.OnStep. StepKept means the change failed before anything
// running was touched: the previous state is still applied (a replace whose new container failed).
const (
	StepDone   = "done"
	StepFailed = "failed"
	StepKept   = "kept-previous"
)

// ExecError is returned by Execute. Disrupted is false when the failure happened before any running
// workload was stopped, removed or created: what ran before still runs, unchanged.
type ExecError struct {
	Err       error
	Disrupted bool
}

func (e *ExecError) Error() string { return e.Err.Error() }
func (e *ExecError) Unwrap() error { return e.Err }

// nextSuffix names the container a replace starts next to the current one.
const nextSuffix = "-next"

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
	// OnProgress, if set, receives what is going on as it happens (for display only).
	OnProgress func(Event)
	// Proxies controls the release proxy and the gateway (default: Envoy). ProxyUser is the uid:gid
	// the release proxy runs as (default: the karga process user, so it can use its 0700 directory).
	Proxies   ProxyControl
	ProxyUser string
}

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
//  3. create and update in dependency order. A recreate update is stop, delete, create, start; a
//     replace update starts the new container next to the old one and switches over (replaceStart);
//  4. wait for recreated workloads; ones that do not become ready are stopped, not removed (D14);
//  5. delete removed networks and volumes (volumes only with retention delete).
//
// Errors after the pre-flight phase are *ExecError.
func Execute(ctx context.Context, rt ports.Runtime, req ExecRequest) error {
	ctx = withProgress(ctx, req.OnProgress)
	disrupted := false
	err := execute(ctx, rt, req, &disrupted)
	if err == nil {
		return nil
	}
	if disrupted && proxySupported.Load() {
		// Point the gateway at what runs now; the error that got us here is the one to report.
		_, _ = gateway.Sync(ctx, rt, proxyControl(req), req.Home, gateway.ReleaseRouting(req.Release, workloadNames(req.Resources)))
	}
	return &ExecError{Err: err, Disrupted: disrupted}
}

func proxyControl(req ExecRequest) ProxyControl {
	if req.Proxies == nil {
		return EnvoyProxies{}
	}
	return req.Proxies
}

func execute(ctx context.Context, rt ports.Runtime, req ExecRequest, disrupted *bool) error {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.Redact == nil {
		req.Redact = func(string) {}
	}
	if req.OnStep == nil {
		req.OnStep = func(Change, release.Step) error { return nil }
	}
	req.Proxies = proxyControl(req)
	if req.ProxyUser == "" {
		req.ProxyUser = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	report := func(c Change, action string, err error) error {
		st := release.Step{Kind: c.Kind, Name: c.Name, Action: action, Status: StepDone}
		if err != nil {
			st.Status, st.Error = StepFailed, err.Error()
			progress(ctx, c.Kind, c.Name, PhaseFailed, "%s: %v", action, err)
		} else {
			progress(ctx, c.Kind, c.Name, PhaseDone, "%s", action)
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
	if err := preflightLocked(ctx, rt, info, req); err != nil {
		return err
	}
	paths, err := Materialize(req.Home, req.Release, req.Revision, req.Resources, req.Redact)
	if err != nil {
		return err
	}
	behind, _ := proxied(req.Resources)
	needGateway := false
	for _, c := range req.Plan.Changes {
		if c.Action != ActionCreate && c.Action != ActionUpdate {
			continue
		}
		if w, ok := workloadOf(c); ok {
			needGateway = needGateway || len(gateway.Ports(w)) > 0
			progress(ctx, c.Kind, c.Name, PhasePull, "image %s (%s)", w.Image, w.PullPolicy)
			if err := rt.EnsureImage(ctx, w.Image, w.PullPolicy); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
		if c.Kind == KindProxy {
			if err := rt.EnsureImage(ctx, c.Proxy.Image, "if-not-present"); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
	}
	if needGateway {
		if err := rt.EnsureImage(ctx, envoy.Image(), "if-not-present"); err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
	}
	owned, err := managedObjects(ctx, rt, req.Release)
	if err != nil {
		return err
	}

	for _, c := range req.Plan.Changes {
		if c.Action == ActionDelete && c.Kind == resource.KindWorkload {
			*disrupted = true
			if err := report(c, ActionDelete, removeWorkload(ctx, rt, c.Applied.RuntimeName)); err != nil {
				return err
			}
		}
	}

	// Proxies that go away or are recreated release the workloads' aliases first.
	for _, c := range req.Plan.Changes {
		if c.Kind == KindProxy && (c.Action == ActionDelete || c.Strategy == proxyRecreate) {
			*disrupted = true
			if err := report(c, ActionDelete, removeProxy(ctx, rt, req.Home, req.Release, c, c.Action == ActionDelete)); err != nil {
				return err
			}
		}
	}

	var changed []Change
	for _, c := range req.Plan.Changes {
		if c.Kind == KindProxy || (c.Action != ActionCreate && c.Action != ActionUpdate) {
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
			if c.Action == ActionUpdate && c.Strategy == resource.StrategyReplace {
				var touched bool
				var rerr error
				touched, rerr = replaceWorkload(ctx, rt, req, c, owned, s, lbl, paths, behind)
				*disrupted = *disrupted || touched
				if rerr != nil && !touched {
					_ = req.OnStep(c, release.Step{Kind: c.Kind, Name: c.Name, Action: c.Action, Status: StepKept, Error: rerr.Error()})
					err := fmt.Errorf("%s %q: %w (the previous container is still running)", c.Kind, c.Name, rerr)
					// Workloads recreated earlier in this plan are waited for like on success: ones that do not
					// become ready are stopped and reported failed, so the next plan recreates them.
					if len(changed) > 0 {
						err = errors.Join(err, waitChanged(ctx, rt, req.Release, changed, req.Timeout, report))
					}
					return err
				}
				err = rerr
				break
			}
			*disrupted = true
			err = recreateWorkload(ctx, rt, c, owned, req.Release, s, lbl, paths, behind[c.Name] != nil)
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

	// The proxy starts or changes once its workloads are ready, pointing at what runs now.
	waitAll := false
	for _, c := range req.Plan.Changes {
		if c.Kind != KindProxy || (c.Action != ActionCreate && c.Action != ActionUpdate) {
			continue
		}
		*disrupted, waitAll = true, true
		var err error
		if c.Action == ActionCreate || c.Strategy == proxyRecreate {
			err = createProxy(ctx, rt, req.Proxies, req, c, behind)
		} else {
			err = reconnectProxy(ctx, rt, req.Release, c)
		}
		if err := report(c, c.Action, err); err != nil {
			return err
		}
	}
	if len(behind) > 0 {
		if err := writeProxy(ctx, rt, req.Proxies, req, behind, nil); err != nil {
			return err
		}
		want := map[string]string{}
		for _, c := range req.Plan.Changes {
			if w := behind[c.Name]; w != nil && c.Kind == resource.KindWorkload && (waitAll || c.Action != ActionUnchanged) {
				if rev, err := revisionOf(ctx, rt, RuntimeName(req.Release, c.Name)); err == nil {
					maps.Copy(want, proxyWant(c.Name, w, BackendAlias(c.Name, rev)))
				}
			}
		}
		if len(want) > 0 {
			if err := envoyErr(ctx, rt, ProxyName(req.Release), req.Proxies.WaitReady(ctx, ProxyDir(req.Home, req.Release), want, proxyReadyTimeout)); err != nil {
				return err
			}
		}
	}
	if proxySupported.Load() {
		progress(ctx, "Gateway", gateway.ContainerName, PhaseSwitch, "updating the host gateway")
		targets, err := gateway.Sync(ctx, rt, req.Proxies, req.Home, gateway.ReleaseRouting(req.Release, workloadNames(req.Resources)))
		if err == nil {
			err = envoyErr(ctx, rt, gateway.ContainerName, gateway.Wait(ctx, req.Proxies, req.Home, req.Release, targets))
		}
		if err != nil {
			*disrupted = true
			return fmt.Errorf("gateway: %w", err)
		}
	}

	for _, c := range Reverse(req.Plan.Changes) {
		if c.Action != ActionDelete || c.Kind == resource.KindWorkload || c.Kind == KindProxy {
			continue
		}
		var err error
		switch c.Kind {
		case resource.KindNetwork:
			*disrupted = true
			err = rt.DeleteNetwork(ctx, c.Applied.RuntimeName)
		case resource.KindVolume:
			if volumeRetention(c.Applied.Spec) == "delete" {
				*disrupted = true
				err = rt.DeleteVolume(ctx, c.Applied.RuntimeName)
			}
		}
		if err := report(c, ActionDelete, err); err != nil {
			return err
		}
	}
	return nil
}

// preflightLocked runs Preflight under the gateway lock: another karga process could otherwise
// bind a gateway port between this one listing the gateway's claims and test-binding the port.
func preflightLocked(ctx context.Context, rt ports.Runtime, info ports.RuntimeInfo, req ExecRequest) error {
	if proxySupported.Load() {
		unlock, err := gateway.Lock(req.Home)
		if err != nil {
			return err
		}
		defer unlock()
	}
	return Preflight(ctx, rt, info, req.Release, req.Supported, req.Resources)
}

func workloadNames(rs []resource.Resource) []string {
	var out []string
	for _, r := range rs {
		if r.Kind == resource.KindWorkload {
			out = append(out, r.Metadata.Name)
		}
	}
	return out
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

// recreateWorkload recreates a workload: the old container goes first, so ports and names are free.
// Its anonymous volumes (image VOLUME paths no Volume covers) move to the new container, as docker
// compose does, so their data survives the update.
func recreateWorkload(ctx context.Context, rt ports.Runtime, c Change, owned map[string]bool, rel string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, behind bool) error {
	name := RuntimeName(rel, c.Name)
	wr, err := workloadRequest(rel, c.Name, s, lbl, paths, behind)
	if err != nil {
		return err
	}
	if c.Applied != nil {
		progress(ctx, c.Kind, c.Name, PhaseStop, "stopping and removing %s", c.Applied.RuntimeName)
		if wr.Mounts, err = takeOver(ctx, rt, c.Applied.RuntimeName, wr.Mounts); err != nil {
			return err
		}
	}
	progress(ctx, c.Kind, c.Name, PhaseCreate, "creating %s", name)
	err = rt.CreateWorkload(ctx, wr)
	if errors.Is(err, ports.ErrConflict) && owned[ports.ObjectWorkload+"/"+name] {
		// Left over from an interrupted operation of this release; it may hold the moved volumes.
		if wr.Mounts, err = takeOver(ctx, rt, name, wr.Mounts); err == nil {
			err = rt.CreateWorkload(ctx, wr)
		}
	}
	if err != nil {
		return err
	}
	return rt.StartWorkload(ctx, name)
}

// takeOver stops and deletes a container, keeping its anonymous volumes, and returns mounts with
// those volumes added at their paths (unless a mount already uses the path).
func takeOver(ctx context.Context, rt ports.Runtime, name string, mounts []ports.Mount) ([]ports.Mount, error) {
	st, err := rt.GetWorkload(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return mounts, nil
	}
	if err != nil {
		return mounts, err
	}
	if err := rt.StopWorkload(ctx, name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return mounts, err
	}
	if err := rt.DeleteWorkload(ctx, name); err != nil {
		return mounts, err
	}
	for _, a := range st.AnonymousVolumes {
		if !slices.ContainsFunc(mounts, func(m ports.Mount) bool { return m.Target == a.Target }) {
			mounts = append(mounts, ports.Mount{Type: ports.MountVolume, Source: a.Source, Target: a.Target})
		}
	}
	return mounts, nil
}

// replaceWorkload updates a workload without downtime (tech design §11 "replace", ADR-010):
//  1. remove a leftover <name>-next of this release;
//  2. create <name>-next with fresh gateway upstream ports and, behind the release proxy, its
//     revision's backend alias (nobody resolves it yet); otherwise without aliases;
//  3. start it and wait until healthy; on failure stop it (kept for inspection) and return untouched;
//  4. switch network clients: point the proxy at the new alias and wait until it routes there
//     (Envoy drops a cluster whose name does not resolve, so only now); without the proxy, connect
//     <name>-next with the workload's alias so DNS resolves to both;
//  5. switch host clients: route the gateway to <name>-next and wait until it does;
//     a failure in 4 or 5 restores the previous routing and stops <name>-next;
//  6. wait until the proxies drained the old container (at most the drain timeout each), stop it
//     (gracefully, with the drain timeout when DNS clients may still use it), remove it, and
//     rename <name>-next to the canonical name.
//
// touched reports whether anything that was serving traffic has been changed.
func replaceWorkload(ctx context.Context, rt ports.Runtime, req ExecRequest, c Change, owned map[string]bool,
	s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, set map[string]*resource.WorkloadSpec) (touched bool, err error) {
	name := RuntimeName(req.Release, c.Name)
	next := name + nextSuffix
	behind := set[c.Name] != nil
	if owned[ports.ObjectWorkload+"/"+next] {
		if err := removeWorkload(ctx, rt, next); err != nil {
			return false, err
		}
	}
	wr, err := workloadRequest(req.Release, c.Name, s, lbl, paths, behind)
	if err != nil {
		return false, err
	}
	aliases := wr.Aliases
	wr.Name = next
	if !behind {
		wr.Aliases = nil
	}
	progress(ctx, c.Kind, c.Name, PhaseCreate, "starting %s next to %s", next, c.Applied.RuntimeName)
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		return false, err
	}
	if err := rt.StartWorkload(ctx, next); err != nil {
		return false, err
	}
	stopNext := func(cause error) error {
		if serr := rt.StopWorkload(ctx, next, 0); serr != nil && !errors.Is(serr, ports.ErrNotFound) {
			return errors.Join(cause, serr)
		}
		return cause
	}
	if _, err := waitReady(ctx, rt, []readyTarget{{next, s}}, req.Timeout); err != nil {
		return false, stopNext(err)
	}

	revertProxy := func() error { return nil }
	progress(ctx, c.Kind, c.Name, PhaseSwitch, "switching traffic to %s", next)
	if behind {
		alias := BackendAlias(c.Name, req.Revision)
		if err := writeProxy(ctx, rt, req.Proxies, req, set, map[string]string{c.Name: alias}); err != nil {
			return false, stopNext(err)
		}
		revertProxy = func() error { return writeProxy(ctx, rt, req.Proxies, req, set, nil) }
		if err := envoyErr(ctx, rt, ProxyName(req.Release), req.Proxies.WaitReady(ctx, ProxyDir(req.Home, req.Release), proxyWant(c.Name, s, alias), proxyReadyTimeout)); err != nil {
			// The proxy may have routed to the new backend for a moment: count it as touched.
			return true, stopNext(errors.Join(err, revertProxy()))
		}
	} else {
		for _, n := range wr.Networks {
			if err := rt.DisconnectNetwork(ctx, next, n); err != nil {
				return false, stopNext(err)
			}
			if err := rt.ConnectNetwork(ctx, next, n, aliases); err != nil {
				return true, stopNext(err)
			}
		}
	}

	viaGateway := len(gateway.Ports(s)) > 0
	if viaGateway {
		targets, err := gateway.Sync(ctx, rt, req.Proxies, req.Home, map[string]string{req.Release + "/" + c.Name: next})
		if err == nil {
			want := gateway.Want(targets, func(t gateway.Target) bool { return t.Container == next })
			err = envoyErr(ctx, rt, gateway.ContainerName, req.Proxies.WaitReady(ctx, gateway.Dir(req.Home), want, proxyReadyTimeout))
		}
		if err != nil {
			_, gerr := gateway.Sync(ctx, rt, req.Proxies, req.Home, map[string]string{req.Release + "/" + c.Name: ""})
			return true, stopNext(errors.Join(err, gerr, revertProxy()))
		}
	}

	_, drain := s.Strategy()
	stopWait := max(drain, s.StopGrace())
	progress(ctx, c.Kind, c.Name, PhaseDrain, "waiting up to %s for connections to %s to finish", drain, c.Applied.RuntimeName)
	if behind {
		if _, err := req.Proxies.WaitDrained(ctx, ProxyDir(req.Home, req.Release), proxyClusters(c.Name, s), drain); err != nil {
			return true, err
		}
		stopWait = 0 // the container's own stopTimeout
	}
	if viaGateway {
		var clusters []string
		for _, p := range gateway.Ports(s) {
			clusters = append(clusters, gateway.Cluster(req.Release, c.Name, p.ContainerPort))
		}
		if _, err := req.Proxies.WaitDrained(ctx, gateway.Dir(req.Home), clusters, drain); err != nil {
			return true, err
		}
	}
	old := c.Applied.RuntimeName
	progress(ctx, c.Kind, c.Name, PhaseStop, "stopping %s", old)
	if err := rt.StopWorkload(ctx, old, stopWait); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return true, err
	}
	if err := deleteWorkload(ctx, rt, old); err != nil {
		return true, err
	}
	if err := rt.RenameWorkload(ctx, next, name); err != nil {
		return true, err
	}
	if viaGateway {
		// -next is gone under that name: the gateway routes to the canonical container again.
		_, err := gateway.Sync(ctx, rt, req.Proxies, req.Home, map[string]string{req.Release + "/" + c.Name: ""})
		return true, err
	}
	return true, nil
}

// Reconcile repairs what an interrupted replace leaves behind, before the next plan is made:
// with both <name> and <name>-next present the one the proxies route to is kept (the old one
// unless they had switched); with only <name>-next it takes the canonical name and is returned with
// an empty digest so the plan applies the workload again.
func Reconcile(ctx context.Context, rt ports.Runtime, home, rel string, applied []release.AppliedResource) ([]release.AppliedResource, error) {
	owned, err := managedObjects(ctx, rt, rel)
	if err != nil {
		return nil, err
	}
	var repaired []release.AppliedResource
	for _, a := range applied {
		if a.Kind != resource.KindWorkload || !owned[ports.ObjectWorkload+"/"+a.RuntimeName+nextSuffix] {
			continue
		}
		next := a.RuntimeName + nextSuffix
		if owned[ports.ObjectWorkload+"/"+a.RuntimeName] && routedTo(ctx, rt, home, rel, a.Name, next) {
			if err := removeWorkload(ctx, rt, a.RuntimeName); err != nil {
				return nil, err
			}
			delete(owned, ports.ObjectWorkload+"/"+a.RuntimeName)
		}
		if owned[ports.ObjectWorkload+"/"+a.RuntimeName] {
			if err := removeWorkload(ctx, rt, next); err != nil {
				return nil, err
			}
			continue
		}
		if err := rt.RenameWorkload(ctx, next, a.RuntimeName); err != nil {
			return nil, err
		}
		a.Digest = ""
		repaired = append(repaired, a)
	}
	return repaired, nil
}

// waitChanged waits for every changed workload. On failure the ones that are not ready are stopped
// (kept for inspection) and reported as failed, so the next plan recreates them.
func waitChanged(ctx context.Context, rt ports.Runtime, rel string, changed []Change, timeout time.Duration, report func(Change, string, error) error) error {
	targets := make([]readyTarget, len(changed))
	for i, c := range changed {
		w, _ := workloadOf(c)
		targets[i] = readyTarget{RuntimeName(rel, c.Name), w}
	}
	notReady, werr := waitReady(ctx, rt, targets, timeout)
	if werr == nil {
		return nil
	}
	errs := []error{werr}
	for _, c := range changed {
		name := RuntimeName(rel, c.Name)
		st, err := rt.GetWorkload(ctx, name)
		if err == nil && !slices.Contains(notReady, name) && st.State == ports.StateRunning && st.Health != ports.HealthUnhealthy && st.Health != ports.HealthStarting {
			continue
		}
		if err := rt.StopWorkload(ctx, name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
			errs = append(errs, err)
		}
		_ = report(c, "wait", werr)
	}
	return errors.Join(errs...)
}

// workloadRequest builds the container of a workload. Behind the release proxy it answers to its
// revision's backend alias instead of its name (the proxy holds the name). Its gateway ports are
// published on fresh loopback upstream ports.
func workloadRequest(release, name string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, behind bool) (ports.WorkloadRequest, error) {
	wr := ports.WorkloadRequest{
		Name:          RuntimeName(release, name),
		Labels:        lbl,
		Image:         s.Image,
		Command:       s.Command,
		Args:          s.Args,
		Env:           s.Environment,
		Aliases:       []string{name},
		RestartPolicy: s.RestartPolicy,
		StopTimeout:   s.StopGrace(),
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
	if behind {
		rev, _ := strconv.Atoi(lbl[ports.LabelRevision])
		wr.Aliases = []string{BackendAlias(name, rev)}
	}
	certDir := func(secret string) (string, bool) { dir, ok := paths[Key{resource.KindSecret, secret}]; return dir, ok }
	if err := gateway.Label(&wr, s, certDir); err != nil {
		return wr, err
	}
	if hc := s.HealthCheck; hc != nil && hc.Type == resource.HealthExec {
		// Durations were validated when the resource was decoded.
		interval, _ := time.ParseDuration(hc.Interval)
		timeout, _ := time.ParseDuration(hc.Timeout)
		start, _ := time.ParseDuration(hc.StartPeriod)
		wr.HealthCheck = &ports.HealthCheck{Command: hc.Command, Interval: interval, Timeout: timeout, Retries: hc.Retries, StartPeriod: start}
	}
	wr.NoNewPrivileges = true
	if sec := s.Security; sec != nil {
		wr.NoNewPrivileges = sec.NoNewPrivileges == nil || *sec.NoNewPrivileges
		wr.CapDrop, wr.CapAdd, wr.User = sec.CapDrop, sec.CapAdd, sec.User
		if sec.ReadOnlyRootFilesystem {
			wr.ReadOnlyRootfs = true
			wr.Tmpfs = map[string]string{"/tmp": "rw,nosuid,nodev,size=64m"}
		}
	}
	wr.PidsLimit = resource.DefaultPids
	wr.LogMaxSize, wr.LogMaxFiles = resource.DefaultLogMaxSize, resource.DefaultLogMaxFiles
	if l := s.Logging; l != nil && l.MaxSize != "" {
		wr.LogMaxSize, wr.LogMaxFiles = l.MaxSize, l.MaxFiles
	}
	if r := s.Resources; r != nil && r.Pids != nil {
		wr.PidsLimit = *r.Pids
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
				if err := rt.StopWorkload(ctx, o.Name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
					errs = append(errs, err)
				}
				errs = append(errs, deleteWorkload(ctx, rt, o.Name))
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
		errs = append(errs, os.RemoveAll(filepath.Join(home, "releases", release)), os.RemoveAll(ProxyDir(home, release)))
		if proxySupported.Load() {
			var names []string
			for _, o := range objs {
				if n := o.Labels[ports.LabelName]; o.Kind == ports.ObjectWorkload && n != "" {
					names = append(names, n)
				}
			}
			_, err := gateway.Sync(ctx, rt, EnvoyProxies{}, home, gateway.ReleaseRouting(release, names))
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
