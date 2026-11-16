package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// ProxyControl writes a proxy's configuration and reads its state. The Envoy implementation is the
// default; tests substitute a fake.
type ProxyControl interface {
	Configure(dir, node string, ports []int, backend string) error
	SetBackend(dir string, ports []int, backend string) error
	Backend(dir string) (string, error)
	WaitReady(ctx context.Context, dir string, ports []int, backend string, timeout time.Duration) error
	WaitDrained(ctx context.Context, dir string, ports []int, timeout time.Duration) (bool, error)
}

// EnvoyProxies controls Envoy proxies through their directories (ADR-009).
type EnvoyProxies struct{}

// Configure implements ProxyControl.
func (EnvoyProxies) Configure(dir, node string, ports []int, backend string) error {
	return envoy.WriteAll(dir, node, ports, backend)
}

// SetBackend implements ProxyControl.
func (EnvoyProxies) SetBackend(dir string, ports []int, backend string) error {
	return envoy.SetBackend(dir, ports, backend)
}

// Backend implements ProxyControl.
func (EnvoyProxies) Backend(dir string) (string, error) { return envoy.Backend(dir) }

// WaitReady implements ProxyControl.
func (EnvoyProxies) WaitReady(ctx context.Context, dir string, ports []int, backend string, timeout time.Duration) error {
	return envoy.NewAdmin(dir).WaitReady(ctx, ports, backend, timeout)
}

// WaitDrained implements ProxyControl.
func (EnvoyProxies) WaitDrained(ctx context.Context, dir string, ports []int, timeout time.Duration) (bool, error) {
	return envoy.NewAdmin(dir).WaitDrained(ctx, ports, timeout)
}

// proxyReadyTimeout bounds how long the proxy may take to route to a new backend.
const proxyReadyTimeout = 30 * time.Second

func containerPorts(w *resource.WorkloadSpec) []int {
	out := make([]int, len(w.Ports))
	for i, p := range w.Ports {
		out[i] = p.ContainerPort
	}
	return out
}

