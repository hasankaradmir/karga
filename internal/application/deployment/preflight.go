package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"syscall"

	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

// Preflight finds problems before anything is created: runtime support, missing capabilities and
// host ports or gateway routes taken by other releases, containers or host processes. Ports already
// held by the same release are allowed (an upgrade will replace those containers).
func Preflight(ctx context.Context, rt ports.Runtime, info ports.RuntimeInfo, release string, supported []string, rs []resource.Resource) error {
	var errs []error
	if !slices.Contains(supported, info.Name) {
		errs = append(errs, fmt.Errorf("package does not support runtime %q (runtime.supported: %v)", info.Name, supported))
	}
	need := func(r resource.Resource, capability, what string) {
		if !info.Has(capability) {
			errs = append(errs, fmt.Errorf("%s %q: %s is not supported by %s %s", r.Kind, r.Metadata.Name, what, info.Name, info.Version))
		}
	}
	for _, r := range rs {
		switch s := r.Spec.(type) {
		case *resource.NetworkSpec:
			need(r, ports.CapNetworkDriver(s.Driver), "network driver "+s.Driver)
		case *resource.VolumeSpec:
			need(r, ports.CapVolumeDriver(s.Driver), "volume driver "+s.Driver)
			if len(s.MountOptions) > 0 {
				errs = append(errs, fmt.Errorf("%s %q: spec.mountOptions is not supported yet", r.Kind, r.Metadata.Name))
			}
		case *resource.WorkloadSpec:
			if s.Resources != nil && s.Resources.Memory != "" {
				need(r, ports.CapResourcesMemory, "a memory limit")
			}
			if s.Resources != nil && s.Resources.CPU != "" {
				need(r, ports.CapResourcesCPU, "a cpu limit")
			}
			if s.HealthCheck != nil {
				need(r, ports.CapHealthcheckExec, "an exec healthcheck")
			}
		}
	}

	published, err := rt.PublishedPorts(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, r := range rs {
		w, ok := r.Spec.(*resource.WorkloadSpec)
		if !ok {
			continue
		}
		for _, p := range w.Ports {
			if p.HostPort == 0 || slices.ContainsFunc(gatewayPorts(w), func(g resource.Port) bool { return g.ContainerPort == p.ContainerPort }) {
				continue // the gateway holds it, checked below
			}
			if err := checkHostPort(release, r.Metadata.Name, p, published); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if proxySupported.Load() {
		errs = append(errs, checkGateway(ctx, rt, release, rs)...)
	}
	return errors.Join(errs...)
}

func checkHostPort(release, workload string, p resource.Port, published []ports.PublishedPort) error {
	where := fmt.Sprintf("%s %q: hostPort %s:%d/%s", resource.KindWorkload, workload, p.HostIP, p.HostPort, p.Protocol)
	for _, pub := range published {
		if pub.Port != p.HostPort || pub.Protocol != p.Protocol || !ipOverlap(pub.HostIP, p.HostIP) {
			continue
		}
		switch pub.Release {
		case release:
			return nil
		case "":
			return fmt.Errorf("%s is used by container %s: %w", where, pub.Workload, ports.ErrPortInUse)
		default:
			return fmt.Errorf("%s is used by release %s (%s): %w", where, pub.Release, pub.Workload, ports.ErrPortInUse)
		}
	}
	if portTaken(p.Protocol, p.HostIP, p.HostPort) {
		return fmt.Errorf("%s is taken by another process on this host: %w", where, ports.ErrPortInUse)
	}
	return nil
}

// ipOverlap reports whether two bind addresses can conflict: equal, or either binds every address.
func ipOverlap(a, b string) bool {
	if a == b {
		return true
	}
	unspecified := func(s string) bool {
		ip := net.ParseIP(s)
		return s == "" || (ip != nil && ip.IsUnspecified())
	}
	return unspecified(a) || unspecified(b)
}

// portTaken test-binds a host port. Only "address in use" counts: karga usually runs without root,
// and Docker or the gateway bind privileged ports that karga itself may not.
func portTaken(protocol, ip string, port int) bool {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	var err error
	if protocol == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			_ = c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			_ = l.Close()
		}
	}
	return errors.Is(err, syscall.EADDRINUSE)
}
