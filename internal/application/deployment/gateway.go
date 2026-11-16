package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"syscall"

	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// The gateway is the host's single Envoy (ADR-010): it holds every tcp hostPort and HTTP route of
// every release on Linux and forwards to the workloads' loopback upstream ports. It belongs to no
// release; karga creates it when something needs it and removes it when nothing does.
const (
	GatewayName = "karga-gateway"
	// LabelGateway on a workload container lists its gateway ports as JSON ([]GatewayPort).
	LabelGateway   = "io.karga.gateway"
	LabelComponent = "io.karga.component"
)

// GatewayDir holds the gateway's configuration and admin socket.
func GatewayDir(home string) string { return filepath.Join(home, "gateway") }

// GatewayPort is a workload port served by the gateway: as tcp on HostIP:HostPort, or as HTTP routes.
// Upstream is the loopback port the container publishes it on.
type GatewayPort struct {
	ContainerPort int              `json:"containerPort"`
	Upstream      int              `json:"upstream"`
	HostIP        string           `json:"hostIP,omitempty"`
	HostPort      int              `json:"hostPort,omitempty"`
	Routes        []resource.Route `json:"routes,omitempty"`
}

// gatewayPorts returns the ports of w the gateway serves (none off Linux).
func gatewayPorts(w *resource.WorkloadSpec) []resource.Port {
	if !proxySupported {
		return nil
	}
	var out []resource.Port
	for _, p := range w.Ports {
		if p.Protocol == "tcp" && (p.HostPort != 0 || len(p.Routes) > 0) {
			out = append(out, p)
		}
	}
	return out
}

// allocatePort picks a free loopback port for an upstream. Docker binds it right after; a race
// with another process makes the container start fail, which is reported as usual.
func allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate an upstream port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// gatewayCluster names the cluster of one workload port. Release and workload names are DNS
// labels, so "_" cannot occur in them.
func gatewayCluster(rel, workload string, port int) string {
	return fmt.Sprintf("%s_%s_p%d", rel, workload, port)
}

// gatewayTarget is one workload container the gateway forwards to.
type gatewayTarget struct {
	Release, Workload, Container string
	Ports                        []GatewayPort
}

func parseGatewayLabel(labels map[string]string) []GatewayPort {
	var out []GatewayPort
	_ = json.Unmarshal([]byte(labels[LabelGateway]), &out) // karga writes it; a broken label means no ports
	return out
}