// revisionOf reads the revision label of a running workload, so the proxy is pointed at the alias
// the container actually carries.
func revisionOf(ctx context.Context, rt ports.Runtime, name string) (int, error) {
	st, err := rt.GetWorkload(ctx, name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(st.Labels[ports.LabelRevision])
}

// proxyRequest is the Envoy container in front of a workload. It runs as the karga user so it can
// read its 0700 directory and karga can reach its admin socket.
func proxyRequest(rel string, revision int, c Change, dir, user string) ports.WorkloadRequest {
	spec := c.Proxy
	wr := ports.WorkloadRequest{
		Name:          c.RuntimeName,
		Labels:        labels(rel, revision, KindProxy, c.Name),
		Image:         spec.Image,
		Command:       envoy.Command(),
		User:          user,
		Sysctls:       map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
		Aliases:       []string{c.Name},
		RestartPolicy: "always",
		Mounts:        []ports.Mount{{Type: ports.MountBind, Source: dir, Target: envoy.MountPath}},
	}
	for _, p := range spec.Ports {
		wr.Ports = append(wr.Ports, ports.PortBinding{ContainerPort: p.ContainerPort, HostPort: p.HostPort, HostIP: p.HostIP, Protocol: p.Protocol})
	}
	for _, n := range spec.Networks {
		wr.Networks = append(wr.Networks, RuntimeName(rel, n))
	}
	return wr
}

// createProxy writes the proxy configuration for the workload's current container and starts Envoy.
func createProxy(ctx context.Context, rt ports.Runtime, px ProxyControl, req ExecRequest, c Change) error {
	workload := RuntimeName(req.Release, c.Name)
	rev, err := revisionOf(ctx, rt, workload)
	if err != nil {
		return fmt.Errorf("find the backend for the proxy: %w", err)
	}
	backend := BackendAlias(c.Name, rev)
	dir := ProxyDir(req.Home, req.Release, c.Name)
	portList := c.Proxy.containerPorts()
	if err := px.Configure(dir, c.Name, portList, backend); err != nil {
		return err
	}
	if err := rt.CreateWorkload(ctx, proxyRequest(req.Release, req.Revision, c, dir, req.ProxyUser)); err != nil {
		return err
	}
	if err := rt.StartWorkload(ctx, c.RuntimeName); err != nil {
		return err
	}
	return px.WaitReady(ctx, dir, portList, backend, proxyReadyTimeout)
}

func (p ProxySpec) containerPorts() []int {
	out := make([]int, len(p.Ports))
	for i, port := range p.Ports {
		out[i] = port.ContainerPort
	}
	return out
}

// removeProxy deletes a proxy container; with dropConfig its directory goes too.
func removeProxy(ctx context.Context, rt ports.Runtime, home, rel string, c Change, dropConfig bool) error {
	if err := removeWorkload(ctx, rt, c.Applied.RuntimeName); err != nil {
		return err
	}
	if dropConfig {
		return os.RemoveAll(ProxyDir(home, rel, c.Name))
	}
	return nil
}

// proxyReplace updates a workload behind its proxy without losing connections (ADR-009):
//  1. start <name>-next carrying the new revision's backend alias (nobody resolves it yet);
//  2. wait until it is healthy; on failure stop it (kept) and return untouched;
//  3. point the proxy at the new alias, and wait until the proxy routes to it healthy. Envoy does
//     not keep the old cluster for an alias it cannot resolve, so this only happens after step 2;
//     if the proxy does not get ready the old alias is written back;
//  4. wait until no connection is left on the old backend (at most drainTimeout);
//  5. stop and remove the old container, rename <name>-next to the canonical name.
func proxyReplace(ctx context.Context, rt ports.Runtime, px ProxyControl, req ExecRequest, c Change,
	owned map[string]bool, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string) (touched bool, err error) {
	name := RuntimeName(req.Release, c.Name)
	next := name + nextSuffix
	if owned[ports.ObjectWorkload+"/"+next] {
		if err := removeWorkload(ctx, rt, next); err != nil {
			return false, err
		}
	}
	wr, err := workloadRequest(req.Release, c.Name, s, lbl, paths)
	if err != nil {
		return false, err
	}
	wr.Name = next
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		return false, err
	}
	if err := rt.StartWorkload(ctx, next); err != nil {
		return false, err
	}
	stopNext := func(cause error) error {
		if serr := rt.StopWorkload(ctx, next, stopTimeout); serr != nil && !errors.Is(serr, ports.ErrNotFound) {
			return errors.Join(cause, serr)
		}
		return cause
	}
	if err := waitReady(ctx, rt, []string{next}, req.Timeout); err != nil {
		return false, stopNext(err)
	}

	dir := ProxyDir(req.Home, req.Release, c.Name)
	portList := containerPorts(s)
	previous, err := px.Backend(dir)
	if err != nil {
		return false, stopNext(err)
	}
	backend := BackendAlias(c.Name, req.Revision)
	if err := px.SetBackend(dir, portList, backend); err != nil {
		return false, stopNext(err)
	}
	if err := px.WaitReady(ctx, dir, portList, backend, proxyReadyTimeout); err != nil {
		if previous != "" {
			err = errors.Join(err, px.SetBackend(dir, portList, previous))
		}
		// The proxy may have routed to the new backend for a moment: count it as touched.
		return true, stopNext(err)
	}

	_, drain := s.Strategy()
	if _, err := px.WaitDrained(ctx, dir, portList, drain); err != nil {
		return true, err
	}
	if err := rt.StopWorkload(ctx, c.Applied.RuntimeName, stopTimeout); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return true, err
	}
	if err := rt.DeleteWorkload(ctx, c.Applied.RuntimeName); err != nil {
		return true, err
	}
	return true, rt.RenameWorkload(ctx, next, name)
}

// proxiesDir is the parent of every proxy directory of a release.
func proxiesDir(home, rel string) string { return filepath.Join(home, "proxies", rel) }
