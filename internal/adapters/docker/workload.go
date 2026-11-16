package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/hasankaradmir/karga/internal/ports"
)

// EnsureImage implements ports.Runtime.
// shortcut: pulls are anonymous; private registries need credentials from docker config (planned with M4 registry auth).
func (r *Runtime) EnsureImage(ctx context.Context, ref, policy string) error {
	if policy != "always" {
		_, err := r.c.ImageInspect(ctx, ref)
		switch {
		case err == nil:
			return nil
		case !cerrdefs.IsNotFound(err):
			return wrap(err, "inspect image "+ref)
		case policy == "never":
			return fmt.Errorf("image %s is not present locally and pullPolicy is never: %w", ref, ports.ErrImageNotFound)
		}
	}
	resp, err := r.c.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err == nil {
		err = resp.Wait(ctx)
		_ = resp.Close()
	}
	if err == nil {
		return nil
	}
	msg := err.Error()
	if cerrdefs.IsNotFound(err) || strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "pull access denied") {
		return fmt.Errorf("image %s was not found in its registry; check the repository and tag (%s): %w",
			ref, strings.TrimPrefix(msg, "Error response from daemon: "), ports.ErrImageNotFound)
	}
	return wrap(err, "pull image "+ref)
}

var restartModes = map[string]container.RestartPolicyMode{
	"always":         container.RestartPolicyAlways,
	"on-failure":     container.RestartPolicyOnFailure,
	"unless-stopped": container.RestartPolicyUnlessStopped,
	"never":          container.RestartPolicyDisabled,
	"":               container.RestartPolicyDisabled,
}

// CreateWorkload implements ports.Runtime.
func (r *Runtime) CreateWorkload(ctx context.Context, req ports.WorkloadRequest) error {
	mode, ok := restartModes[req.RestartPolicy]
	if !ok {
		return fmt.Errorf("create workload %s: unknown restart policy %q", req.Name, req.RestartPolicy)
	}
	cfg := &container.Config{
		Image:        req.Image,
		Cmd:          append(append([]string{}, req.Command...), req.Args...),
		Env:          envList(req.Env),
		Labels:       req.Labels,
		User:         req.User,
		ExposedPorts: network.PortSet{},
	}
	host := &container.HostConfig{
		PortBindings:  network.PortMap{},
		RestartPolicy: container.RestartPolicy{Name: mode},
		Resources:     container.Resources{Memory: req.MemoryBytes, NanoCPUs: req.MilliCPU * 1_000_000},
		Sysctls:       req.Sysctls,
		CapAdd:        req.CapAdd,
		CapDrop:       req.CapDrop,
		// Explicit even though these are Docker defaults (tech design §17.5).
		Privileged: false,
	}
	if hc := req.HealthCheck; hc != nil {
		cfg.Healthcheck = &container.HealthConfig{
			Test: append([]string{"CMD"}, hc.Command...), Interval: hc.Interval, Timeout: hc.Timeout, Retries: hc.Retries,
			StartPeriod: hc.StartPeriod,
		}
	}
	for _, p := range req.Ports {
		port, ok := network.PortFrom(uint16(p.ContainerPort), network.IPProtocol(p.Protocol))
		if !ok {
			return fmt.Errorf("create workload %s: invalid port %d/%s", req.Name, p.ContainerPort, p.Protocol)
		}
		cfg.ExposedPorts[port] = struct{}{}
		if p.HostPort == 0 {
			continue
		}
		ip, err := netip.ParseAddr(p.HostIP)
		if err != nil {
			return fmt.Errorf("create workload %s: host IP %q: %w", req.Name, p.HostIP, err)
		}
		host.PortBindings[port] = append(host.PortBindings[port], network.PortBinding{HostIP: ip, HostPort: strconv.Itoa(p.HostPort)})
	}
	for _, m := range req.Mounts {
		t := mount.TypeVolume
		if m.Type == ports.MountBind {
			t = mount.TypeBind
		}
		host.Mounts = append(host.Mounts, mount.Mount{Type: t, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	if req.NoNewPrivileges {
		host.SecurityOpt = []string{"no-new-privileges"}
	}
	var netCfg *network.NetworkingConfig
	if req.HostNetwork {
		host.NetworkMode = network.NetworkHost
	}
	if len(req.Networks) > 0 {
		host.NetworkMode = container.NetworkMode(req.Networks[0])
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			req.Networks[0]: {Aliases: req.Aliases},
		}}
	}

	if _, err := r.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: req.Name, Config: cfg, HostConfig: host, NetworkingConfig: netCfg,
	}); err != nil {
		return wrap(err, "create workload "+req.Name)
	}
	for _, n := range req.Networks[min(1, len(req.Networks)):] {
		if _, err := r.c.NetworkConnect(ctx, n, client.NetworkConnectOptions{
			Container: req.Name, EndpointConfig: &network.EndpointSettings{Aliases: req.Aliases},
		}); err != nil {
			// Do not leave a half-configured container behind.
			_ = r.DeleteWorkload(ctx, req.Name)
			return wrap(err, fmt.Sprintf("connect workload %s to network %s", req.Name, n))
		}
	}
	return nil
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// StartWorkload implements ports.Runtime.
func (r *Runtime) StartWorkload(ctx context.Context, name string) error {
	_, err := r.c.ContainerStart(ctx, name, client.ContainerStartOptions{})
	err = wrap(err, "start workload "+name)
	if errors.Is(err, ports.ErrPortInUse) {
		return fmt.Errorf("%w (another process or container uses a published host port)", err)
	}
	return err
}

// StopWorkload implements ports.Runtime.
func (r *Runtime) StopWorkload(ctx context.Context, name string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	_, err := r.c.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &secs})
	return wrap(err, "stop workload "+name)
}

