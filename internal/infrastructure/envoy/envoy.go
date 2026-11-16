// Package envoy writes the file-based configuration of karga's Envoy proxies and reads their admin
// API (ADR-009, ADR-010). Switching a cluster's endpoint in cds.json moves new connections to
// another container while existing ones finish on the old one.
package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MountPath is where the proxy directory is mounted inside the Envoy container.
const MountPath = "/etc/karga-proxy"

// DefaultImage is the Envoy image validated in the spikes (1.36, distroless: 149MB instead of 241MB).
// KARGA_PROXY_IMAGE overrides it.
const DefaultImage = "envoyproxy/envoy:distroless-v1.36-latest"

// Image returns the proxy image to use.
func Image() string {
	if v := os.Getenv("KARGA_PROXY_IMAGE"); v != "" {
		return v
	}
	return DefaultImage
}

// Command runs Envoy on the mounted bootstrap file with one worker thread (ADR-010: low resources).
// It starts with a flag, so both the distroless entrypoint (envoy) and the debian one accept it.
func Command() []string {
	return []string{"-c", MountPath + "/envoy.json", "--concurrency", "1", "--log-level", "warn"}
}

const (
	bootstrapFile = "envoy.json"
	listenersFile = "lds.json"
	clustersFile  = "cds.json"
	adminSocket   = "admin.sock"
)

// Cluster is an upstream with one endpoint. With DNS the host is a name resolved continuously
// (container restarts and IP changes do not matter); otherwise it is an IP address.
type Cluster struct {
	Name string
	Host string
	Port int
	DNS  bool
}

// TCPListener proxies every connection on Address:Port to Cluster.
type TCPListener struct {
	Address string
	Port    int
	Cluster string
}

// HTTPListener serves HTTP on Address:Port and routes requests by Host header and path prefix.
type HTTPListener struct {
	Address string
	Port    int
	Routes  []HTTPRoute
}

// HTTPRoute sends requests for Host whose path starts with Path to Cluster.
type HTTPRoute struct {
	Host    string
	Path    string
	Cluster string
}

// Config is the dynamic part of a proxy: listeners (lds.json) and clusters (cds.json).
type Config struct {
	TCP      []TCPListener
	HTTP     []HTTPListener
	Clusters []Cluster
}

// resources wraps typed Envoy resources the way path_config_source expects them.
func resources(items []map[string]any) ([]byte, error) {
	if items == nil {
		items = []map[string]any{}
	}
	return json.MarshalIndent(map[string]any{"resources": items}, "", "  ")
}

func socketAddress(host string, port int) map[string]any {
	return map[string]any{"socket_address": map[string]any{"address": host, "port_value": port}}
}

// Bootstrap is the static part: an admin API on a unix socket and file-based LDS/CDS. socketMode is
// the admin socket's permission; the proxy directory (0700) is what keeps other users out.
func Bootstrap(node string, socketMode int) ([]byte, error) {
	source := func(file string) map[string]any {
		return map[string]any{
			"resource_api_version": "V3",
			"path_config_source": map[string]any{
				"path":              MountPath + "/" + file,
				"watched_directory": map[string]any{"path": MountPath},
			},
		}
	}
	return json.MarshalIndent(map[string]any{
		"node":  map[string]any{"id": node, "cluster": "karga"},
		"admin": map[string]any{"address": map[string]any{"pipe": map[string]any{"path": MountPath + "/" + adminSocket, "mode": socketMode}}},
		"dynamic_resources": map[string]any{
			"lds_config": source(listenersFile),
			"cds_config": source(clustersFile),
		},
	}, "", "  ")
}

func listener(name, address string, port int, filter map[string]any) map[string]any {
	return map[string]any{
		"@type":         "type.googleapis.com/envoy.config.listener.v3.Listener",
		"name":          name,
		"address":       socketAddress(address, port),
		"filter_chains": []any{map[string]any{"filters": []any{filter}}},
	}
}

func tcpName(address string, port int) string  { return fmt.Sprintf("tcp_%s_%d", address, port) }
func httpName(address string, port int) string { return fmt.Sprintf("http_%s_%d", address, port) }

// Listeners renders lds.json. Output is sorted so equal configs give equal files.
func (c Config) Listeners() ([]byte, error) {
	var items []map[string]any
	tcp := slices.Clone(c.TCP)
	slices.SortFunc(tcp, func(a, b TCPListener) int {
		return strings.Compare(tcpName(a.Address, a.Port), tcpName(b.Address, b.Port))
	})
	for _, l := range tcp {
		name := tcpName(l.Address, l.Port)
		items = append(items, listener(name, l.Address, l.Port, map[string]any{
			"name": "envoy.filters.network.tcp_proxy",
			"typed_config": map[string]any{
				"@type":                "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
				"stat_prefix":          name,
				"cluster":              l.Cluster,
				"max_connect_attempts": 3,
			},
		}))
	}
	httpls := slices.Clone(c.HTTP)
	slices.SortFunc(httpls, func(a, b HTTPListener) int {
		return strings.Compare(httpName(a.Address, a.Port), httpName(b.Address, b.Port))
	})
	for _, l := range httpls {
		name := httpName(l.Address, l.Port)
		items = append(items, listener(name, l.Address, l.Port, map[string]any{
			"name": "envoy.filters.network.http_connection_manager",
			"typed_config": map[string]any{
				"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
				"stat_prefix": name,
				// The gateway sees the real client address and passes it on in x-forwarded-for.
				"use_remote_address": true,
				"http_filters": []any{map[string]any{
					"name":         "envoy.filters.http.router",
					"typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"},
				}},
				"route_config": map[string]any{"name": name, "virtual_hosts": virtualHosts(l.Routes)},
			},
		}))
	}
	return resources(items)
}

