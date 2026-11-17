// Package releases implements the release lifecycle: install, upgrade, rollback, uninstall and reads.
package releases

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hasankaradmir/karga/internal/application/deployment"
	"github.com/hasankaradmir/karga/internal/domain/pkg"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/values"
	"github.com/hasankaradmir/karga/internal/ports"
)

// Errors callers can check.
var (
	ErrLocked      = errors.New("another operation is running on this release")
	ErrInterrupted = errors.New("a previous operation on this release was interrupted")
	ErrExists      = errors.New("release already exists")
)

// DefaultHistoryMax is how many revisions are kept per release (D13).
const DefaultHistoryMax = 10

// Service runs release use cases. Runtime is opened lazily so read-only commands never need Docker.
type Service struct {
	Store   ports.StateStore
	Runtime func(context.Context) (ports.Runtime, error)
	Home    string
	// Redact registers secret values with the log redactor.
	Redact   func(string)
	Log      *slog.Logger
	Now      func() time.Time
	PID      int
	Hostname string

	keyOnce sync.Once
	key     []byte
	keyErr  error
}

// New returns a Service with process defaults filled in.
func New(store ports.StateStore, rt func(context.Context) (ports.Runtime, error), home string, redact func(string), log *slog.Logger) *Service {
	host, _ := os.Hostname()
	if redact == nil {
		redact = func(string) {}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{Store: store, Runtime: rt, Home: home, Redact: redact, Log: log, Now: time.Now, PID: os.Getpid(), Hostname: host}
}

// CommonOptions apply to every mutating command.
type CommonOptions struct {
	DryRun     bool
	Timeout    time.Duration
	HistoryMax int
	// Force aborts an interrupted operation (whose process is gone) instead of refusing to run.
	Force bool
}

// Result describes what a mutating command did.
type Result struct {
	Release  string         `json:"release"`
	Revision int            `json:"revision"`
	From     int            `json:"from"`
	Status   release.Status `json:"status"`
	DryRun   bool           `json:"dryRun,omitempty"`
	// NoOp is set when an install found the release already deployed with the same package and values.
	NoOp bool            `json:"noop,omitempty"`
	Plan deployment.Plan `json:"plan"`
}

// processAlive reports whether pid runs on this host (signal 0 only checks existence).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func describe(op release.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s of revision %d started %s by pid %d on %s", op.Type, op.Revision, op.StartedAt.Local().Format(time.RFC3339), op.PID, op.Hostname)
	for _, s := range op.Steps {
		fmt.Fprintf(&b, "\n  %s %s %s: %s", s.Action, s.Kind, s.Name, s.Status)
		if s.Error != "" {
			fmt.Fprintf(&b, " (%s)", s.Error)
		}
	}
	return b.String()
}

// begin records a running operation. A live process holding the release always wins; a dead one is
// only overridden with force (D15).
func (s *Service) begin(ctx context.Context, rel, typ string, revision int, force bool) (release.Operation, error) {
	op := release.Operation{Release: rel, Type: typ, Revision: revision, Status: release.OperationRunning,
		PID: s.PID, Hostname: s.Hostname, StartedAt: s.Now()}
	id, err := s.Store.BeginOperation(ctx, op, func(running release.Operation) error {
		if running.Hostname == s.Hostname && processAlive(running.PID) {
			return fmt.Errorf("%w: %s", ErrLocked, describe(running))
		}
		if !force {
			return fmt.Errorf("%w: %s\nInspect with 'karga status %s'; rerun with --force to continue", ErrInterrupted, describe(running), rel)
		}
		s.Log.Warn("aborting interrupted operation", "release", rel, "operation", running.ID)
		return nil
	})
	op.ID = id
	return op, err
}

// finish records the end of an operation. It uses a context that survives cancellation (Ctrl-C) so
// the operation is not left running.
func (s *Service) finish(ctx context.Context, op release.Operation, err error) {
	ctx = context.WithoutCancel(ctx)
	op.Status = release.OperationSucceeded
	if err != nil {
		op.Status, op.Error = release.OperationFailed, err.Error()
	}
	if ferr := s.Store.FinishOperation(ctx, op); ferr != nil {
		s.Log.Error("record operation result", "release", op.Release, "error", ferr)
	}
}

// digestJSON is the keyed digest of v's JSON: values and manifests may hold secret material.
func digestJSON(v any) (string, []byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return deployment.Digest(js), js, nil
}

// keyFile holds the key of the state database's digests of secret material (R7).
const keyFile = "state.key"

