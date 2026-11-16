package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// KindProxy is the derived resource for a release's Envoy proxy (ADR-010). It is not written in
// packages: karga adds it to the plan when the release has workloads to replace on a network.
const KindProxy = "Proxy"

// ProxyResource is the name of the release proxy in the plan and the applied state.
const ProxyResource = "proxy"

// proxySupported is set by UseProxies: karga's proxies rely on bind-mounted unix sockets, inotify
// and (the gateway) the host network namespace, so only a local Docker daemon on Linux has them.
var proxySupported = false

// UseProxies turns the gateway and release proxies on when the runtime supports them. The
// release service calls it once it knows the runtime, before planning.
func UseProxies(info ports.RuntimeInfo) { proxySupported = info.Has(ports.CapHostGateway) }

// ProxyName is the container name of a release's proxy.
func ProxyName(release string) string {
	return release + "-" + resource.ReservedPrefix + "proxy"
}

// BackendAlias is the network alias a workload container of a revision gets behind the proxy.
func BackendAlias(workload string, revision int) string {
	return fmt.Sprintf("%s%s-r%d", resource.ReservedPrefix, workload, revision)
}

// ProxyDir holds a release proxy's configuration and admin socket. It outlives revisions.
func ProxyDir(home, release string) string {
	return filepath.Join(home, "proxies", release)
}

// ProxySpec is the desired proxy container: what forces a recreate or a network reconnect when it
// changes. Listeners and backends are not part of it; they only rewrite the proxy's files.
type ProxySpec struct {
	Image string `json:"image"`
	// Networks maps each network to the aliases the proxy answers to on it.
	Networks map[string][]string `json:"networks"`
}

// proxyCluster names the cluster of one proxied workload port.
func proxyCluster(workload string, port int) string { return fmt.Sprintf("%s-p%d", workload, port) }

// proxied decides which workloads run behind the release proxy: those that ask for replace, can be
// replaced and are on a network. The proxy listens on each one's container ports, so a port can be
// proxied for one workload only. reasons says why a replace workload is not proxied.
func proxied(rs []resource.Resource) (set map[string]*resource.WorkloadSpec, reasons map[string]string) {
	set, reasons = map[string]*resource.WorkloadSpec{}, map[string]string{}
	if !proxySupported {
		return set, reasons
	}
	var names []string
	specs := map[string]*resource.WorkloadSpec{}
	for _, r := range rs {
		if w, ok := r.Spec.(*resource.WorkloadSpec); ok {
			names, specs[r.Metadata.Name] = append(names, r.Metadata.Name), w
		}
	}
	sort.Strings(names)
	taken := map[int]string{}
	for _, n := range names {
		w := specs[n]
		if typ, _ := w.Strategy(); typ != resource.StrategyReplace {
			continue
		}
		if typ, _ := eligible(w); typ != resource.StrategyReplace {
			continue
		}
		if reason := proxyReason(w, taken); reason != "" {
			reasons[n] = reason
			continue
		}
		for _, p := range w.Ports {
			taken[p.ContainerPort] = n
		}
		set[n] = w
	}
	return set, reasons
}

func proxyReason(w *resource.WorkloadSpec, taken map[int]string) string {
	switch {
	case len(w.Networks) == 0:
		return "not on a network"
	case len(w.Ports) == 0:
		return "no ports to proxy"
	}
	for _, p := range w.Ports {
		if p.Protocol == "udp" {
			return fmt.Sprintf("udp port %d is not proxied", p.ContainerPort)
		}
		if other, ok := taken[p.ContainerPort]; ok {
			return fmt.Sprintf("containerPort %d already proxied for %s", p.ContainerPort, other)
		}
	}
	return ""
}

func proxySpecFor(set map[string]*resource.WorkloadSpec) ProxySpec {
	spec := ProxySpec{Image: envoy.Image(), Networks: map[string][]string{}}
	for name, w := range set {
		for _, n := range w.Networks {
			spec.Networks[n] = append(spec.Networks[n], name)
		}
	}
	for n := range spec.Networks {
		sort.Strings(spec.Networks[n])
	}
	return spec
}

func proxyApplied(releaseName string, spec ProxySpec) (release.AppliedResource, error) {
	js, err := json.Marshal(spec)
	if err != nil {
		return release.AppliedResource{}, err
	}
	sum := sha256.Sum256(js)
	return release.AppliedResource{Kind: KindProxy, Name: ProxyResource, RuntimeName: ProxyName(releaseName),
		Spec: js, Digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// appliedProxied returns the workloads the applied release proxy answers for.
func appliedProxied(applied []release.AppliedResource) map[string]bool {
	out := map[string]bool{}
	for _, a := range applied {
		if a.Kind != KindProxy {
			continue
		}
		if a.Name != ProxyResource {
			out[a.Name] = true // a per-workload proxy of an earlier karga version
			continue
		}
		var spec ProxySpec
		_ = json.Unmarshal(a.Spec, &spec)
		for _, names := range spec.Networks {
			for _, n := range names {
				out[n] = true
			}
		}
	}
	return out
}

// Proxy strategies: the container is recreated (image), or reconnected to networks whose aliases changed.
const (
	proxyRecreate  = "recreate"
	proxyReconnect = "reconnect"
)

// proxyChange plans the release proxy. Docker cannot change a running container's image or
// aliases: an image change recreates it, an alias change reconnects it to the affected networks.
// Both briefly interrupt the proxied connections of what they touch.
func proxyChange(releaseName string, set map[string]*resource.WorkloadSpec, current map[Key]release.AppliedResource) (Change, error) {
	spec := proxySpecFor(set)
	want, err := proxyApplied(releaseName, spec)
	if err != nil {
		return Change{}, err
	}
	c := Change{Kind: KindProxy, Name: ProxyResource, Spec: want.Spec, Digest: want.Digest, RuntimeName: want.RuntimeName, Proxy: &spec}
	have, exists := current[Key{KindProxy, ProxyResource}]
	switch {
	case !exists:
		c.Action = ActionCreate
	case have.Digest == want.Digest:
		c.Action = ActionUnchanged
	default:
		c.Action, c.Applied = ActionUpdate, &have
		c.Fields = fieldChanges(KindProxy, have.Spec, want.Spec)
		var old ProxySpec
		_ = json.Unmarshal(have.Spec, &old)
		if old.Image != spec.Image {
			c.Strategy, c.StrategyReason = proxyRecreate, "image changed: brief interruption"
		} else {
			c.Strategy, c.StrategyReason = proxyReconnect, strings.Join(changedNetworks(old, spec), ", ")+": brief interruption"
		}
	}
	return c, nil
}

// changedNetworks lists the networks whose proxy aliases differ between two specs.
func changedNetworks(a, b ProxySpec) []string {
	var out []string
	for n := range a.Networks {
		if !slices.Equal(a.Networks[n], b.Networks[n]) {
			out = append(out, n)
		}
	}
	for n := range b.Networks {
		if _, ok := a.Networks[n]; !ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
