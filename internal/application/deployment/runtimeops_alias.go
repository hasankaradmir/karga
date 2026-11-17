package deployment

import "github.com/hasankaradmir/karga/internal/application/deployment/runtimeops"

// Names from runtimeops, which the gateway shares with the executor.
var (
	// RuntimeName is the runtime object name of a release-local resource name.
	RuntimeName    = runtimeops.RuntimeName
	removeWorkload = runtimeops.RemoveWorkload
	ipOverlap      = runtimeops.IPOverlap
	portTaken      = runtimeops.PortTaken
	httpHealthPath = runtimeops.HTTPHealthPath
	proxySupported = &runtimeops.Supported
	// UseProxies turns the gateway and release proxies on when the runtime supports them.
	UseProxies  = runtimeops.UseProxies
	envoyLimits = runtimeops.EnvoyLimits
)

const (
	stopTimeout       = runtimeops.StopTimeout
	proxyReadyTimeout = runtimeops.ProxyReadyTimeout
	envoyMemory       = runtimeops.EnvoyMemory
	envoyMilliCPU     = runtimeops.EnvoyMilliCPU
	envoyPids         = runtimeops.EnvoyPids
)

// ProxyControl writes an Envoy's configuration and reads its state.
type ProxyControl = runtimeops.ProxyControl

// EnvoyProxies controls Envoy proxies through their directories.
type EnvoyProxies = runtimeops.EnvoyProxies
