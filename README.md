# karga

Package management for containers on a **single host**, without Kubernetes.

karga renders a versioned package (templates + values + schema) into its own small resource model
(Workload, Service, Volume, Network, Config, Secret), applies it to Docker, and keeps every install,
upgrade and rollback as an immutable revision in a local SQLite database.

karga is two programs: **karga-server** owns the state, talks to Docker and keeps every release
running as declared (it repairs removed, stopped, unhealthy or hand-changed containers); the **karga**
CLI is its client on a local unix socket.

Nothing a package runs is reachable on its own. Workloads publish no port; a **Service** gives a
workload a name inside its release, and only the host's administrator decides what is reachable from
outside, by defining the listeners of the host **gateway** that Services can be exposed on.

```bash
karga install cache examples/redis
karga upgrade cache examples/redis --set config.maxmemory=128mb --dry-run
karga upgrade cache examples/redis --set config.maxmemory=128mb
karga history cache
karga rollback cache 1
karga uninstall cache          # the data volume is kept
```

## Status

MVP, for one Linux host with Docker. Not available (yet): Podman, package repositories/registries,
package dependencies, automatic certificates, lifecycle jobs. macOS and Windows are not supported: Services
need karga's gateway and release proxy, which need Linux with a local Docker daemon (the macOS build
only lints and renders packages).

## Requirements

- Linux with a local Docker Engine and systemd. karga-server connects through `DOCKER_HOST` or the
  default Docker socket.
- Go 1.27 to build.

## Install

From a release (Linux, amd64 and arm64), checked against the release checksums. The
repository is private, so the script downloads with an authenticated GitHub CLI (`gh auth login`) or
`GITHUB_TOKEN`:

```bash
gh api repos/hasankaradmir/karga/contents/scripts/install.sh -H "Accept: application/vnd.github.raw" | KARGA_SYSTEMD=1 sh
sudo usermod -aG karga $USER      # then log in again: members of the karga group may use karga
# KARGA_VERSION=v0.1.0 picks a version, KARGA_BIN_DIR another directory (default /usr/local/bin)
```

`KARGA_SYSTEMD=1` installs `karga-server` as a systemd service: a `karga` system user in the
`docker` group runs it, its state is `/var/lib/karga`, and it listens on `/run/karga/karga.sock`
(mode 0660, group `karga`). The CLI finds the socket there, or at `--socket` / `KARGA_SOCKET`.
Without the server every command but `create`, `lint`, `template`, `show`, `completion` and
`version` stops with `karga-server is not running`.

From source: `make build` produces `bin/karga` and `bin/karga-server`; the unit file is
`deploy/systemd/karga-server.service`. Shell completion: `karga completion bash|zsh|fish`.

## Quickstart

```bash
karga lint examples/redis
karga template cache examples/redis --set image.tag=7.4.2      # render only, touches nothing

karga install cache examples/redis
karga status cache
karga exec cache redis -- redis-cli ping                       # nothing is reachable from the host

# The host's administrator opens a tcp listener; the package exposes its Service on it.
karga gateway listener add redis --port 16379 --protocol tcp
karga upgrade cache examples/redis --set service.listener=redis --dry-run
karga upgrade cache examples/redis --set service.listener=redis
redis-cli -p 16379 ping                                        # through the gateway, on 127.0.0.1 only
karga history cache
karga get values cache
karga get manifest cache

karga rollback cache 1
karga uninstall cache                    # keeps the volume; --delete-volumes removes it
```

Every command accepts `-o json`. `--debug` writes logs to stderr (`--log-format json` for structured
logs). The state lives in karga-server's `KARGA_HOME` (`/var/lib/karga` for the service; the server's
`--home` or `KARGA_HOME`, else `~/.local/share/karga`). Below, `$KARGA_HOME` means that directory.

## Commands