// virtualHosts groups routes by host; within a host longer path prefixes match first.
func virtualHosts(routes []HTTPRoute) []any {
	byHost := map[string][]HTTPRoute{}
	for _, r := range routes {
		byHost[r.Host] = append(byHost[r.Host], r)
	}
	hosts := slices.Sorted(maps.Keys(byHost))
	out := []any{}
	for _, h := range hosts {
		rs := byHost[h]
		slices.SortFunc(rs, func(a, b HTTPRoute) int {
			if len(a.Path) != len(b.Path) {
				return len(b.Path) - len(a.Path)
			}
			return strings.Compare(a.Path, b.Path)
		})
		var matches []any
		for _, r := range rs {
			matches = append(matches, map[string]any{
				"match": map[string]any{"prefix": r.Path},
				// Only connect failures are retried: the request has not reached a backend yet.
				"route": map[string]any{"cluster": r.Cluster, "retry_policy": map[string]any{"retry_on": "connect-failure", "num_retries": 2}},
			})
		}
		out = append(out, map[string]any{"name": h, "domains": []string{h, h + ":*"}, "routes": matches})
	}
	return out
}

// ClustersJSON renders cds.json.
func (c Config) ClustersJSON() ([]byte, error) {
	var items []map[string]any
	cs := slices.Clone(c.Clusters)
	slices.SortFunc(cs, func(a, b Cluster) int { return strings.Compare(a.Name, b.Name) })
	for _, cl := range cs {
		item := map[string]any{
			"@type":           "type.googleapis.com/envoy.config.cluster.v3.Cluster",
			"name":            cl.Name,
			"connect_timeout": "1s",
			"load_assignment": map[string]any{
				"cluster_name": cl.Name,
				"endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{
					"endpoint": map[string]any{"address": socketAddress(cl.Host, cl.Port)},
				}}}},
			},
			// A new cluster only takes traffic once its endpoint passed this check (warming).
			"health_checks": []any{map[string]any{
				"timeout": "1s", "interval": "0.5s", "unhealthy_threshold": 1, "healthy_threshold": 1,
				"tcp_health_check": map[string]any{},
			}},
		}
		if cl.DNS {
			// The DnsCluster extension replaces the deprecated type/dns_refresh_rate fields.
			item["cluster_type"] = map[string]any{
				"name": "envoy.cluster.dns",
				"typed_config": map[string]any{
					"@type":                            "type.googleapis.com/envoy.extensions.clusters.dns.v3.DnsCluster",
					"dns_refresh_rate":                 "1s",
					"dns_lookup_family":                "V4_ONLY",
					"all_addresses_in_single_endpoint": false,
				},
			}
		} else {
			item["type"] = "STATIC"
		}
		items = append(items, item)
	}
	return resources(items)
}

// writeAtomic replaces dir/name with a rename, which is the change Envoy's directory watch picks
// up. An unchanged file is left alone so Envoy does not reload for nothing.
func writeAtomic(dir, name string, data []byte) error {
	if old, err := os.ReadFile(filepath.Join(dir, name)); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// Write creates the proxy directory (0700) and writes bootstrap, clusters and listeners. Clusters
// go first so a new listener never points at a cluster Envoy does not know yet.
func Write(dir, node string, socketMode int, cfg Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	boot, err := Bootstrap(node, socketMode)
	if err != nil {
		return err
	}
	cds, err := cfg.ClustersJSON()
	if err != nil {
		return err
	}
	lds, err := cfg.Listeners()
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{bootstrapFile, boot}, {clustersFile, cds}, {listenersFile, lds}} {
		if err := writeAtomic(dir, f.name, f.data); err != nil {
			return err
		}
	}
	return nil
}