// loadKey reads $KARGA_HOME/state.key, creating it (32 random bytes, 0600) on first use, and makes
// it the digest key. Losing it only means secrets are re-applied once.
func (s *Service) loadKey() error {
	s.keyOnce.Do(func() {
		path := filepath.Join(s.Home, keyFile)
		key, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			key = make([]byte, 32)
			if _, err = rand.Read(key); err == nil {
				err = writeNewFile(path, key)
				if errors.Is(err, os.ErrExist) { // another karga created it first
					key, err = os.ReadFile(path)
				}
			}
		}
		if err == nil && len(key) < 16 {
			err = fmt.Errorf("%s is too short; remove it to create a new key", path)
		}
		if err != nil {
			s.keyErr = fmt.Errorf("state key: %w", err)
			return
		}
		s.key = key
	})
	if s.keyErr != nil {
		return s.keyErr
	}
	// shortcut: the digest key is process-wide; services of different homes in one process must
	// not plan concurrently (karga runs one per process). Pass the key down if a daemon needs more.
	deployment.SetDigestKey(s.key)
	return nil
}

// writeNewFile creates path with data atomically: the data is written to a temporary file that is
// then linked into place, so a concurrent reader never sees it half written. It fails with
// os.ErrExist when path exists.
func writeNewFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

// encodeManifest stores resources as a JSON array with secret values replaced by digests.
func encodeManifest(rs []resource.Resource) (json.RawMessage, string, error) {
	redacted := make([]resource.Resource, len(rs))
	for i, r := range rs {
		redacted[i] = deployment.Redacted(r)
	}
	d, js, err := digestJSON(redacted)
	return js, d, err
}

// decodeManifest parses a stored manifest. Secret values are still digests; see restoreSecrets.
func decodeManifest(js json.RawMessage) ([]resource.Resource, error) {
	var docs []json.RawMessage
	if err := json.Unmarshal(js, &docs); err != nil {
		return nil, err
	}
	rs := make([]resource.Resource, len(docs))
	for i, d := range docs {
		r, err := resource.Decode(d) // JSON is YAML
		if err != nil {
			return nil, err
		}
		rs[i] = r
	}
	return rs, nil
}

// restoreSecrets fills secret values from the files materialized for that revision.
func (s *Service) restoreSecrets(rel string, revision int, rs []resource.Resource) error {
	dir := deployment.RevisionDir(s.Home, rel, revision)
	for i, r := range rs {
		sec, ok := r.Spec.(*resource.SecretSpec)
		if !ok {
			continue
		}
		values := map[string]string{}
		for k := range sec.Values {
			b, err := os.ReadFile(filepath.Join(dir, "secrets", r.Metadata.Name, k))
			if err != nil {
				return fmt.Errorf("revision %d: secret %q is no longer available (%v); it may have been pruned", revision, r.Metadata.Name, err)
			}
			values[k] = string(b)
		}
		rs[i].Spec = &resource.SecretSpec{Values: values}
	}
	return nil
}

// deployInput is a desired revision, from install, upgrade or rollback.
type deployInput struct {
	CommonOptions
	Name          string
	Type          string // install, upgrade, rollback
	Description   string
	Package       pkg.Metadata
	PackageDigest string
	Overrides     map[string]any
	Values        map[string]any
	Resources     []resource.Resource
	Revision      int
	// Strategy overrides the workloads' updateStrategy ("" = use the spec).
	Strategy string
	// SecretPaths are writeOnly values: stored as <redacted>, never in plain text.
	SecretPaths [][]string
	// Existing is the current release row, nil for a new release.
	Existing *release.Release
}

func (s *Service) nextRevision(ctx context.Context, name string) (int, error) {
	revs, err := s.Store.ListRevisions(ctx, name)
	if err != nil {
		return 0, err
	}
	if len(revs) == 0 {
		return 1, nil
	}
	return revs[len(revs)-1].Number + 1, nil
}

var inProgress = []release.Status{release.StatusPending, release.StatusDeploying, release.StatusUpgrading, release.StatusUninstalling}

// deploy plans the desired revision against the applied state and executes it.
// runtime connects to the container runtime and tells the deployment package whether it can run
// karga's proxies there.
func (s *Service) runtime(ctx context.Context) (ports.Runtime, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	info, err := rt.Info(ctx)
	if err != nil {
		return nil, err
	}
	deployment.UseProxies(info)
	return rt, nil
}