| Command | What it does |
|---|---|
| `create <dir>` | Start a new package: a web workload with an http health check, replace updates and notes |
| `lint <package>...` | Validate `Package.yaml`, values and schema, render and check every resource |
| `template <release> <package>` | Render locally; Secret values are shown as `<redacted>` unless `--show-secrets` |
| `show package\|values\|readme <package>` | Show package metadata, default values or README |
| `version` | Versions of karga and karga-server (a warning if they differ) |
| `reconcile <release>` | Repair now: bring what runs back to the current revision (karga-server also does it on its own) |
| `pause\|resume <release>` | Stop or restart karga-server's repairs of a release (maintenance) |
| `events [release]` | Commands and repairs, newest last (`-f` follows, `-n` how many) |
| `install <release> <package>` | Install. Re-running with the same package and values does nothing |
| `upgrade <release> <package>` | Apply new values or a new package version. `--reuse-values`, `--install`, `--strategy`, `--delete-volumes` |
| `rollback <release> [revision]` | Re-apply an earlier revision as a new revision. `--strategy`, `--delete-volumes` |
| `uninstall <release>` | Remove workloads and networks. `--delete-volumes`, `--keep-history` |
| `history`, `list`, `status <release>` | Revisions (`--diff` adds what changed), releases, and desired vs actual state (`--watch`) |
| `diff <release> <rev> [rev]` | What differs between two revisions (values, package, resource fields; secrets only as changed) |
| `get values\|manifest\|notes <release>` | Stored values (`--all` merges defaults), rendered resources, or the package's notes |
| `exec <release> <workload> [-it] [-- cmd]` | Run a command in a workload (default: an interactive `sh`); exits with its code |
| `logs <release> [workload...]` | Container logs (`--tail`, `-f`); `--proxy` for the release proxy, `--gateway` for the gateway |
| `gateway status` | Health, version, image and listen addresses of the host gateway, and what it serves |
| `gateway listener list\|add\|remove` | The ports Services can be exposed on: `add <name> --port <port> --protocol tcp\|http\|https`; a listener in use cannot be removed |
| `gateway set listen <ip>[,<ip>]` | Where every listener binds (default `127.0.0.1`; `0.0.0.0` is every interface) |
| `gateway set access-log on\|off` | Log every request and connection the gateway handles (off by default) |
| `state backup <file>` | Consistent copy of the state database, safe while karga runs |
| `volume backup\|restore <release> <volume> <file>` | Copy a volume's files to/from a gzipped tar; `--stop` for a consistent backup |

Mutating commands take `--dry-run`, `--verbose` (every changed field of the plan), `--timeout`
(default `5m`), `--history-max` (default 10 revisions kept) and `--force`; they show their progress
on stderr. A package's `NOTES.txt` (a template) is shown after install and upgrade. Tables are
colored on a terminal (`--color auto|always|never`, `NO_COLOR`). Values come from the package defaults, then each `-f/--values` file in
order, then `--set` and `--set-string`; `--set a=null` deletes a default.

## Packages

```text
redis/
├── Package.yaml          # apiVersion, name, version, runtime.supported
├── values.yaml           # defaults
├── values.schema.json    # optional JSON Schema, checked before rendering
├── README.md
├── templates/            # *.yaml render to resources; _*.tpl only hold named templates
└── files/                # read from templates with .Files.Get / .Files.Glob
```

```yaml
# Package.yaml
apiVersion: karga.io/v1alpha1
name: redis
version: 1.0.0
runtime:
  supported: [docker, podman]
```

A package can also be a `.tgz` with the directory at its root.