// DeleteWorkload implements ports.Runtime.
func (r *Runtime) DeleteWorkload(ctx context.Context, name string) error {
	// RemoveVolumes only removes anonymous volumes (image VOLUME declarations); named volumes always survive.
	_, err := r.c.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return ignoreNotFound(wrap(err, "delete workload "+name))
}

// GetWorkload implements ports.Runtime.
func (r *Runtime) GetWorkload(ctx context.Context, name string) (ports.WorkloadStatus, error) {
	res, err := r.c.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return ports.WorkloadStatus{}, wrap(err, "inspect workload "+name)
	}
	st := ports.WorkloadStatus{Name: name, Health: ports.HealthNone}
	if s := res.Container.State; s != nil {
		st.State = string(s.Status)
		st.ExitCode = s.ExitCode
		if s.Health != nil && s.Health.Status != "" {
			st.Health = string(s.Health.Status)
		}
	}
	if res.Container.Config != nil {
		st.Labels = res.Container.Config.Labels
	}
	return st, nil
}

func managedFilter(release string) client.Filters {
	return client.Filters{}.
		Add("label", ports.LabelManaged+"=true").
		Add("label", ports.LabelRelease+"="+release)
}

// ListManaged implements ports.Runtime.
func (r *Runtime) ListManaged(ctx context.Context, release string) ([]ports.Object, error) {
	var out []ports.Object
	cs, err := r.c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: managedFilter(release)})
	if err != nil {
		return nil, wrap(err, "list workloads")
	}
	for _, c := range cs.Items {
		if len(c.Names) > 0 {
			out = append(out, ports.Object{Kind: ports.ObjectWorkload, Name: strings.TrimPrefix(c.Names[0], "/"), Labels: c.Labels})
		}
	}
	ns, err := r.c.NetworkList(ctx, client.NetworkListOptions{Filters: managedFilter(release)})
	if err != nil {
		return nil, wrap(err, "list networks")
	}
	for _, n := range ns.Items {
		out = append(out, ports.Object{Kind: ports.ObjectNetwork, Name: n.Name, Labels: n.Labels})
	}
	vs, err := r.c.VolumeList(ctx, client.VolumeListOptions{Filters: managedFilter(release)})
	if err != nil {
		return nil, wrap(err, "list volumes")
	}
	for _, v := range vs.Items {
		out = append(out, ports.Object{Kind: ports.ObjectVolume, Name: v.Name, Labels: v.Labels})
	}
	return out, nil
}

// Logs implements ports.Runtime.
func (r *Runtime) Logs(ctx context.Context, name string, tail int) (string, error) {
	rc, err := r.c.ContainerLogs(ctx, name, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(tail)})
	if err != nil {
		return "", wrap(err, "logs of "+name)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	// Containers without a TTY multiplex stdout and stderr.
	if _, err := stdcopy.StdCopy(&out, &out, rc); err != nil {
		return "", fmt.Errorf("logs of %s: %w", name, err)
	}
	return out.String(), nil
}

// ListLabeled implements ports.Runtime.
func (r *Runtime) ListLabeled(ctx context.Context, key string) ([]ports.Object, error) {
	cs, err := r.c.ContainerList(ctx, client.ContainerListOptions{All: true,
		Filters: client.Filters{}.Add("label", ports.LabelManaged+"=true").Add("label", key)})
	if err != nil {
		return nil, wrap(err, "list workloads")
	}
	var out []ports.Object
	for _, c := range cs.Items {
		if len(c.Names) > 0 {
			out = append(out, ports.Object{Kind: ports.ObjectWorkload, Name: strings.TrimPrefix(c.Names[0], "/"), Labels: c.Labels})
		}
	}
	return out, nil
}

// PublishedPorts implements ports.Runtime.
func (r *Runtime) PublishedPorts(ctx context.Context) ([]ports.PublishedPort, error) {
	cs, err := r.c.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, wrap(err, "list containers")
	}
	var out []ports.PublishedPort
	for _, c := range cs.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		for _, p := range c.Ports {
			if p.PublicPort == 0 {
				continue
			}
			ip := ""
			if p.IP.IsValid() {
				ip = p.IP.String()
			}
			out = append(out, ports.PublishedPort{
				HostIP: ip, Port: int(p.PublicPort), Protocol: p.Type,
				Release: c.Labels[ports.LabelRelease], Workload: name,
			})
		}
	}
	return out, nil
}

// RenameWorkload implements ports.Runtime.
func (r *Runtime) RenameWorkload(ctx context.Context, from, to string) error {
	_, err := r.c.ContainerRename(ctx, from, client.ContainerRenameOptions{NewName: to})
	return wrap(err, fmt.Sprintf("rename workload %s to %s", from, to))
}

// ConnectNetwork implements ports.Runtime.
func (r *Runtime) ConnectNetwork(ctx context.Context, workload, net string, aliases []string) error {
	_, err := r.c.NetworkConnect(ctx, net, client.NetworkConnectOptions{
		Container: workload, EndpointConfig: &network.EndpointSettings{Aliases: aliases},
	})
	return wrap(err, fmt.Sprintf("connect workload %s to network %s", workload, net))
}

// DisconnectNetwork implements ports.Runtime.
func (r *Runtime) DisconnectNetwork(ctx context.Context, workload, net string) error {
	_, err := r.c.NetworkDisconnect(ctx, net, client.NetworkDisconnectOptions{Container: workload})
	if err != nil && strings.Contains(err.Error(), "is not connected") {
		return nil
	}
	return wrap(err, fmt.Sprintf("disconnect workload %s from network %s", workload, net))
}