func (s *Service) deploy(ctx context.Context, in deployInput) (*Result, error) {
	if err := s.loadKey(); err != nil {
		return nil, err
	}
	applied, err := s.Store.ListResources(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	// A dry run plans without a runtime if there is none (as if proxies were unavailable).
	rt, rtErr := s.runtime(ctx)
	if rtErr != nil && !in.DryRun {
		return nil, rtErr
	}
	plan, err := deployment.Diff(in.Name, in.Resources, applied, in.Strategy)
	if err != nil {
		return nil, err
	}
	res := &Result{Release: in.Name, Revision: in.Revision, Plan: plan, DryRun: in.DryRun}
	if in.Existing != nil {
		res.From = in.Existing.CurrentRevision
	}
	if in.DryRun {
		return res, nil
	}

	op, err := s.begin(ctx, in.Name, in.Type, in.Revision, in.Force)
	if err != nil {
		return nil, err
	}
	if plan, err = s.reconcile(ctx, rt, in, applied, plan); err == nil {
		res.Plan = plan
		err = s.run(ctx, rt, &op, in, plan)
	}
	s.finish(ctx, op, err)
	if err != nil {
		res.Status = release.StatusFailed
		hint := fmt.Sprintf("revision %d failed", in.Revision)
		var execErr *deployment.ExecError
		switch {
		case errors.As(err, &execErr) && !execErr.Disrupted && res.From > 0:
			res.Status = in.Existing.Status
			hint += fmt.Sprintf("; nothing that was running changed and revision %d is still serving", res.From)
		case res.From > 0:
			hint += fmt.Sprintf("; revision %d stays current. Inspect with 'karga status %s' (and 'docker logs'), then fix and upgrade or run 'karga rollback %s'", res.From, in.Name, in.Name)
		default:
			hint += fmt.Sprintf("; fix the problem and run 'karga install' again, or remove the failed release with 'karga uninstall %s'", in.Name)
		}
		return res, fmt.Errorf("%w\n%s", err, hint)
	}
	res.Status = release.StatusDeployed
	if perr := s.prune(context.WithoutCancel(ctx), in.Name, in.HistoryMax); perr != nil {
		s.Log.Warn("prune history", "release", in.Name, "error", perr)
	}
	return res, nil
}

// reconcile repairs leftovers of an interrupted replace and, if anything changed, plans again.
func (s *Service) reconcile(ctx context.Context, rt ports.Runtime, in deployInput, applied []release.AppliedResource, plan deployment.Plan) (deployment.Plan, error) {
	repaired, err := deployment.Reconcile(ctx, rt, s.Home, in.Name, applied)
	if err != nil || len(repaired) == 0 {
		return plan, err
	}
	for _, a := range repaired {
		s.Log.Warn("repaired interrupted replace", "release", in.Name, "resource", a.Kind+"/"+a.Name)
		if err := s.Store.SaveResource(ctx, in.Name, a); err != nil {
			return plan, err
		}
	}
	if applied, err = s.Store.ListResources(ctx, in.Name); err != nil {
		return plan, err
	}
	return deployment.Diff(in.Name, in.Resources, applied, in.Strategy)
}

func (s *Service) run(ctx context.Context, rt ports.Runtime, op *release.Operation, in deployInput, plan deployment.Plan) error {
	now := s.Now()
	rel := release.Release{Name: in.Name, PackageName: in.Package.Name, PackageVersion: in.Package.Version,
		Status: release.StatusPending, CreatedAt: now, UpdatedAt: now}
	target := release.StatusDeploying
	if in.Existing != nil {
		rel = *in.Existing
		if slices.Contains(inProgress, rel.Status) {
			// Left behind by an operation that begin just aborted.
			rel.Status = release.StatusFailed
		}
		if in.Type != "install" {
			target = release.StatusUpgrading
		}
	}
	if err := release.Transition(rel.Status, target); err != nil {
		return err
	}
	previous := rel.Status
	rel.Status, rel.UpdatedAt = target, now
	if err := s.Store.SaveRelease(ctx, rel); err != nil {
		return err
	}

	manifest, manifestDigest, err := encodeManifest(in.Resources)
	if err != nil {
		return err
	}
	valuesDigest, _, err := digestJSON(in.Values)
	if err != nil {
		return err
	}
	if err := s.Store.CreateRevision(ctx, release.Revision{
		Release: in.Name, Number: in.Revision, PackageName: in.Package.Name, PackageVersion: in.Package.Version,
		RuntimeSupported: in.Package.Runtime.Supported, PackageDigest: in.PackageDigest, ValuesDigest: valuesDigest, ManifestDigest: manifestDigest,
		Values: values.Mask(in.Overrides, in.SecretPaths), ValuesAll: values.Mask(in.Values, in.SecretPaths), Manifest: manifest,
		Status: release.RevisionPending, Description: in.Description, CreatedAt: now,
	}); err != nil {
		return err
	}

	execErr := deployment.Execute(ctx, rt, deployment.ExecRequest{
		Home: s.Home, Release: in.Name, Revision: in.Revision, Supported: in.Package.Runtime.Supported,
		Resources: in.Resources, Plan: plan, Redact: s.Redact, Timeout: in.Timeout,
		OnStep: func(c deployment.Change, st release.Step) error { return s.record(ctx, op, in, c, st) },
	})

	bg := context.WithoutCancel(ctx)
	rel.UpdatedAt = s.Now()
	if execErr != nil {
		rel.Status = release.StatusFailed
		// A failed replace that touched nothing leaves the release as it was (D18).
		var ee *deployment.ExecError
		if errors.As(execErr, &ee) && !ee.Disrupted && in.Existing != nil {
			rel.Status = previous
		}
		desc := fmt.Sprintf("%s failed: %v", titled(in.Type), execErr)
		return errors.Join(execErr,
			s.Store.UpdateRevisionStatus(bg, in.Name, in.Revision, release.RevisionFailed, desc),
			s.Store.SaveRelease(bg, rel))
	}
	rel.Status, rel.CurrentRevision = release.StatusDeployed, in.Revision
	rel.PackageName, rel.PackageVersion = in.Package.Name, in.Package.Version
	return errors.Join(
		s.Store.UpdateRevisionStatus(bg, in.Name, in.Revision, release.RevisionDeployed, in.Description),
		s.Store.SaveRelease(bg, rel))
}

func titled(t string) string { return strings.ToUpper(t[:1]) + t[1:] }

// record persists one executor step: progress on the operation and the applied state of the resource.
// A failed workload is stored with an empty digest so the next plan recreates it; a kept-previous
// step (failed replace) leaves the applied state alone.
func (s *Service) record(ctx context.Context, op *release.Operation, in deployInput, c deployment.Change, st release.Step) error {
	ctx = context.WithoutCancel(ctx)
	op.Steps = append(op.Steps, st)
	s.Log.Debug("step", "operation_id", op.ID, "release", in.Name, "resource", c.Kind+"/"+c.Name, "action", st.Action, "status", st.Status)
	if err := s.Store.SaveOperationSteps(ctx, op.ID, op.Steps); err != nil {
		return err
	}
	runtimeName := c.RuntimeName
	if runtimeName == "" {
		runtimeName = deployment.RuntimeName(in.Name, c.Name)
	}
	applied := release.AppliedResource{Kind: c.Kind, Name: c.Name, Revision: in.Revision,
		RuntimeName: runtimeName, Spec: c.Spec, Digest: c.Digest}
	switch {
	case st.Status == deployment.StepKept:
		// The change failed before touching anything; the previous state is still what runs.
	case st.Status == deployment.StepDone && c.Action == deployment.ActionDelete:
		return s.Store.DeleteResource(ctx, in.Name, c.Kind, c.Name)
	case st.Status == deployment.StepDone:
		return s.Store.SaveResource(ctx, in.Name, applied)
	case (c.Kind == resource.KindWorkload || c.Kind == deployment.KindProxy) && c.Spec != nil:
		applied.Digest = ""
		return s.Store.SaveResource(ctx, in.Name, applied)
	}
	return nil
}

// prune keeps the newest historyMax revisions. The current revision and any revision whose files are still
// mounted by an applied resource are never removed (D13).
func (s *Service) prune(ctx context.Context, name string, historyMax int) error {
	if historyMax <= 0 {
		return nil
	}
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return err
	}
	revs, err := s.Store.ListRevisions(ctx, name)
	if err != nil {
		return err
	}
	applied, err := s.Store.ListResources(ctx, name)
	if err != nil {
		return err
	}
	keep := map[int]bool{rel.CurrentRevision: true}
	for _, a := range applied {
		keep[a.Revision] = true
	}
	var errs []error
	// revs is ascending: everything before the newest max is a candidate.
	for _, r := range revs[:max(0, len(revs)-historyMax)] {
		if keep[r.Number] {
			continue
		}
		errs = append(errs, s.Store.DeleteRevision(ctx, name, r.Number),
			os.RemoveAll(deployment.RevisionDir(s.Home, name, r.Number)))
	}
	return errors.Join(errs...)
}