// gatewayTargets finds, per release workload, the container the gateway routes to: the one named in
// prefer ("release/workload" -> container) or else the canonical one. Others (a replace's -next,
// a stopped leftover) are ignored.
func gatewayTargets(ctx context.Context, rt ports.Runtime, prefer map[string]string) ([]gatewayTarget, error) {
	objs, err := rt.ListLabeled(ctx, LabelGateway)
	if err != nil {
		return nil, err
	}
	var out []gatewayTarget
	for _, o := range objs {
		rel, name := o.Labels[ports.LabelRelease], o.Labels[ports.LabelName]
		want, ok := prefer[rel+"/"+name]
		if !ok {
			want = RuntimeName(rel, name)
		}
		if o.Name != want {
			continue
		}
		out = append(out, gatewayTarget{Release: rel, Workload: name, Container: o.Name, Ports: parseGatewayLabel(o.Labels)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out, nil
}

// gatewayConfig builds the gateway's listeners and clusters. Conflicts are refused by Preflight; if
// one slips through anyway (two releases applied at once) the first target in name order wins.
func gatewayConfig(targets []gatewayTarget) envoy.Config {
	var cfg envoy.Config
	type addr struct {
		ip   string
		port int
	}
	tcp := map[addr]bool{}
	httpIdx := map[addr]int{}
	routes := map[string]bool{}
	for _, t := range targets {
		for _, p := range t.Ports {
			cluster := gatewayCluster(t.Release, t.Workload, p.ContainerPort)
			cfg.Clusters = append(cfg.Clusters, envoy.Cluster{Name: cluster, Host: "127.0.0.1", Port: p.Upstream})
			if p.HostPort != 0 {
				a := addr{p.HostIP, p.HostPort}
				if _, isHTTP := httpIdx[a]; !tcp[a] && !isHTTP {
					tcp[a] = true
					cfg.TCP = append(cfg.TCP, envoy.TCPListener{Address: p.HostIP, Port: p.HostPort, Cluster: cluster})
				}
			}
			for _, r := range p.Routes {
				a := addr{r.HostIP, r.HostPort}
				key := fmt.Sprintf("%s:%d %s%s", r.HostIP, r.HostPort, r.Host, r.Path)
				if tcp[a] || routes[key] {
					continue
				}
				routes[key] = true
				i, ok := httpIdx[a]
				if !ok {
					i = len(cfg.HTTP)
					httpIdx[a] = i
					cfg.HTTP = append(cfg.HTTP, envoy.HTTPListener{Address: r.HostIP, Port: r.HostPort})
				}
				cfg.HTTP[i].Routes = append(cfg.HTTP[i].Routes, envoy.HTTPRoute{Host: r.Host, Path: r.Path, Cluster: cluster})
			}
		}
	}
	return cfg
}

// gatewayWant is what the gateway must route to for the given targets to be served: cluster -> endpoint.
func gatewayWant(targets []gatewayTarget, keep func(gatewayTarget) bool) map[string]string {
	want := map[string]string{}
	for _, t := range targets {
		if !keep(t) {
			continue
		}
		for _, p := range t.Ports {
			want[gatewayCluster(t.Release, t.Workload, p.ContainerPort)] = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Upstream))
		}
	}
	return want
}

// lockGateway serializes gateway updates of concurrent karga processes (release locks are per release).
func lockGateway(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock the gateway: %w", err)
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}

// labelGatewaySource records what a gateway container was created from; another KARGA_HOME or image
// means it must be recreated.
const labelGatewaySource = "io.karga.gateway-source"

func gatewaySource(dir string) string { return dir + " " + envoy.Image() }

func gatewayRequest(dir string) ports.WorkloadRequest {
	return ports.WorkloadRequest{
		Name:   GatewayName,
		Labels: map[string]string{ports.LabelManaged: "true", LabelComponent: "gateway", labelGatewaySource: gatewaySource(dir)},
		Image:  envoy.Image(),
		// Root only to bind low ports in the host namespace (Docker grants no ambient capabilities
		// to other users); every capability but those two is dropped.
		Command:         envoy.Command(),
		User:            "0:0",
		Env:             map[string]string{"ENVOY_UID": "0"},
		HostNetwork:     true,
		CapDrop:         []string{"ALL"},
		CapAdd:          []string{"NET_BIND_SERVICE", "DAC_OVERRIDE"},
		NoNewPrivileges: true,
		RestartPolicy:   "always",
		Mounts:          []ports.Mount{{Type: ports.MountBind, Source: dir, Target: envoy.MountPath}},
	}
}

// SyncGateway rewrites the gateway from every release's workload containers and creates or removes
// the gateway container as needed. prefer routes specific workloads to another container (a
// replace's -next). It returns the targets it routes to.
func SyncGateway(ctx context.Context, rt ports.Runtime, px ProxyControl, home string, prefer map[string]string) ([]gatewayTarget, error) {
	unlock, err := lockGateway(home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	targets, err := gatewayTargets(ctx, rt, prefer)
	if err != nil {
		return nil, err
	}
	dir := GatewayDir(home)
	if len(targets) == 0 {
		if err := removeWorkload(ctx, rt, GatewayName); err != nil && !errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}
		return nil, os.RemoveAll(dir)
	}
	// The socket is created by root in the 0700 directory, which keeps other users out.
	if err := px.Write(dir, "gateway", 0o666, gatewayConfig(targets)); err != nil {
		return nil, err
	}
	st, err := rt.GetWorkload(ctx, GatewayName)
	if err == nil && st.Labels[labelGatewaySource] != gatewaySource(dir) {
		if err := removeWorkload(ctx, rt, GatewayName); err != nil {
			return nil, err
		}
		err = fmt.Errorf("gateway %w", ports.ErrNotFound)
	}
	switch {
	case errors.Is(err, ports.ErrNotFound):
		if err := rt.EnsureImage(ctx, envoy.Image(), "if-not-present"); err != nil {
			return nil, err
		}
		if err := rt.CreateWorkload(ctx, gatewayRequest(dir)); err != nil {
			return nil, err
		}
		err = rt.StartWorkload(ctx, GatewayName)
	case err == nil && st.State != ports.StateRunning:
		err = rt.StartWorkload(ctx, GatewayName)
	}
	return targets, err
}

// waitGateway waits until the gateway routes to every target of release rel.
func waitGateway(ctx context.Context, px ProxyControl, home, rel string, targets []gatewayTarget) error {
	want := gatewayWant(targets, func(t gatewayTarget) bool { return t.Release == rel })
	if len(want) == 0 {
		return nil
	}
	return px.WaitReady(ctx, GatewayDir(home), want, proxyReadyTimeout)
}

// GatewayRoute is one line of `karga gateway status`.
type GatewayRoute struct {
	Listener string `json:"listener"` // ip:port
	Route    string `json:"route"`    // "tcp" or host+path
	Release  string `json:"release"`
	Workload string `json:"workload"`
	Upstream string `json:"upstream"`
}

// GatewayRoutes lists what the gateway serves, as computed from the running workloads.
func GatewayRoutes(ctx context.Context, rt ports.Runtime) ([]GatewayRoute, error) {
	targets, err := gatewayTargets(ctx, rt, nil)
	if err != nil {
		return nil, err
	}
	var out []GatewayRoute
	for _, t := range targets {
		for _, p := range t.Ports {
			up := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Upstream))
			if p.HostPort != 0 {
				out = append(out, GatewayRoute{net.JoinHostPort(p.HostIP, strconv.Itoa(p.HostPort)), "tcp", t.Release, t.Workload, up})
			}
			for _, r := range p.Routes {
				out = append(out, GatewayRoute{net.JoinHostPort(r.HostIP, strconv.Itoa(r.HostPort)), r.Host + r.Path, t.Release, t.Workload, up})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Listener != out[j].Listener {
			return out[i].Listener < out[j].Listener
		}
		return out[i].Route < out[j].Route
	})
	return out, nil
}

