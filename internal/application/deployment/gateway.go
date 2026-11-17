package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"strings"
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
	// CertDirs maps each TLS route's Secret to its materialized directory (tls.crt, tls.key).
	CertDirs map[string]string `json:"certDirs,omitempty"`
}

// gatewayPorts returns the ports of w the gateway serves (none off Linux).
func gatewayPorts(w *resource.WorkloadSpec) []resource.Port {
	if !proxySupported.Load() {
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

// certName is the file name (without extension) of a TLS host's certificate in the gateway
// directory. It includes a digest of the revision directory the certificate comes from: Envoy reads
// certificate files only when the listener changes, so a new certificate needs a new name.
func certName(rel string, port int, host, src string) string {
	sum := sha256.Sum256([]byte(src))
	return fmt.Sprintf("%s_%d_%s_%x", rel, port, host, sum[:4])
}

// gatewayConfig builds the gateway's listeners and clusters. Conflicts are refused by Preflight; if
// one slips through anyway (two releases applied at once) the first target in name order wins.
// TLS hosts also get a redirect from plain HTTP on port 80 of their address, unless that port
// already serves the host or is taken otherwise.
func gatewayConfig(targets []gatewayTarget) envoy.Config {
	var cfg envoy.Config
	type addr struct {
		ip   string
		port int
	}
	tcp := map[addr]bool{}
	httpIdx := map[addr]int{}
	routes := map[string]bool{}
	listener := func(a addr) int {
		i, ok := httpIdx[a]
		if !ok {
			i = len(cfg.HTTP)
			httpIdx[a] = i
			cfg.HTTP = append(cfg.HTTP, envoy.HTTPListener{Address: a.ip, Port: a.port})
		}
		return i
	}
	type tlsHost struct {
		a    addr
		host string
	}
	var tlsHosts []tlsHost
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
				i := listener(a)
				l := &cfg.HTTP[i]
				if (r.TLS != nil) != (len(l.Certs) > 0) && len(l.Routes) > 0 {
					continue // a listener is TLS or plain, never both
				}
				routes[key] = true
				l.Routes = append(l.Routes, envoy.HTTPRoute{Host: r.Host, Path: r.Path, Cluster: cluster})
				if r.TLS != nil {
					if l.Certs == nil {
						l.Certs = map[string]envoy.Cert{}
					}
					if _, ok := l.Certs[r.Host]; !ok {
						n := envoy.MountPath + "/certs/" + certName(t.Release, r.HostPort, r.Host, p.CertDirs[r.TLS.Secret])
						l.Certs[r.Host] = envoy.Cert{Chain: n + ".crt", Key: n + ".key"}
						tlsHosts = append(tlsHosts, tlsHost{a, r.Host})
					}
				}
			}
		}
	}
	for _, th := range tlsHosts {
		plain := addr{th.a.ip, 80}
		if tcp[plain] || slices.ContainsFunc(cfg.TCP, func(l envoy.TCPListener) bool {
			return l.Port == 80 && l.Address != plain.ip && ipOverlap(l.Address, plain.ip)
		}) ||
			slices.ContainsFunc(cfg.HTTP, func(l envoy.HTTPListener) bool {
				return l.Port == 80 && l.Address != plain.ip && ipOverlap(l.Address, plain.ip)
			}) {
			continue // the address cannot get a plain listener
		}
		l := &cfg.HTTP[listener(plain)]
		if len(l.Certs) > 0 || slices.ContainsFunc(l.Routes, func(r envoy.HTTPRoute) bool { return r.Host == th.host }) || slices.Contains(l.Redirects, th.host) {
			continue
		}
		l.Redirects, l.RedirectPort = append(l.Redirects, th.host), th.a.port
	}
	return cfg
}

