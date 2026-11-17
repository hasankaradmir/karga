package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hasankaradmir/karga/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// revisionOf reads the revision label of a running workload, so the proxy is pointed at the alias
// the container actually carries.
func revisionOf(ctx context.Context, rt ports.Runtime, name string) (int, error) {
	st, err := rt.GetWorkload(ctx, name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(st.Labels[ports.LabelRevision])
}

// proxyWant is what the release proxy must route to for workload name to be served via alias.
func proxyWant(name string, w *resource.WorkloadSpec, alias string) map[string]string {
	want := map[string]string{}
	for _, p := range w.Ports {
		want[proxyCluster(name, p.ContainerPort)] = alias
	}
	return want
}

func proxyClusters(name string, w *resource.WorkloadSpec) []string {
	var out []string
	for _, p := range w.Ports {
		out = append(out, proxyCluster(name, p.ContainerPort))
	}
	return out
}

// proxyConfig routes each proxied workload's ports to the backend alias of the container running it
// (override names another alias per workload, for a replace). A workload without a container is
// left out until it has one.
func proxyConfig(ctx context.Context, rt ports.Runtime, rel string, set map[string]*resource.WorkloadSpec, override map[string]string) envoy.Config {
	var cfg envoy.Config
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		alias, ok := override[name]
		if !ok {
			rev, err := revisionOf(ctx, rt, RuntimeName(rel, name))
			if err != nil {
				continue
			}
			alias = BackendAlias(name, rev)
		}
		for _, p := range set[name].Ports {
			cluster := proxyCluster(name, p.ContainerPort)
			cfg.TCP = append(cfg.TCP, envoy.TCPListener{Address: "0.0.0.0", Port: p.ContainerPort, Cluster: cluster})
			cfg.Clusters = append(cfg.Clusters, envoy.Cluster{Name: cluster, Host: alias, Port: p.ContainerPort, DNS: true,
				HealthPath: httpHealthPath(set[name], p.ContainerPort)})
		}
	}
	return cfg
}

func writeProxy(ctx context.Context, rt ports.Runtime, px ProxyControl, req ExecRequest, set map[string]*resource.WorkloadSpec, override map[string]string) error {
	return px.Write(ProxyDir(req.Home, req.Release), req.Release, 0o600, proxyConfig(ctx, rt, req.Release, set, override))
}

// proxyRequest is the release's Envoy container. It runs as the karga user so it can read its 0700
// directory and karga can reach its admin socket. It joins its first network at creation; the
// others are connected with their own aliases (connectProxy).
func proxyRequest(rel string, revision int, spec ProxySpec, dir, user string) ports.WorkloadRequest {
	wr := ports.WorkloadRequest{
		Name:          ProxyName(rel),
		Labels:        labels(rel, revision, KindProxy, ProxyResource),
		Image:         spec.Image,
		Command:       envoy.Command(),
		User:          user,
		Sysctls:       map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
		RestartPolicy: "always",
		Mounts:        []ports.Mount{{Type: ports.MountBind, Source: dir, Target: envoy.MountPath}},
		MemoryBytes:   envoyMemory,
		MilliCPU:      envoyMilliCPU,
		PidsLimit:     envoyPids,
		LogMaxSize:    resource.DefaultLogMaxSize,
		LogMaxFiles:   resource.DefaultLogMaxFiles,
	}
	if nets := sortedNetworks(spec); len(nets) > 0 {
		wr.Networks, wr.Aliases = []string{RuntimeName(rel, nets[0])}, spec.Networks[nets[0]]
	}
	return wr
}

func sortedNetworks(spec ProxySpec) []string {
	var out []string
	for n := range spec.Networks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// createProxy writes the configuration for what runs now and starts the release proxy.
func createProxy(ctx context.Context, rt ports.Runtime, px ProxyControl, req ExecRequest, c Change, set map[string]*resource.WorkloadSpec) error {
	if err := writeProxy(ctx, rt, px, req, set, nil); err != nil {
		return err
	}
	wr := proxyRequest(req.Release, req.Revision, *c.Proxy, ProxyDir(req.Home, req.Release), req.ProxyUser)
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		return err
	}
	for _, n := range sortedNetworks(*c.Proxy)[1:] {
		if err := rt.ConnectNetwork(ctx, wr.Name, RuntimeName(req.Release, n), c.Proxy.Networks[n]); err != nil {
			return err
		}
	}
	return rt.StartWorkload(ctx, wr.Name)
}

// reconnectProxy gives the running proxy its new aliases: Docker only sets aliases when a container
// joins a network, so each changed network is left and joined again.
func reconnectProxy(ctx context.Context, rt ports.Runtime, rel string, c Change) error {
	var old ProxySpec
	_ = json.Unmarshal(c.Applied.Spec, &old)
	for _, n := range changedNetworks(old, *c.Proxy) {
		if err := rt.DisconnectNetwork(ctx, c.RuntimeName, RuntimeName(rel, n)); err != nil {
			return err
		}
		if aliases := c.Proxy.Networks[n]; len(aliases) > 0 {
			if err := rt.ConnectNetwork(ctx, c.RuntimeName, RuntimeName(rel, n), aliases); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeProxy deletes a proxy container; with dropConfig its directory goes too. A per-workload
// proxy of an earlier karga version had its own directory below the release's.
func removeProxy(ctx context.Context, rt ports.Runtime, home, rel string, c Change, dropConfig bool) error {
	if err := removeWorkload(ctx, rt, c.Applied.RuntimeName); err != nil {
		return err
	}
	if !dropConfig {
		return nil
	}
	if c.Name != ProxyResource {
		return os.RemoveAll(filepath.Join(ProxyDir(home, rel), c.Name))
	}
	for _, f := range []string{"envoy.json", "lds.json", "cds.json", "admin.sock"} {
		if err := os.Remove(filepath.Join(ProxyDir(home, rel), f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// routedTo reports whether the proxy or the gateway routes workload name to container next: then an
// interrupted replace switched over and next is the one to keep.
func routedTo(ctx context.Context, rt ports.Runtime, home, rel, name, next string) bool {
	st, err := rt.GetWorkload(ctx, next)
	if err != nil {
		return false
	}
	if rev, err := strconv.Atoi(st.Labels[ports.LabelRevision]); err == nil {
		eps, _ := envoy.Endpoints(ProxyDir(home, rel))
		for cluster, ep := range eps {
			if strings.HasPrefix(cluster, name+"-p") && strings.HasPrefix(ep, BackendAlias(name, rev)+":") {
				return true
			}
		}
	}
	eps, _ := envoy.Endpoints(gateway.Dir(home))
	for _, p := range gateway.ParseLabel(st.Labels) {
		if eps[gateway.Cluster(rel, name, p.ContainerPort)] == "127.0.0.1:"+strconv.Itoa(p.Upstream) {
			return true
		}
	}
	return false
}

// envoyErr adds what an Envoy container logged about rejected configuration to err, so a config
// Envoy refused (e.g. a port it cannot bind) is reported instead of only a timeout.
func envoyErr(ctx context.Context, rt ports.Runtime, container string, err error) error {
	if err == nil {
		return nil
	}
	var out strings.Builder
	if lerr := rt.Logs(ctx, container, 50, false, &out); lerr != nil {
		return err
	}
	var why []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "rejected") || strings.Contains(line, "][error]") || strings.Contains(line, "][critical]") {
			why = append(why, strings.TrimSpace(line))
		}
	}
	if len(why) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s logged:\n  %s", err, container, strings.Join(why[max(0, len(why)-3):], "\n  "))
}