// gatewayClaim is a host address the gateway serves for a release.
type gatewayClaim struct {
	release string
	ip      string
	tcp     bool
	routes  map[string]bool // host+path
}

// checkGateway refuses gateway ports that clash with other releases (or within the release), and
// binds unclaimed ones once to see that no other process holds them.
func checkGateway(ctx context.Context, rt ports.Runtime, rel string, rs []resource.Resource) []error {
	objs, err := rt.ListLabeled(ctx, LabelGateway)
	if err != nil {
		return []error{err}
	}
	claims := map[int][]*gatewayClaim{}
	claim := func(owner, ip string, port int) *gatewayClaim {
		for _, c := range claims[port] {
			if c.release == owner && c.ip == ip {
				return c
			}
		}
		c := &gatewayClaim{release: owner, ip: ip, routes: map[string]bool{}}
		claims[port] = append(claims[port], c)
		return c
	}
	for _, o := range objs {
		owner := o.Labels[ports.LabelRelease]
		if owner == rel {
			continue
		}
		for _, p := range parseGatewayLabel(o.Labels) {
			if p.HostPort != 0 {
				claim(owner, p.HostIP, p.HostPort).tcp = true
			}
			for _, r := range p.Routes {
				claim(owner, r.HostIP, r.HostPort).routes[r.Host+r.Path] = true
			}
		}
	}
	held := map[int]bool{} // ports the gateway already listens on for this release
	for _, o := range objs {
		if o.Labels[ports.LabelRelease] != rel {
			continue
		}
		for _, p := range parseGatewayLabel(o.Labels) {
			held[p.HostPort] = true
			for _, r := range p.Routes {
				held[r.HostPort] = true
			}
		}
	}

	var errs []error
	for _, r := range rs {
		w, ok := r.Spec.(*resource.WorkloadSpec)
		if !ok {
			continue
		}
		for _, p := range gatewayPorts(w) {
			if p.HostPort != 0 {
				where := fmt.Sprintf("%s %q: hostPort %s:%d", resource.KindWorkload, r.Metadata.Name, p.HostIP, p.HostPort)
				if err := gatewayFree(claims[p.HostPort], p.HostIP, true, "", where); err != nil {
					errs = append(errs, err)
				} else if !held[p.HostPort] && !bindable(p.HostIP, p.HostPort) {
					errs = append(errs, fmt.Errorf("%s is taken by another process on this host: %w", where, ports.ErrPortInUse))
				}
				claim(rel+" ("+r.Metadata.Name+")", p.HostIP, p.HostPort).tcp = true
			}
			for _, rt := range p.Routes {
				where := fmt.Sprintf("%s %q: route %s%s on %s:%d", resource.KindWorkload, r.Metadata.Name, rt.Host, rt.Path, rt.HostIP, rt.HostPort)
				if err := gatewayFree(claims[rt.HostPort], rt.HostIP, false, rt.Host+rt.Path, where); err != nil {
					errs = append(errs, err)
				} else if !held[rt.HostPort] && len(claims[rt.HostPort]) == 0 && !bindable(rt.HostIP, rt.HostPort) {
					errs = append(errs, fmt.Errorf("%s: the port is taken by another process on this host: %w", where, ports.ErrPortInUse))
				}
				claim(rel+" ("+r.Metadata.Name+")", rt.HostIP, rt.HostPort).routes[rt.Host+rt.Path] = true
			}
		}
	}
	return errs
}

