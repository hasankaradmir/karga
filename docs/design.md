# karga design

This document describes how karga works today. Architecture decisions and their reasons are in
[`adr/`](adr/); the implementation decision log is [`adr/decisions.md`](adr/decisions.md); what is
planned and what is known to be missing is in [`roadmap.md`](roadmap.md).

## 1. Goals and non-goals

karga installs, configures, upgrades and rolls back containerized applications on **one host**,
from versioned packages, without Kubernetes and with as few resources as possible.

Goals:

- Packages: versioned, templated, configurable through values validated by a JSON Schema.
- Releases: every install, upgrade and rollback is an immutable revision with a reviewable plan.
- Upgrades without downtime where the workload allows it, and a clear plan where it does not.
- Small footprint: a CLI and ordinary containers; no daemon, no control plane.
- Runtime independence: the package format does not depend on Docker.

Non-goals: multi-node orchestration, scheduling, horizontal scaling, Kubernetes compatibility, a
package marketplace, automatic database migration rollback.

## 2. Architecture

```
cmd/karga                       entry point (signals, exit codes)
internal/cli                    cobra commands, output (tables, JSON, progress, colors, completion)
internal/application/packages   load, lint, render packages; create scaffolds
internal/application/releases   release lifecycle: install, upgrade, rollback, uninstall, reads
internal/application/deployment plan (diff, strategies), execute, release proxy, probes
  .../deployment/gateway        the host gateway
  .../deployment/runtimeops     primitives shared by the executor and the gateway
internal/domain/{pkg,release,resource}   pure model and validation
internal/ports                  Runtime and StateStore interfaces; runtime contract suite
internal/adapters/{docker,sqlite,fakeruntime}
internal/infrastructure/{renderer,values,envoy,archive,config,logger}
```

The domain packages depend on nothing else in karga, enforced by `depguard` in `.golangci.yml`.
The application layer talks to the container runtime and the state only through `ports`. Every
runtime adapter passes the same contract suite (`internal/ports/runtimetest`); the fake runtime used
by unit tests passes it too.

## 3. Packages

A package is a directory or `.tgz`:

```
Package.yaml          apiVersion karga.io/v1alpha1, name, version (semver), runtime.supported
values.yaml           defaults
values.schema.json    optional JSON Schema; "writeOnly": true marks secret values
templates/*.yaml      resources; templates/_*.tpl hold named templates
files/                read with .Files.Get / .Files.Glob
NOTES.txt             optional template shown after install and upgrade
README.md
```

Rendering uses Go `text/template` with Sprig. Functions that read the host (`env`, `expandenv`,
`getHostByName`) or are not deterministic (time, random, generated keys, `bcrypt`, ...) fail the
render, so the same package and values always give the same resources. The context is `.Values`,
`.Release.Name`, `.Release.Revision`, `.Package.Name`, `.Package.Version` and `.Files`.

Values are the package defaults overlaid with `-f` files in order, then `--set` and
`--set-string` (`--set a=null` deletes a default), then validated against the schema (`--strict`
rejects undeclared keys). The package format is alpha (`v1alpha1`): it may still change.

## 4. Resource model

| Kind | Purpose |
|------|---------|
| Workload | One container: image, command, environment, networks, ports (host ports or HTTP(S) routes), mounts, health check, resources, logging, security, update strategy. |
| Volume | A named Docker volume; `retention: retain` (default) keeps it across uninstalls. |
| Network | A bridge network; workloads are reachable by their name on it. |
| Config | Files mounted read-only. |
| Secret | Values mounted read-only as files; the state keeps only keyed digests. |

`resource.Decode` applies defaults (host IP 127.0.0.1, 1024 processes, log rotation 10m×3,
`no-new-privileges`, health check port and path) and validates each resource; `ValidateSet`
checks references across the set (mounted volumes, configs and secrets, networks, TLS secrets with
`tls.crt` and `tls.key`, duplicate host ports). Because defaults are written into the spec, a new
default shows up in the plan as a field change.

## 5. Releases and state

The state is a SQLite database (`$KARGA_HOME/karga.db`, mode 0600, WAL):

| Table | Holds |
|-------|-------|
| releases | name, package, status, current revision |
| revisions | immutable: package digest, values (writeOnly redacted), rendered manifest (secret values as digests), notes, status |
| resources | what is applied now, one row per resource: runtime name, spec, digest |
| operations | running and finished operations with their steps, PID and hostname |

Migrations are numbered SQL files applied in order; the version is SQLite's `user_version`.
Opening and migrating hold `karga.db.lock`; before migrating an existing database karga copies it
to `karga.db.bak-v<n>` and refuses a database written by a newer karga.

Every mutating command records an operation first. An operation whose process is alive on this
host locks the release; one whose process is gone is aborted only with `--force`. A failed apply
marks the revision failed; an install that failed before creating anything removes the release; if nothing that was running changed (an image that cannot be pulled, an
unhealthy new container in a replace), the release keeps its status. Rollback re-applies a stored
revision as a new revision. `--history-max` (default 10) prunes old revisions.