// syncCerts copies the certificates of the targets' TLS routes into the gateway directory and
// removes the ones no longer used. Files are 0600; the gateway reads them as root.
func syncCerts(dir string, targets []gatewayTarget, prune bool) error {
	certs := filepath.Join(dir, "certs")
	want := map[string]bool{}
	for _, t := range targets {
		for _, p := range t.Ports {
			for _, r := range p.Routes {
				if r.TLS == nil {
					continue
				}
				src, ok := p.CertDirs[r.TLS.Secret]
				if !ok {
					return fmt.Errorf("%s/%s: no certificate for route %s", t.Release, t.Workload, r.Host)
				}
				name := certName(t.Release, r.HostPort, r.Host, src)
				for ext, key := range map[string]string{".crt": resource.TLSCertKey, ".key": resource.TLSKeyKey} {
					data, err := os.ReadFile(filepath.Join(src, key))
					if err != nil {
						return fmt.Errorf("%s/%s: certificate for %s: %w", t.Release, t.Workload, r.Host, err)
					}
					if err := os.MkdirAll(certs, 0o700); err != nil {
						return err
					}
					if err := writeFile(filepath.Join(certs, name+ext), data); err != nil {
						return err
					}
					want[name+ext] = true
				}
			}
		}
	}
	if !prune {
		return nil
	}
	entries, err := os.ReadDir(certs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !want[e.Name()] {
			if err := os.Remove(filepath.Join(certs, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeFile replaces path atomically with mode 0600, leaving an identical file alone.
func writeFile(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	// Certificates go first: a listener referencing a missing file would be rejected.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := syncCerts(dir, targets, false); err != nil {
		return nil, err
	}
	cfg := gatewayConfig(targets)
	settings, err := ReadGatewaySettings(home)
	if err != nil {
		return nil, err
	}
	cfg.AccessLog = settings.AccessLog
	// The socket is created by root in the 0700 directory, which keeps other users out.
	if err := px.Write(dir, "gateway", 0o666, cfg); err != nil {
		return nil, err
	}
	if err := syncCerts(dir, targets, true); err != nil {
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

// GatewaySettings are the user's gateway choices; they outlive the gateway itself.
type GatewaySettings struct {
	AccessLog bool `json:"accessLog"`
}

func gatewaySettingsPath(home string) string { return filepath.Join(home, "gateway.json") }

// ReadGatewaySettings returns the stored settings (defaults when none are stored).
func ReadGatewaySettings(home string) (GatewaySettings, error) {
	var s GatewaySettings
	data, err := os.ReadFile(gatewaySettingsPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", gatewaySettingsPath(home), err)
	}
	return s, nil
}

// WriteGatewaySettings stores settings; the next SyncGateway applies them.
func WriteGatewaySettings(home string, s GatewaySettings) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return writeFile(gatewaySettingsPath(home), data)
}

// GatewayState is the health part of `karga gateway status`.
type GatewayState struct {
	// Container is the gateway container's state, or "missing" when no release needs it.
	Container string `json:"container"`
	Ready     bool   `json:"ready"`
	Version   string `json:"version,omitempty"`
	Image     string `json:"image,omitempty"`
	AccessLog bool   `json:"accessLog"`
	// Error says why the admin API could not be read, if it could not.
	Error string `json:"error,omitempty"`
}

// GatewayHealth reads the gateway container and Envoy's admin API.
func GatewayHealth(ctx context.Context, rt ports.Runtime, home string) (GatewayState, error) {
	settings, err := ReadGatewaySettings(home)
	if err != nil {
		return GatewayState{}, err
	}
	st := GatewayState{AccessLog: settings.AccessLog}
	ws, err := rt.GetWorkload(ctx, GatewayName)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		st.Container = "missing"
		return st, nil
	case err != nil:
		return st, err
	}
	st.Container = ws.State
	if src := ws.Labels[labelGatewaySource]; strings.Contains(src, " ") {
		st.Image = src[strings.LastIndex(src, " ")+1:]
	}
	admin := envoy.NewAdmin(GatewayDir(home))
	if st.Ready, err = admin.Live(ctx); err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Version, _ = admin.Version(ctx)
	return st, nil
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
	tls     bool
	routes  map[string]bool   // host+path
	certs   map[string]string // TLS host -> "release/secret" its certificate comes from
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
		c := &gatewayClaim{release: owner, ip: ip, routes: map[string]bool{}, certs: map[string]string{}}
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
				addRouteClaim(claim(owner, r.HostIP, r.HostPort), owner, r)
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
				if err := gatewayFreeRoute(claims[rt.HostPort], rel, rt, where); err != nil {
					errs = append(errs, err)
				} else if !held[rt.HostPort] && len(claims[rt.HostPort]) == 0 && !bindable(rt.HostIP, rt.HostPort) {
					errs = append(errs, fmt.Errorf("%s: the port is taken by another process on this host: %w", where, ports.ErrPortInUse))
				}
				if rt.TLS != nil && !held[80] && len(claims[80]) == 0 && !bindable(rt.HostIP, 80) {
					errs = append(errs, fmt.Errorf("%s: port 80 for the HTTPS redirect is taken by another process on this host: %w", where, ports.ErrPortInUse))
				}
				addRouteClaim(claim(rel+" ("+r.Metadata.Name+")", rt.HostIP, rt.HostPort), rel, rt)
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

func addRouteClaim(c *gatewayClaim, rel string, r resource.Route) {
	c.routes[r.Host+r.Path], c.tls = true, r.TLS != nil
	if r.TLS != nil {
		c.certs[r.Host] = rel + "/" + r.TLS.Secret
	}
}

// gatewayFreeRoute checks an HTTP route of release rel against the existing claims of its port.
func gatewayFreeRoute(existing []*gatewayClaim, rel string, r resource.Route, where string) error {
	if err := gatewayFree(existing, r.HostIP, false, r.Host+r.Path, where); err != nil {
		return err
	}
	tls := r.TLS != nil
	for _, c := range existing {
		if !ipOverlap(c.ip, r.HostIP) || len(c.routes) == 0 {
			continue
		}
		switch {
		case c.tls != tls:
			return fmt.Errorf("%s: %s serves %s on this port: %w", where, c.release, map[bool]string{true: "HTTPS", false: "plain HTTP"}[c.tls], ports.ErrPortInUse)
		case tls && c.certs[r.Host] != "" && c.certs[r.Host] != rel+"/"+r.TLS.Secret:
			return fmt.Errorf("%s: %s already serves HTTPS for %s with another certificate: %w", where, c.release, r.Host, ports.ErrPortInUse)
		}
	}
	return nil
}

func bindable(ip string, port int) bool { return !portTaken("tcp", ip, port) }

// gatewayLabel publishes the gateway ports of a workload request on fresh loopback upstream ports
// and records them in its labels.
func gatewayLabel(wr *ports.WorkloadRequest, gw []resource.Port, paths map[Key]string) error {
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
		gp := GatewayPort{ContainerPort: p.ContainerPort, Upstream: up, HostIP: gw[j].HostIP, HostPort: gw[j].HostPort, Routes: gw[j].Routes}
		for _, r := range gw[j].Routes {
			if r.TLS == nil {
				continue
			}
			dir, ok := paths[Key{resource.KindSecret, r.TLS.Secret}]
			if !ok {
				return fmt.Errorf("tls secret %q was not materialized", r.TLS.Secret)
			}
			if gp.CertDirs == nil {
				gp.CertDirs = map[string]string{}
			}
			gp.CertDirs[r.TLS.Secret] = dir
		}
		label = append(label, gp)
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