// gatewayFree checks a tcp port (route == "") or an HTTP route against existing claims of a port.
func gatewayFree(existing []*gatewayClaim, ip string, tcp bool, route, where string) error {
	for _, c := range existing {
		if !ipOverlap(c.ip, ip) {
			continue
		}
		switch {
		case tcp || c.tcp:
			return fmt.Errorf("%s is used by %s: %w", where, c.release, ports.ErrPortInUse)
		case c.ip != ip:
			return fmt.Errorf("%s: %s serves HTTP on this port with hostIP %s: %w", where, c.release, c.ip, ports.ErrPortInUse)
		case c.routes[route]:
			return fmt.Errorf("%s is already routed to %s: %w", where, c.release, ports.ErrPortInUse)
		}
	}
	return nil
}

func bindable(ip string, port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// gatewayLabel publishes the gateway ports of a workload request on fresh loopback upstream ports
// and records them in its labels.
func gatewayLabel(wr *ports.WorkloadRequest, gw []resource.Port) error {
	if len(gw) == 0 {
		return nil
	}
	var label []GatewayPort
	for i := range wr.Ports {
		p := &wr.Ports[i]
		j := slices.IndexFunc(gw, func(g resource.Port) bool { return g.ContainerPort == p.ContainerPort && g.Protocol == p.Protocol })
		if j < 0 {
			continue
		}
		up, err := allocatePort()
		if err != nil {
			return err
		}
		label = append(label, GatewayPort{ContainerPort: p.ContainerPort, Upstream: up, HostIP: gw[j].HostIP, HostPort: gw[j].HostPort, Routes: gw[j].Routes})
		p.HostIP, p.HostPort = "127.0.0.1", up
	}
	js, err := json.Marshal(label)
	if err != nil {
		return err
	}
	wr.Labels = maps.Clone(wr.Labels)
	wr.Labels[LabelGateway] = string(js)
	return nil
}
