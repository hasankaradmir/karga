// Package fakeruntime is an in-memory ports.Runtime for unit tests.
package fakeruntime

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hasankaradmir/karga/internal/ports"
)

// Runtime is safe for concurrent use. Set Fail["<Op>:<name>"] to make an operation fail.
type Runtime struct {
	mu        sync.Mutex
	Images    map[string]bool
	Networks  map[string]ports.NetworkRequest
	Volumes   map[string]ports.VolumeRequest
	Workloads map[string]*Workload
	// Fail injects errors, e.g. Fail["StartWorkload:web-redis"] = errors.New("boom").
	Fail map[string]error
	// Calls records every operation as "<Op>:<name>" in order.
	Calls []string
	// Health is the health a started workload with a healthcheck reports (default healthy).
	Health string
	// Capabilities overrides the reported capabilities when non-nil.
	Capabilities []string
}

// Workload is a fake container. Aliases maps each attached network to the workload's DNS aliases.
type Workload struct {
	Request ports.WorkloadRequest
	State   string
	Aliases map[string][]string
}

// New returns an empty runtime. Images listed in pulled are present locally.
func New(pulled ...string) *Runtime {
	r := &Runtime{
		Images:    map[string]bool{},
		Networks:  map[string]ports.NetworkRequest{},
		Volumes:   map[string]ports.VolumeRequest{},
		Workloads: map[string]*Workload{},
		Fail:      map[string]error{},
	}
	for _, i := range pulled {
		r.Images[i] = true
	}
	return r
}

func (r *Runtime) call(op, name string) error {
	key := op + ":" + name
	r.Calls = append(r.Calls, key)
	return r.Fail[key]
}

// Info implements ports.Runtime.
func (r *Runtime) Info(context.Context) (ports.RuntimeInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("Info", ""); err != nil {
		return ports.RuntimeInfo{}, err
	}
	caps := r.Capabilities
	if caps == nil {
		caps = []string{
			ports.CapNetworkDriver("bridge"), ports.CapVolumeDriver("local"),
			ports.CapResourcesMemory, ports.CapResourcesCPU, ports.CapHealthcheckExec,
		}
	}
	return ports.RuntimeInfo{Name: "docker", Version: "fake", Capabilities: caps}, nil
}

// EnsureImage implements ports.Runtime. Images under "missing/" or "karga.invalid/" cannot be pulled.
func (r *Runtime) EnsureImage(_ context.Context, ref, policy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("EnsureImage", ref); err != nil {
		return err
	}
	if policy == "if-not-present" && r.Images[ref] {
		return nil
	}
	if policy == "never" {
		if r.Images[ref] {
			return nil
		}
		return fmt.Errorf("image %s is not present and pullPolicy is never: %w", ref, ports.ErrImageNotFound)
	}
	if strings.HasPrefix(ref, "missing/") || strings.HasPrefix(ref, "karga.invalid/") {
		return fmt.Errorf("pull %s: %w", ref, ports.ErrImageNotFound)
	}
	r.Calls = append(r.Calls, "Pull:"+ref)
	r.Images[ref] = true
	return nil
}

// CreateNetwork implements ports.Runtime.
func (r *Runtime) CreateNetwork(_ context.Context, req ports.NetworkRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("CreateNetwork", req.Name); err != nil {
		return err
	}
	if _, ok := r.Networks[req.Name]; ok {
		return fmt.Errorf("network %s: %w", req.Name, ports.ErrConflict)
	}
	r.Networks[req.Name] = req
	return nil
}

// DeleteNetwork implements ports.Runtime.
func (r *Runtime) DeleteNetwork(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DeleteNetwork", name); err != nil {
		return err
	}
	delete(r.Networks, name)
	return nil
}

// CreateVolume implements ports.Runtime.
func (r *Runtime) CreateVolume(_ context.Context, req ports.VolumeRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("CreateVolume", req.Name); err != nil {
		return err
	}
	if _, ok := r.Volumes[req.Name]; ok {
		return fmt.Errorf("volume %s: %w", req.Name, ports.ErrConflict)
	}
	r.Volumes[req.Name] = req
	return nil
}

// DeleteVolume implements ports.Runtime.
func (r *Runtime) DeleteVolume(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DeleteVolume", name); err != nil {
		return err
	}
	delete(r.Volumes, name)
	return nil
}

// CreateWorkload implements ports.Runtime.
func (r *Runtime) CreateWorkload(_ context.Context, req ports.WorkloadRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("CreateWorkload", req.Name); err != nil {
		return err
	}
	if _, ok := r.Workloads[req.Name]; ok {
		return fmt.Errorf("workload %s: %w", req.Name, ports.ErrConflict)
	}
	if !r.Images[req.Image] {
		return fmt.Errorf("image %s: %w", req.Image, ports.ErrImageNotFound)
	}
	for _, n := range req.Networks {
		if _, ok := r.Networks[n]; !ok {
			return fmt.Errorf("network %s: %w", n, ports.ErrNotFound)
		}
	}
	w := &Workload{Request: req, State: ports.StateCreated, Aliases: map[string][]string{}}
	for _, n := range req.Networks {
		w.Aliases[n] = slices.Clone(req.Aliases)
	}
	r.Workloads[req.Name] = w
	return nil
}

