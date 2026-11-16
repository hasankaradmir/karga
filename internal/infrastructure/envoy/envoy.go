// Package envoy writes the file-based configuration of a karga proxy and reads its admin API
// (ADR-009). The proxy routes each TCP port to a backend DNS alias; switching the alias in cds.json
// moves new connections to another container while existing ones finish on the old one.
package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MountPath is where the proxy directory is mounted inside the Envoy container.
const MountPath = "/etc/karga-proxy"

// DefaultImage is the Envoy image validated in the spike (1.36.13). KARGA_PROXY_IMAGE overrides it.
const DefaultImage = "envoyproxy/envoy:v1.36-latest"

// Image returns the proxy image to use.
func Image() string {
	if v := os.Getenv("KARGA_PROXY_IMAGE"); v != "" {
		return v
	}
	return DefaultImage
}

// Command runs Envoy on the mounted bootstrap file.
func Command() []string {
	return []string{"envoy", "-c", MountPath + "/envoy.json", "--log-level", "warn"}
}

const (
	bootstrapFile = "envoy.json"
	listenersFile = "lds.json"
	clustersFile  = "cds.json"
	adminSocket   = "admin.sock"
)

func clusterName(port int) string { return "p" + strconv.Itoa(port) }

func sorted(ports []int) []int {
	out := slices.Clone(ports)
	slices.Sort(out)
	return slices.Compact(out)
}

// resources wraps typed Envoy resources the way path_config_source expects them.
func resources(items []map[string]any) ([]byte, error) {
	return json.MarshalIndent(map[string]any{"resources": items}, "", "  ")
}

// Bootstrap is the static part: an admin API on a unix socket and file-based LDS/CDS.
func Bootstrap(node string) ([]byte, error) {
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
		"admin": map[string]any{"address": map[string]any{"pipe": map[string]any{"path": MountPath + "/" + adminSocket, "mode": 0o600}}},
		"dynamic_resources": map[string]any{
			"lds_config": source(listenersFile),
			"cds_config": source(clustersFile),
		},
	}, "", "  ")
}

// Listeners has one TCP listener per port, proxying to the cluster of the same port.
func Listeners(ports []int) ([]byte, error) {
	var items []map[string]any
	for _, p := range sorted(ports) {
		items = append(items, map[string]any{
			"@type":   "type.googleapis.com/envoy.config.listener.v3.Listener",
			"name":    clusterName(p),
			"address": map[string]any{"socket_address": map[string]any{"address": "0.0.0.0", "port_value": p}},
			"filter_chains": []any{map[string]any{"filters": []any{map[string]any{
				"name": "envoy.filters.network.tcp_proxy",
				"typed_config": map[string]any{
					"@type":                "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
					"stat_prefix":          clusterName(p),
					"cluster":              clusterName(p),
					"max_connect_attempts": 3,
				},
			}}}},
		})
	}
	return resources(items)
}

// Clusters has one cluster per port resolving backend by DNS (strict DNS semantics): container
// restarts and IP changes do not matter, only the alias does.
func Clusters(ports []int, backend string) ([]byte, error) {
	var items []map[string]any
	for _, p := range sorted(ports) {
		items = append(items, map[string]any{
			"@type":           "type.googleapis.com/envoy.config.cluster.v3.Cluster",
			"name":            clusterName(p),
			"connect_timeout": "1s",
			// The DnsCluster extension replaces the deprecated type/dns_refresh_rate fields.
			"cluster_type": map[string]any{
				"name": "envoy.cluster.dns",
				"typed_config": map[string]any{
					"@type":                            "type.googleapis.com/envoy.extensions.clusters.dns.v3.DnsCluster",
					"dns_refresh_rate":                 "1s",
					"dns_lookup_family":                "V4_ONLY",
					"all_addresses_in_single_endpoint": false,
				},
			},
			"load_assignment": map[string]any{
				"cluster_name": clusterName(p),
				"endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{
					"endpoint": map[string]any{"address": map[string]any{"socket_address": map[string]any{"address": backend, "port_value": p}}},
				}}}},
			},
			"health_checks": []any{map[string]any{
				"timeout": "1s", "interval": "0.5s", "unhealthy_threshold": 1, "healthy_threshold": 1,
				"tcp_health_check": map[string]any{},
			}},
		})
	}
	return resources(items)
}

// writeAtomic replaces dir/name with a rename, which is the change Envoy's directory watch picks up.
func writeAtomic(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// WriteAll creates the proxy directory (0700) with bootstrap, listeners and clusters.
func WriteAll(dir, node string, ports []int, backend string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	boot, err := Bootstrap(node)
	if err != nil {
		return err
	}
	lds, err := Listeners(ports)
	if err != nil {
		return err
	}
	if err := writeAtomic(dir, bootstrapFile, boot); err != nil {
		return err
	}
	if err := writeAtomic(dir, listenersFile, lds); err != nil {
		return err
	}
	return SetBackend(dir, ports, backend)
}

// SetBackend points every port at backend.
func SetBackend(dir string, ports []int, backend string) error {
	cds, err := Clusters(ports, backend)
	if err != nil {
		return err
	}
	return writeAtomic(dir, clustersFile, cds)
}

// Backend returns the alias the proxy currently routes to, or "" if there is no config.
func Backend(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, clustersFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		Resources []struct {
			LoadAssignment struct {
				Endpoints []struct {
					LbEndpoints []struct {
						Endpoint struct {
							Address struct {
								SocketAddress struct {
									Address string `json:"address"`
								} `json:"socket_address"`
							} `json:"address"`
						} `json:"endpoint"`
					} `json:"lb_endpoints"`
				} `json:"endpoints"`
			} `json:"load_assignment"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Join(dir, clustersFile), err)
	}
	for _, r := range cfg.Resources {
		for _, e := range r.LoadAssignment.Endpoints {
			for _, lb := range e.LbEndpoints {
				return lb.Endpoint.Address.SocketAddress.Address, nil
			}
		}
	}
	return "", nil
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

// Ready reports whether every port's cluster routes to a healthy host named backend.
func (a *Admin) Ready(ctx context.Context, ports []int, backend string) (bool, error) {
	hosts, err := a.hosts(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range sorted(ports) {
		ok := slices.ContainsFunc(hosts[clusterName(p)], func(h hostStatus) bool { return h.Hostname == backend && h.healthy() })
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// OldConnections counts connections still open to hosts no longer in the clusters: each cluster's
// upstream_cx_active (all connections ever opened under that name) minus those of its current hosts.
func (a *Admin) OldConnections(ctx context.Context, ports []int) (int, error) {
	hosts, err := a.hosts(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, p := range sorted(ports) {
		name := clusterName(p)
		body, err := a.get(ctx, "/stats?filter=^cluster\\."+name+"\\.upstream_cx_active$")
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
func (a *Admin) WaitReady(ctx context.Context, ports []int, backend string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := a.Ready(ctx, ports, backend)
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
		return fmt.Errorf("proxy does not route to %s after %s: %w", backend, timeout, lastErr)
	}
	return fmt.Errorf("proxy does not route to a healthy %s after %s", backend, timeout)
}

// WaitDrained polls OldConnections until it is 0 or timeout passes; it reports whether it drained.
func (a *Admin) WaitDrained(ctx context.Context, ports []int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		n, err := a.OldConnections(ctx, ports)
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
