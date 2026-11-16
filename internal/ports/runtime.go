// Package ports defines the boundaries karga's application layer talks to.
package ports

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Errors every Runtime implementation wraps (with %w) so callers can react to them.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("already exists")
	ErrPortInUse     = errors.New("port already in use")
	ErrImageNotFound = errors.New("image not found")
	ErrUnavailable   = errors.New("runtime unavailable")
	ErrPermission    = errors.New("permission denied")
)

// Capability strings reported in RuntimeInfo.
const (
	CapResourcesMemory = "resources.memory"
	CapResourcesCPU    = "resources.cpu"
	CapHealthcheckExec = "healthcheck.exec"
)

// CapNetworkDriver and CapVolumeDriver build driver capability strings.
func CapNetworkDriver(d string) string { return "network.driver/" + d }

// CapVolumeDriver returns the capability string for a volume driver.
func CapVolumeDriver(d string) string { return "volume.driver/" + d }

// Labels karga puts on every runtime object. ListManaged selects on LabelManaged and LabelRelease.
const (
	LabelManaged   = "io.karga.managed"
	LabelRelease   = "io.karga.release"
	LabelKind      = "io.karga.kind"
	LabelName      = "io.karga.name"
	LabelRevision  = "io.karga.revision"
	LabelRetention = "io.karga.retention"
)

// Kinds returned by ListManaged.
const (
	ObjectWorkload = "workload"
	ObjectNetwork  = "network"
	ObjectVolume   = "volume"
)

// Runtime is a container runtime (Docker, later Podman). Objects are addressed by name.
// Create returns ErrConflict if the name exists; Delete of a missing object succeeds.
type Runtime interface {
	Info(ctx context.Context) (RuntimeInfo, error)
	// EnsureImage makes ref available according to policy (always, if-not-present, never).
	EnsureImage(ctx context.Context, ref, policy string) error

	CreateNetwork(ctx context.Context, req NetworkRequest) error
	DeleteNetwork(ctx context.Context, name string) error
	CreateVolume(ctx context.Context, req VolumeRequest) error
	DeleteVolume(ctx context.Context, name string) error

	// CreateWorkload creates a container without starting it.
	CreateWorkload(ctx context.Context, req WorkloadRequest) error
	StartWorkload(ctx context.Context, name string) error
	StopWorkload(ctx context.Context, name string, timeout time.Duration) error
	// DeleteWorkload removes the container (stopping it if needed). Named volumes are kept.
	DeleteWorkload(ctx context.Context, name string) error
	GetWorkload(ctx context.Context, name string) (WorkloadStatus, error)

	// ListManaged returns every karga-managed object of a release.
	ListManaged(ctx context.Context, release string) ([]Object, error)
	// PublishedPorts returns host ports published by any container.
	PublishedPorts(ctx context.Context) ([]PublishedPort, error)
}

// RuntimeInfo describes the runtime and what it supports.
type RuntimeInfo struct {
	Name         string
	Version      string
	Capabilities []string
}

// Has reports whether the runtime has capability c.
func (i RuntimeInfo) Has(c string) bool { return slices.Contains(i.Capabilities, c) }

// NetworkRequest creates a network.
type NetworkRequest struct {
	Name     string
	Labels   map[string]string
	Driver   string
	Internal bool
}

// VolumeRequest creates a named volume.
type VolumeRequest struct {
	Name   string
	Labels map[string]string
	Driver string
}

// WorkloadRequest creates a container.
type WorkloadRequest struct {
	Name    string
	Labels  map[string]string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	// User overrides the image user ("uid[:gid]"). Empty keeps the image default.
	User          string
	Ports         []PortBinding
	Mounts        []Mount
	Networks      []string
	Aliases       []string
	HealthCheck   *HealthCheck
	MemoryBytes   int64
	MilliCPU      int64
	RestartPolicy string
}

// PortBinding publishes ContainerPort. HostPort 0 means the port is not published.
type PortBinding struct {
	ContainerPort int
	HostPort      int
	HostIP        string
	Protocol      string
}

// Mount types.
const (
	MountVolume = "volume"
	MountBind   = "bind"
)

// Mount attaches a named volume or a host path.
type Mount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

// HealthCheck runs Command inside the container.
type HealthCheck struct {
	Command  []string
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
}

// Workload states and health values.
const (
	StateCreated = "created"
	StateRunning = "running"
	StateExited  = "exited"

	HealthNone      = "none"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// WorkloadStatus is the observed state of a container.
type WorkloadStatus struct {
	Name     string
	State    string
	Health   string
	ExitCode int
	Labels   map[string]string
}

// Object is a managed runtime object.
type Object struct {
	Kind   string
	Name   string
	Labels map[string]string
}

// PublishedPort is a host port bound by a container.
type PublishedPort struct {
	HostIP   string
	Port     int
	Protocol string
	// Release is the owning karga release, empty for containers karga does not manage.
	Release string
	// Workload is the container name.
	Workload string
}
