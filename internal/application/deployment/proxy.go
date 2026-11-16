package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
)

// KindProxy is the derived resource for a workload's Envoy proxy (ADR-009). It is not written in
// packages: karga adds it to the plan for workloads with updateStrategy.proxy.
const KindProxy = "Proxy"

// proxySupported is false off Linux: the proxy relies on bind-mounted unix sockets and inotify.
var proxySupported = runtime.GOOS == "linux"

// ProxyName is the container name of a workload's proxy.
func ProxyName(release, workload string) string {
	return release + "-" + resource.ReservedPrefix + "proxy-" + workload
}

// BackendAlias is the network alias a workload container of a revision gets behind its proxy.
func BackendAlias(workload string, revision int) string {
	return fmt.Sprintf("%s%s-r%d", resource.ReservedPrefix, workload, revision)
}

// ProxyDir holds a proxy's configuration and admin socket. It outlives revisions.
func ProxyDir(home, release, workload string) string {
	return filepath.Join(home, "proxies", release, workload)
}

// ProxySpec is the desired proxy of a workload: what forces a proxy recreate when it changes.
// The active backend is not part of it; switching backends only rewrites the proxy's clusters.
type ProxySpec struct {
	Image    string          `json:"image"`
	Ports    []resource.Port `json:"ports"`
	Networks []string        `json:"networks"`
}

func proxyRequested(w *resource.WorkloadSpec) bool {
	return w.UpdateStrategy != nil && w.UpdateStrategy.Proxy
}

// proxyReason says why a workload that asks for a proxy cannot have one ("" if it can).
func proxyReason(w *resource.WorkloadSpec) string {
	switch {
	case !proxySupported:
		return "proxy requires Linux"
	case len(w.Networks) == 0:
		return "proxy needs the workload on a network"
	case len(w.Ports) == 0:
		return "proxy needs at least one port"
	}
	for _, p := range w.Ports {
		if p.Protocol == "udp" {
			return fmt.Sprintf("udp port %d is not proxied", p.ContainerPort)
		}
	}
	return ""
}

// proxied reports whether a workload runs behind a proxy: it asks for one and can be replaced.
func proxied(w *resource.WorkloadSpec) bool {
	typ, _ := eligible(w)
	return proxyRequested(w) && typ == resource.StrategyReplace
}

func proxySpecFor(w *resource.WorkloadSpec) ProxySpec {
	return ProxySpec{Image: envoy.Image(), Ports: w.Ports, Networks: w.Networks}
}

func proxyApplied(releaseName, name string, w *resource.WorkloadSpec) (release.AppliedResource, error) {
	js, err := json.Marshal(proxySpecFor(w))
	if err != nil {
		return release.AppliedResource{}, err
	}
	sum := sha256.Sum256(js)
	return release.AppliedResource{Kind: KindProxy, Name: name, RuntimeName: ProxyName(releaseName, name),
		Spec: js, Digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}
