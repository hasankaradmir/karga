# karga

Package management for containers on a **single host**, without Kubernetes.

karga renders a versioned package (templates + values + schema) into its own small resource model
(Workload, Volume, Network, Config, Secret), applies it to Docker, and keeps every install, upgrade
and rollback as an immutable revision in a local SQLite database.

```bash
karga install cache examples/redis --set network.port=6379
karga upgrade cache examples/redis --set network.port=6379 --set config.maxmemory=128mb --dry-run
karga upgrade cache examples/redis --set network.port=6379 --set config.maxmemory=128mb
karga history cache
karga rollback cache 1
karga uninstall cache          # the data volume is kept
```

## Status

MVP. Works on one Linux or macOS host with Docker. Not available (yet): Podman, package
repositories/registries, package dependencies, a daemon that reconciles drift, Windows.

## Requirements

- Go 1.27 to build.
- A Docker Engine: Linux Docker, or OrbStack, Docker Desktop or colima on macOS. karga connects
  through `DOCKER_HOST` or, like the docker CLI, the active `docker context`.

## Install

```bash
make build            # produces bin/karga
```

## Quickstart

```bash
karga lint examples/redis
karga template cache examples/redis --set image.tag=7.4.2      # render only, touches nothing

karga install cache examples/redis --set network.port=16379
karga status cache
docker exec cache-redis redis-cli ping                         # port 16379 is published on 127.0.0.1 only

karga upgrade cache examples/redis --set network.port=16379 --set config.maxmemory=128mb --dry-run
karga upgrade cache examples/redis --set network.port=16379 --set config.maxmemory=128mb
karga history cache
karga get values cache
karga get manifest cache

karga rollback cache 1
karga uninstall cache                    # keeps the volume; --delete-volumes removes it
```

Every command accepts `-o json`. `--debug` writes logs to stderr (`--log-format json` for structured
logs). State lives in `$KARGA_HOME` (default `$XDG_DATA_HOME/karga`, i.e. `~/.local/share/karga`).

## Commands

| Command | What it does |
|---|---|
| `lint <package>...` | Validate `Package.yaml`, values and schema, render and check every resource |
| `template <release> <package>` | Render locally; Secret values are shown as `<redacted>` unless `--show-secrets` |
| `show package\|values\|readme <package>` | Show package metadata, default values or README |
| `install <release> <package>` | Install. Re-running with the same package and values does nothing |
| `upgrade <release> <package>` | Apply new values or a new package version. `--reuse-values`, `--install`, `--strategy` |
| `rollback <release> [revision]` | Re-apply an earlier revision as a new revision |
| `uninstall <release>` | Remove workloads and networks. `--delete-volumes`, `--keep-history` |
| `history`, `list`, `status <release>` | Revisions, releases, and desired vs actual state |
| `get values\|manifest <release>` | Stored values (`--all` merges defaults) or rendered resources |
| `logs <release> [workload...]` | Container logs (`--tail`, `-f`); `--proxy` for the release proxy, `--gateway` for the gateway |
| `gateway status` | Health, version and image of the host gateway, and its listeners, routes and upstreams (Linux) |
| `gateway set access-log on\|off` | Log every request and connection the gateway handles (off by default) |
| `state backup <file>` | Consistent copy of the state database, safe while karga runs |

Mutating commands take `--dry-run`, `--timeout` (default `5m`), `--history-max` (default 10
revisions kept) and `--force`. Values come from the package defaults, then each `-f/--values` file in
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