// StartWorkload implements ports.Runtime.
func (r *Runtime) StartWorkload(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("StartWorkload", name); err != nil {
		return err
	}
	w, ok := r.Workloads[name]
	if !ok {
		return fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	for _, p := range w.Request.Ports {
		if p.HostPort == 0 {
			continue
		}
		for otherName, o := range r.Workloads {
			if otherName == name || o.State != ports.StateRunning {
				continue
			}
			for _, op := range o.Request.Ports {
				if op.HostPort == p.HostPort && op.Protocol == p.Protocol {
					return fmt.Errorf("port %d/%s: %w", p.HostPort, p.Protocol, ports.ErrPortInUse)
				}
			}
		}
	}
	w.State = ports.StateRunning
	return nil
}

// StopWorkload implements ports.Runtime.
func (r *Runtime) StopWorkload(_ context.Context, name string, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("StopWorkload", name); err != nil {
		return err
	}
	w, ok := r.Workloads[name]
	if !ok {
		return fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	w.State = ports.StateExited
	return nil
}

// DeleteWorkload implements ports.Runtime.
func (r *Runtime) DeleteWorkload(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DeleteWorkload", name); err != nil {
		return err
	}
	delete(r.Workloads, name)
	return nil
}

// GetWorkload implements ports.Runtime.
func (r *Runtime) GetWorkload(_ context.Context, name string) (ports.WorkloadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.Fail["GetWorkload:"+name]; err != nil {
		return ports.WorkloadStatus{}, err
	}
	w, ok := r.Workloads[name]
	if !ok {
		return ports.WorkloadStatus{}, fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	health := ports.HealthNone
	if w.Request.HealthCheck != nil && w.State == ports.StateRunning {
		health = ports.HealthHealthy
		if r.Health != "" {
			health = r.Health
		}
	}
	return ports.WorkloadStatus{Name: name, State: w.State, Health: health, Labels: maps.Clone(w.Request.Labels)}, nil
}

// ListManaged implements ports.Runtime.
func (r *Runtime) ListManaged(_ context.Context, release string) ([]ports.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.Object
	add := func(kind, name string, labels map[string]string) {
		if labels[ports.LabelManaged] == "true" && labels[ports.LabelRelease] == release {
			out = append(out, ports.Object{Kind: kind, Name: name, Labels: maps.Clone(labels)})
		}
	}
	for n, w := range r.Workloads {
		add(ports.ObjectWorkload, n, w.Request.Labels)
	}
	for n, v := range r.Networks {
		add(ports.ObjectNetwork, n, v.Labels)
	}
	for n, v := range r.Volumes {
		add(ports.ObjectVolume, n, v.Labels)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].Name < out[j].Kind+out[j].Name })
	return out, nil
}

// ListLabeled implements ports.Runtime.
func (r *Runtime) ListLabeled(_ context.Context, key string) ([]ports.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.Object
	for n, w := range r.Workloads {
		if _, ok := w.Request.Labels[key]; ok && w.Request.Labels[ports.LabelManaged] == "true" {
			out = append(out, ports.Object{Kind: ports.ObjectWorkload, Name: n, Labels: maps.Clone(w.Request.Labels)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PublishedPorts implements ports.Runtime.
func (r *Runtime) PublishedPorts(context.Context) ([]ports.PublishedPort, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.PublishedPort
	for n, w := range r.Workloads {
		if w.State != ports.StateRunning {
			continue
		}
		for _, p := range w.Request.Ports {
			if p.HostPort != 0 {
				out = append(out, ports.PublishedPort{
					HostIP: p.HostIP, Port: p.HostPort, Protocol: p.Protocol,
					Release: w.Request.Labels[ports.LabelRelease], Workload: n,
				})
			}
		}
	}
	return out, nil
}

// RenameWorkload implements ports.Runtime.
func (r *Runtime) RenameWorkload(_ context.Context, from, to string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("RenameWorkload", from+"->"+to); err != nil {
		return err
	}
	w, ok := r.Workloads[from]
	if !ok {
		return fmt.Errorf("workload %s: %w", from, ports.ErrNotFound)
	}
	if _, taken := r.Workloads[to]; taken {
		return fmt.Errorf("workload %s: %w", to, ports.ErrConflict)
	}
	delete(r.Workloads, from)
	w.Request.Name = to
	r.Workloads[to] = w
	return nil
}

// ConnectNetwork implements ports.Runtime.
func (r *Runtime) ConnectNetwork(_ context.Context, workload, network string, aliases []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("ConnectNetwork", workload+"@"+network+"="+strings.Join(aliases, ",")); err != nil {
		return err
	}
	w, ok := r.Workloads[workload]
	if !ok {
		return fmt.Errorf("workload %s: %w", workload, ports.ErrNotFound)
	}
	if _, ok := r.Networks[network]; !ok {
		return fmt.Errorf("network %s: %w", network, ports.ErrNotFound)
	}
	if _, attached := w.Aliases[network]; attached {
		return fmt.Errorf("workload %s already on network %s: %w", workload, network, ports.ErrConflict)
	}
	w.Aliases[network] = slices.Clone(aliases)
	return nil
}

// DisconnectNetwork implements ports.Runtime.
func (r *Runtime) DisconnectNetwork(_ context.Context, workload, network string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DisconnectNetwork", workload+"@"+network); err != nil {
		return err
	}
	w, ok := r.Workloads[workload]
	if !ok {
		return fmt.Errorf("workload %s: %w", workload, ports.ErrNotFound)
	}
	delete(w.Aliases, network)
	return nil
}