Digests of secret material (Secret values, values, manifests) are HMAC-SHA256 keyed by
`$KARGA_HOME/state.key` (32 random bytes, created on first use), so a weak password cannot be
guessed offline from the database.

## 6. Planning

`deployment.Diff` compares the desired resources with the applied ones by digest. Applied
workloads, networks and volumes that no longer exist in the runtime are left out first, so the plan
creates them again (removed by hand, or a state restored on another host). A workload's
digest covers its spec and the content of the Configs and Secrets it mounts or serves TLS with, so
changing a config file re-applies the workloads using it. Changing a Network or a Volume is refused
(uninstall or rename instead). Each change gets an action (create, update, delete, unchanged) and,
for workloads, a strategy with the reason when it falls back. The CLI shows a summary; `--verbose`
shows every changed field.

## 7. Applying

`deployment.Execute`:

1. Pre-flight: runtime support, capabilities, host ports and routes (against other releases and
   other processes); pulls every image before touching anything running.
2. Materializes Config and Secret files under `$KARGA_HOME/releases/<release>/<revision>/`.
3. Deletes removed workloads, then creates and updates resources in dependency order.
4. Waits for recreated workloads; ones that do not become ready are stopped, not removed, so their
   logs can be read. A recreated workload's new container mounts the same named volumes, and the
   anonymous volumes Docker made for image `VOLUME` paths no mount covers are moved to it (as
   docker compose does); they are deleted only with the workload for good (removed from the
   package or uninstalled). A workload with such volumes is never replaced.
5. Updates the release proxy and the gateway, then deletes removed networks and volumes (a removed
   volume only with `retention: delete`; `uninstall --delete-volumes` removes them all).

Progress events (pull, create, wait, switch, drain, stop, done) are reported as they happen.

### Update strategies

- **recreate** (default): stop the old container, start the new one. Short outage.
- **replace:** start `<name>-next` next to the old container, wait until healthy, switch traffic,
  let the old one drain for `drainTimeout`, remove it and rename `<name>-next`. Needs a health
  check and read-only volumes (two writers on the same data are unsafe); otherwise the plan falls
  back to recreate and says why. An interrupted replace is repaired before the next plan.

## 8. Traffic: the gateway and the release proxy (Linux)

With a local Docker daemon on Linux (runtime capability `karga.host-gateway`), traffic goes through
karga-managed Envoys (ADR-0010):

- **Gateway** (`karga-gateway`, one per host, host network) serves every tcp `hostPort` and every
  HTTP(S) route of every release. Workload containers publish only a loopback upstream port chosen
  by karga and recorded in a container label. The gateway configuration is rebuilt from those labels
  under `$KARGA_HOME/gateway.lock`: TCP listeners, HTTP listeners with virtual hosts by `Host` and
  path prefix, TLS listeners with one filter chain per host (SNI) and a redirect from port 80.
  During a replace the gateway is pointed at `<name>-next`; that switch is stored
  (`gateway-routing.json`) so another release's update does not undo it.
- **Release proxy** (`<release>-karga-proxy`) carries the network aliases of the release's replace
  workloads and routes each port to the alias of the current revision's container, so clients on
  the network switch without loss as well.

Configuration is built from Envoy's protobuf types, validated, and written atomically as files that
Envoy watches; listeners are written only once Envoy knows their clusters. karga waits for Envoy's
admin API (a unix socket) to report the new backend healthy and used by an active listener before
it continues, and reverts on failure. Without the capability (macOS, a remote Docker host), Docker
publishes host ports, routes are refused, and replace switches network clients by DNS.

## 9. Health checks

- `exec`: Docker's healthcheck runs a command in the container.
- `tcp` and `http`: karga checks the port from the host (through the gateway upstream, a published
  port, or on Linux the container's address), so the image needs no tool. Behind the proxies Envoy
  checks the same HTTP path before routing to a new container.

All take `interval`, `timeout`, `retries` and `startPeriod`; failures during the start period do not
count.

## 10. Security model

- Access to the Docker socket is root-equivalent; karga is meant for the host's administrator.
- Containers are never privileged and run with `no-new-privileges` by default; the `security` block
  adds a read-only root filesystem, capability changes and a user. The gateway runs as root with only
  `NET_BIND_SERVICE` and `DAC_OVERRIDE`.
- Host ports bind to 127.0.0.1 unless `hostIP` says otherwise.
- Secret files are mounted read-only from a 0700 directory; secret values never appear in output,
  logs or the database (only keyed digests); `writeOnly` values are stored as `<redacted>`.
- Memory limits also cap swap; every container has a process limit and log rotation.
- Templates cannot read the host environment or the network.

## 11. Testing

- Unit tests with the fake runtime (`make test`, with `-race`).
- The runtime contract suite against Docker, including what Docker actually enforces (limits,
  logging, security options).
- Integration tests against Docker (`make test-integration`), run in parallel with one shared
  `KARGA_HOME`; the gateway, proxy and TLS tests run on Linux (`make test-linux` uses a VM).
  Zero-loss claims are measured with clients on the host and on the release network.
- End-to-end tests drive the built binary (`make test-e2e`).