**Workload**: one container.

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
  networks: [web-net]               # the workload is reachable as "web" on these networks
  ports:
    - containerPort: 80
      hostPort: 8080                # optional, tcp: held by the gateway on Linux
      hostIP: 127.0.0.1             # default; set 0.0.0.0 to expose on every interface
    - containerPort: 3000
      routes:                       # HTTP through the gateway (Linux); excludes hostPort
        - {host: app.local, path: /, hostPort: 80, hostIP: 127.0.0.1}   # path, hostPort, hostIP are defaults
        - {host: shop.example.com, tls: {secret: shop-cert}}            # HTTPS on 443; Secret has tls.crt, tls.key
  mounts:
    - {volume: data, mountPath: /data, readOnly: false}
    - {config: site, mountPath: /etc/nginx/conf.d}      # always read-only
    - {secret: creds, mountPath: /run/secrets/creds}    # always read-only
  healthCheck: {type: exec, command: [curl, -f, http://127.0.0.1/], interval: 10s, timeout: 3s, retries: 3, startPeriod: 30s}
  resources: {memory: 512Mi, cpu: 1}
  updateStrategy: {type: recreate, drainTimeout: 10s}   # recreate | replace
```

**Volume**: `driver` (default `local`) and `retention: retain | delete` (default `retain`). A retained
volume survives uninstall and is reused by the next install of the same release.
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

### Traffic through karga's Envoys (Linux)

On Linux with a local Docker daemon, karga runs two kinds of Envoy (Envoy 1.36.13 distroless, pinned by
digest, about 150MB, one worker thread, about 15MB RAM each). Envoy updates ship with karga releases;
a new image recreates the gateway and the release proxies on the next apply, a short interruption.
`KARGA_PROXY_IMAGE` overrides the image.

- **Gateway** (`karga-gateway`, one per host, host network): holds every tcp `hostPort` and every
  HTTP `route` of every release. Workloads publish nothing else than a loopback upstream port
  (`127.0.0.1:<random>`). HTTP routes select a workload by `Host` header and path prefix, so several
  releases can share port 80; the backend receives the client address in `x-forwarded-for`. karga
  creates the gateway when a release needs it and removes it with the last one.
  `karga gateway status` shows whether it is ready and lists its listeners, routes and upstreams.
  `karga gateway set access-log on` logs every request (see `karga logs --gateway`); it is off by
  default to keep the Docker log small.
- **Release proxy** (`<release>-karga-proxy`, one per release): answers on the network for the
  release's `replace` workloads, so their network clients switch over too. Recreate workloads, such
  as databases, are reached directly.

A replace switches both: no request is lost, from the host or from other workloads. Notes:

- Replace workloads of one release cannot share a `containerPort`; the later one (by name) is
  replaced by DNS (a request may fail during the switch) and the plan says why.
- Adding or removing a replace workload reconnects the release proxy to its networks, which briefly
  interrupts proxied connections on them; changing the proxy image recreates it. The plan says so.
- **TLS:** a route with `tls: {secret: <name>}` is served as HTTPS (port 443 by default). The Secret
  holds the PEM certificate chain as `tls.crt` and the key as `tls.key`. The certificate is chosen by
  SNI, so several hosts and releases can share the port. Plain HTTP on port 80 of the same address
  redirects to HTTPS, unless port 80 already serves that host. A new certificate is a new Secret
  value: the upgrade replaces the workload and the gateway serves the new certificate. A port is
  either TLS or plain HTTP, and one host has one certificate. There is no automatic certificate
  (ACME) yet.
- The gateway is TCP and HTTP only: UDP host ports are published by Docker directly.
- The gateway runs as root with only `NET_BIND_SERVICE` and `DAC_OVERRIDE` (to bind low ports in the
  host namespace) and `no-new-privileges`. It is a single point for host traffic; it restarts
  automatically.
- Local users can reach the loopback upstream ports directly, bypassing the gateway.
- A connection open during a replace stays on the old container until it closes or `drainTimeout`
  passes. Configuration is validated before it is written; if Envoy still rejects it (for example a
  port it cannot bind), the error shows Envoy's reason.
- After a reboot or Docker restart the gateway, proxies and workloads come back on their own. If
  another process took a workload's loopback upstream port meanwhile, that workload does not start
  until the next upgrade.

Elsewhere (macOS, a remote Docker host) ports are published by Docker, `routes` are refused, a
workload with a `hostPort` falls back to recreate, and replace switches network clients by DNS: there
is no outage window, but a request that resolved the old container's address just before it stopped
can still fail, so clients should retry.

If a recreate fails, the failed container is stopped (but kept for `docker logs`), the revision is
marked failed and the previous revision stays current. Run `karga rollback <release>` to go back.
Rollback re-applies the stored desired state; it cannot undo data changes such as database
migrations. Changing a Network or Volume spec in an upgrade is refused.

## State

Everything karga knows is in `$KARGA_HOME/karga.db` (SQLite, `0600`); proxy and gateway
configuration are rebuilt from it and from Docker. `karga state backup <file>` writes a consistent
copy at any time. To restore, stop using karga and copy the file over `karga.db`. Before upgrading
the database schema karga keeps a copy as `karga.db.bak-v<schema>`, and it refuses a database written
by a newer karga.

`$KARGA_HOME/state.key` keys the database's digests of secret values. Back it up with the database
if you can keep both safe; restored without it, karga creates a new key and applies each release's
Secrets (and what mounts them) once more on its next upgrade.

## Security notes

- Access to the Docker socket is root-equivalent on the host. Run karga as a user you trust with it.
- Ports are published on `127.0.0.1` unless `hostIP` says otherwise.
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
make test-integration   # needs Docker: adapter contract and deployments
make test-e2e           # needs Docker: builds the binary and runs the lifecycle end to end
```

On Linux the integration tests also cover the gateway and release proxy.
`KARGA_TEST_DAEMON_RESTART=1 make test-integration` additionally restarts the Docker daemon (needs
passwordless sudo).

The design is in `docs/` (tech design and execution plan, kept outside version control).