Templates use Go `text/template` with the [Sprig](https://masterminds.github.io/sprig/) functions,
plus `include`, `tpl`, `required`, `toYaml` and `fromYaml`. The data is `.Values`,
`.Release.Name`, `.Release.Revision`, `.Package.Name`, `.Package.Version` and `.Files`. Rendering is
deterministic: functions that read the host or return different results on each run are removed
(`env`, `expandenv`, `getHostByName`, `now`, `date*`, `rand*`, `uuidv4`, `shuffle`, `gen*`,
`encryptAES`, `bcrypt`, `htpasswd`), and `keys`/`values` return sorted results. `--strict` rejects
values that the schema does not declare.

Mark passwords and tokens `"writeOnly": true` in `values.schema.json`. They are used to render but are
stored and printed as `<redacted>`; `--reuse-values` asks for them again.

## Resources

All resources use `apiVersion: karga.io/v1alpha1` and `metadata.name` (a DNS label). Runtime objects
are named `<release>-<name>` and labelled `io.karga.*`.

**Workload**: one container. It publishes nothing and its name in the package is not a DNS name:
other workloads and outside clients reach it through a Service. (Docker still resolves the container
name, `<release>-<name>`, on the release's networks; there is no network policy yet.)

```yaml
kind: Workload
metadata: {name: web}
spec:
  image: nginx:1.27
  pullPolicy: if-not-present        # always | if-not-present | never
  restartPolicy: always             # always | on-failure | unless-stopped | never
  command: []
  args: []
  environment: {KEY: value}
  networks: [web-net]               # Services of this workload are reachable on these networks
  ports:                            # what the container listens on, for Services to refer to
    - {name: http, containerPort: 80}           # protocol defaults to tcp
  mounts:
    - {volume: data, mountPath: /data, readOnly: false}
    - {config: site, mountPath: /etc/nginx/conf.d}      # always read-only
    - {secret: creds, mountPath: /run/secrets/creds}    # always read-only
  healthCheck: {type: exec, command: [curl, -f, http://127.0.0.1/], interval: 10s, timeout: 3s, retries: 3, startPeriod: 30s}
  # or {type: http, port: 80, path: /healthz} / {type: tcp, port: 80}: no tool needed in the image
  resources: {memory: 512Mi, cpu: 1, pids: 1024}  # memory also caps swap; pids defaults to 1024 (0: no limit)
  logging: {maxSize: 10m, maxFiles: 3}            # the defaults: at most 30MB of container log
  security:                                       # noNewPrivileges defaults to true
    noNewPrivileges: true
    readOnlyRootFilesystem: false                 # true: image read-only, /tmp is a 64m tmpfs
    capDrop: [ALL]
    capAdd: [NET_BIND_SERVICE]
    user: "1000:1000"
  updateStrategy: {type: recreate, drainTimeout: 10s}   # recreate | replace
  stopTimeout: 10s                                # default; time to shut down after SIGTERM before a kill (1s-1h)
```

**Service**: a stable name for one workload inside the release, served by the release proxy, and
optionally exposed on the host gateway's listeners.

```yaml
kind: Service
metadata: {name: shop}              # other workloads of the release connect to shop:80
spec:
  workload: web
  ports:
    - {name: http, port: 80, targetPort: http}   # targetPort: a port name or number of the workload (default: name)
  expose:                                        # optional; listeners are defined by the host's administrator
    - {port: http, listener: web, host: shop.example.com, path: /}           # http listener: by Host and path prefix
    - {port: http, listener: websecure, host: shop.example.com, tls: {secret: shop-cert}}   # https; Secret has tls.crt, tls.key
    # - {port: db, listener: pg}                 # tcp listener: belongs to this one service
```

One release proxy serves every Service of a release on one address, so a port number belongs to one
Service of the release. A workload is reachable only on the networks it joins, so a Service's
workload must join one.

**Health checks:** `exec` runs a command inside the container (Docker's healthcheck), so the image
needs that tool. `tcp` (the port accepts a connection) and `http` (GET `path` answers 2xx/3xx) are run
by karga from the host against the container's address and need nothing in the image. Behind the
release proxy, Envoy checks the same HTTP path before switching traffic to a new container. All types take `interval`,
`timeout`, `retries` and `startPeriod`, and any of them makes a workload eligible for replace.

**Limits and defaults:** Docker enforces memory (without extra swap), CPU and the process limit;
`karga status` shows restarts and OOM kills. Container logs rotate (json-file). Workloads run with
`no-new-privileges`; an image that needs setuid (e.g. `sudo`) needs `noNewPrivileges: false`.
Releases installed by an earlier karga apply their workloads once more for these defaults; the plan
lists the new fields.

**Volume**: `driver` (default `local`) and `retention: retain | delete` (default `retain`). A retained
volume survives uninstall and is reused by the next install of the same release. An upgrade or
rollback that would delete a `retention: delete` volume stops unless given `--delete-volumes`.
**Network**: `driver` (default `bridge`) and `internal`.
**Config**: `files: {name: content}`, mounted read-only.
**Secret**: `values: {name: content}`, mounted read-only as files.

## Upgrades

Before any change karga pulls every image it needs, so a bad tag fails without touching what runs.
`--dry-run` shows the plan: what is created, updated, deleted or unchanged, and which fields change.

- **recreate** (default): stop the old container, then start the new one. There is a short outage.
- **replace**: start the new container next to the old one, switch traffic to it once it is healthy,
  then let the old one drain for up to `drainTimeout` and remove it. If the new container never
  becomes healthy, the old one keeps running and the release stays deployed. Replace only applies
  when the workload mounts volumes read-only and has a healthcheck; otherwise the plan falls back to
  recreate and says why.

### Traffic through karga's Envoys

karga runs two kinds of Envoy (Envoy 1.36.13 distroless, pinned by digest, about 150MB, one worker
thread, about 15MB RAM each, limited to 128Mi, 0.5 CPU and 256 processes). Envoy updates ship with
karga releases; a new image recreates the gateway and the release proxies on the next apply, a short
interruption. `KARGA_PROXY_IMAGE` overrides the image.

- **Release proxy** (`<release>-karga-proxy`, one per release with Services): answers to the release's
  Service names on its networks and forwards each Service port to the current container of its
  workload. Workloads carry only a per-revision alias meant for the proxy. It publishes the
  exposed Service ports on loopback upstream ports (`127.0.0.1:<random>`) for the gateway.
- **Gateway** (`karga-gateway`, one per host, host network): the host's only entry point. Its
  settings belong to the host's administrator, not to packages: the addresses it listens on
  (`karga gateway set listen`, default `127.0.0.1`) and its **listeners** (`karga gateway listener
  add`): a name, a port and a protocol. A new host has `web` (80, http) and `websecure` (443,
  https). A Service exposes a port on a listener by name; exposing on a listener that does not exist,
  of another protocol, or that another release holds is refused before anything changes. HTTP and
  HTTPS listeners select a Service by `Host` header and path prefix, so several releases share them;
  the backend receives the client address in `x-forwarded-for`. A tcp listener belongs to one Service.
  Listeners nobody uses are not opened. karga creates the gateway when a Service is exposed and
  removes it with the last one. `karga gateway status` shows whether it is ready and what it serves.
  `karga gateway set access-log on` logs every request (see `karga logs --gateway`).

A replace switches the release proxy, which every client goes through: no request is lost, from
outside or from other workloads. Notes:

- Adding or removing a Service reconnects the release proxy to its networks, and exposing a new port
  recreates it; both briefly interrupt proxied connections. The plan says so.
- **TLS:** an exposure on an https listener names a Secret with the PEM certificate chain as
  `tls.crt` and the key as `tls.key`. The certificate is chosen by SNI, so several hosts and releases
  share the listener. Plain HTTP on the port-80 http listener redirects to HTTPS, unless it serves that
  host itself. A new certificate is a new Secret value; the gateway serves it after the upgrade. One
  host has one certificate. There is no automatic certificate (ACME) yet.
- Services carry TCP only; there is no UDP.
- The gateway runs as root with only `NET_BIND_SERVICE` and `DAC_OVERRIDE` (to bind low ports in the
  host namespace) and `no-new-privileges`. It is a single point for outside traffic; it restarts
  automatically.
- Local users can reach the loopback upstream ports directly, bypassing the gateway.
- A connection open during a replace stays on the old container until it closes or `drainTimeout`
  passes. Configuration is validated before it is written; if Envoy still rejects it (for example a
  port it cannot bind), the error shows Envoy's reason.
- After a reboot or Docker restart the gateway, proxies and workloads come back on their own. If
  another process took a proxy's loopback upstream port meanwhile, the proxy does not start until the
  next upgrade.

If a recreate fails, the failed container is stopped (but kept for `docker logs`), the revision is
marked failed and the previous revision stays current. Run `karga rollback <release>` to go back.
Rollback re-applies the stored desired state; it cannot undo data changes such as database
migrations. Changing a Network or Volume spec in an upgrade is refused.

## State

Everything karga knows is in `$KARGA_HOME/karga.db` (SQLite, `0600`), which only karga-server
opens; proxy and gateway configuration are rebuilt from it and from Docker. `karga state backup
<file>` writes a consistent copy at any time (the server streams it to the CLI). To restore, stop
karga-server and copy the file over `karga.db`. Before upgrading
the database schema karga keeps a copy as `karga.db.bak-v<schema>`, and it refuses a database written
by a newer karga.

`karga volume backup <release> <volume> <file.tar.gz>` copies a volume's files through a workload
that mounts it (no helper container). It runs live by default; `--stop` stops the workloads
mounting the volume for a consistent copy (for databases prefer it, or the database's own dump).
`karga volume restore` stops them, writes the files back (overwriting files of the same name,
keeping others) and starts them again.

`$KARGA_HOME/state.key` keys the database's digests of secret values. Back it up with the database
if you can keep both safe; restored without it, karga creates a new key and applies each release's
Secrets (and what mounts them) once more on its next upgrade.

## Security notes

- Access to the Docker socket is root-equivalent on the host, and so is access to karga's socket:
  add to the `karga` group only users you trust with it. The socket is local only; there is no
  remote API. karga-server runs as the unprivileged `karga` user (in the `docker` group), with
  `NoNewPrivileges`, a read-only system and no access to home directories (packages and values files
  travel from the CLI over the socket).
- Packages cannot open ports: workloads publish nothing, and only the gateway's listeners, which the
  host's administrator defines, are reachable from outside, on `127.0.0.1` unless `karga gateway set
  listen` says otherwise.
- Containers are never privileged and never join the host PID, network or IPC namespaces, except the
  gateway, which uses the host network (see above). Release proxies run as the karga user. Proxy and
  gateway configuration and admin sockets are in `0700` directories under `$KARGA_HOME/proxies/` and
  `$KARGA_HOME/gateway/`.
- Config and Secret files live under `$KARGA_HOME/releases/<release>/<revision>/`. That directory is
  `0700`; the files inside are `0644` so any user in the container can read them. The state database
  is `0600`.
- Secret values never appear in output, logs or the database. The database keeps HMAC-SHA256
  digests of them (keyed by `$KARGA_HOME/state.key`, `0600`) to detect changes; without the key they
  cannot be used to guess a password. Releases written by an older karga re-apply their Secrets once.
- TLS keys are copied into `$KARGA_HOME/gateway/certs/` (`0600`, in a `0700` directory) for the
  gateway.
- Image pulls are anonymous; private registries are not supported yet.

## Development

```bash
make test               # unit tests
make lint               # golangci-lint
make test-integration   # Linux with Docker: adapter contract and deployments
make test-e2e           # Linux with Docker: builds both binaries, starts karga-server, runs the lifecycle
make test-linux         # both in a Linux VM (OrbStack or Lima; KARGA_VM names it, default karga-linux)
```

Integration and e2e tests run on Linux only (they stop with a message elsewhere). `make test-linux` creates
and provisions the VM on first use (`KARGA_VM` names it) and runs `TARGETS` there (default
`test-integration test-e2e`). Integration tests run in parallel and share one `KARGA_HOME`.
`KARGA_TEST_DAEMON_RESTART=1 make test-integration` additionally restarts the Docker daemon (needs
passwordless sudo).

Releases: add the version to `CHANGELOG.md`, then `git tag vX.Y.Z && git push --tags`; the release
workflow builds the binaries with GoReleaser and creates a draft GitHub release with the changelog
section as notes; publish it to make it the latest release. `goreleaser release --snapshot --clean` builds the same locally.

Documentation:

- [USAGE.md](USAGE.md): how to use karga, scenario by scenario.
- [docs/design.md](docs/design.md): how karga works.
- [docs/roadmap.md](docs/roadmap.md): planned features, known limitations, risks, open questions.
- [docs/adr/](docs/adr/): architecture decisions, and the implementation decisions in
  [docs/adr/decisions.md](docs/adr/decisions.md).

## License

Apache License 2.0; see [LICENSE](LICENSE).