// Endpoints returns each cluster's endpoint as written in dir ("host:port"), empty without config.
func Endpoints(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, clustersFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Resources []struct {
			Name           string `json:"name"`
			LoadAssignment struct {
				Endpoints []struct {
					LbEndpoints []struct {
						Endpoint struct {
							Address address `json:"address"`
						} `json:"endpoint"`
					} `json:"lb_endpoints"`
				} `json:"endpoints"`
			} `json:"load_assignment"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, clustersFile), err)
	}
	out := map[string]string{}
	for _, r := range cfg.Resources {
		for _, e := range r.LoadAssignment.Endpoints {
			for _, lb := range e.LbEndpoints {
				out[r.Name] = lb.Endpoint.Address.String()
			}
		}
	}
	return out, nil
}

type address struct {
	SocketAddress struct {
		Address   string `json:"address"`
		PortValue int    `json:"port_value"`
	} `json:"socket_address"`
}

func (a address) String() string {
	return net.JoinHostPort(a.SocketAddress.Address, strconv.Itoa(a.SocketAddress.PortValue))
}

// Admin reads a proxy's admin API through the unix socket in its directory.
type Admin struct {
	client *http.Client
}

// NewAdmin returns a client for the proxy whose directory is dir.
func NewAdmin(dir string) *Admin {
	sock := filepath.Join(dir, adminSocket)
	return &Admin{client: &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}}
}

func (a *Admin) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://envoy"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("envoy admin %s: %s", path, resp.Status)
	}
	return buf.Bytes(), nil
}

type hostStatus struct {
	Hostname     string            `json:"hostname"`
	Address      address           `json:"address"`
	HealthStatus map[string]any    `json:"health_status"`
	Stats        []json.RawMessage `json:"stats"`
}

func (h hostStatus) healthy() bool {
	if h.HealthStatus["eds_health_status"] != "HEALTHY" {
		return false
	}
	for k, v := range h.HealthStatus {
		if strings.HasPrefix(k, "failed_") && v == true {
			return false
		}
	}
	return true
}

// cxActive reads the cx_active gauge; Envoy omits the value when it is 0.
func (h hostStatus) cxActive() int {
	for _, raw := range h.Stats {
		var s struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if json.Unmarshal(raw, &s) == nil && s.Name == "cx_active" {
			n, _ := strconv.Atoi(s.Value)
			return n
		}
	}
	return 0
}

func (a *Admin) hosts(ctx context.Context) (map[string][]hostStatus, error) {
	body, err := a.get(ctx, "/clusters?format=json")
	if err != nil {
		return nil, err
	}
	var d struct {
		ClusterStatuses []struct {
			Name         string       `json:"name"`
			HostStatuses []hostStatus `json:"host_statuses"`
		} `json:"cluster_statuses"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	out := map[string][]hostStatus{}
	for _, c := range d.ClusterStatuses {
		out[c.Name] = c.HostStatuses
	}
	return out, nil
}

// Ready reports whether every cluster in want routes to a healthy host matching its value (the DNS
// name of a DNS cluster, or "ip:port" of a static one) and is used by an active listener, so a
// listener change written with it has been applied too.
func (a *Admin) Ready(ctx context.Context, want map[string]string) (bool, error) {
	hosts, err := a.hosts(ctx)
	if err != nil {
		return false, err
	}
	used, err := a.listenerClusters(ctx)
	if err != nil {
		return false, err
	}
	for cluster := range want {
		if !used[cluster] {
			return false, nil
		}
	}
	for cluster, endpoint := range want {
		ok := slices.ContainsFunc(hosts[cluster], func(h hostStatus) bool {
			return (h.Hostname == endpoint || h.Address.String() == endpoint) && h.healthy()
		})
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// listenerClusters returns the clusters the active listeners route to.
func (a *Admin) listenerClusters(ctx context.Context) (map[string]bool, error) {
	body, err := a.get(ctx, "/config_dump?resource=dynamic_listeners")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Configs []struct {
			ActiveState any `json:"active_state"` // warming listeners are not serving yet
		} `json:"configs"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, c := range v {
				if s, ok := c.(string); ok && k == "cluster" {
					out[s] = true
				}
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	for _, c := range doc.Configs {
		walk(c.ActiveState)
	}
	return out, nil
}

// OldConnections counts connections still open to hosts no longer in the clusters: each cluster's
// upstream_cx_active (all connections ever opened under that name) minus those of its current hosts.
func (a *Admin) OldConnections(ctx context.Context, clusters []string) (int, error) {
	hosts, err := a.hosts(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, name := range clusters {
		body, err := a.get(ctx, "/stats?filter=^cluster\\."+regexp.QuoteMeta(name)+"\\.upstream_cx_active$")
		if err != nil {
			return 0, err
		}
		_, v, _ := strings.Cut(strings.TrimSpace(string(body)), ": ")
		all, _ := strconv.Atoi(v)
		current := 0
		for _, h := range hosts[name] {
			current += h.cxActive()
		}
		total += max(0, all-current)
	}
	return total, nil
}

// WaitReady polls Ready until it is true or timeout passes.
func (a *Admin) WaitReady(ctx context.Context, want map[string]string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := a.Ready(ctx, want)
		if ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("proxy does not route to %s after %s: %w", describe(want), timeout, lastErr)
	}
	return fmt.Errorf("proxy does not route to a healthy %s after %s", describe(want), timeout)
}

// WaitDrained polls OldConnections until it is 0 or timeout passes; it reports whether it drained.
func (a *Admin) WaitDrained(ctx context.Context, clusters []string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		n, err := a.OldConnections(ctx, clusters)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func describe(want map[string]string) string {
	var parts []string
	for _, c := range slices.Sorted(maps.Keys(want)) {
		parts = append(parts, want[c])
	}
	return strings.Join(slices.Compact(parts), ", ")
}
