# Using karga

This guide walks through karga by task: what to run, what you will see, and what to do when
something goes wrong. The command list is in the [README](README.md); how karga works inside is in
[docs/design.md](docs/design.md). Outputs below are from real runs, shortened; client addresses are
replaced by documentation addresses.

- [1. Concepts in five minutes](#1-concepts-in-five-minutes)
- [2. Install karga](#2-install-karga)
- [3. Your first release](#3-your-first-release)
- [4. Configure with values](#4-configure-with-values)
- [5. Upgrade safely](#5-upgrade-safely)
- [6. Expose services through the gateway (Linux)](#6-expose-services-through-the-gateway-linux)
- [7. Upgrades without downtime](#7-upgrades-without-downtime)
- [8. Databases and other stateful services](#8-databases-and-other-stateful-services)
- [9. Secrets](#9-secrets)
- [10. Resources and hardening](#10-resources-and-hardening)
- [11. Operate and troubleshoot](#11-operate-and-troubleshoot)
- [12. Back up and move a host](#12-back-up-and-move-a-host)
- [13. Write a package](#13-write-a-package)
- [14. Uninstall and clean up](#14-uninstall-and-clean-up)
- [15. Limits to know](#15-limits-to-know)

## 1. Concepts in five minutes

| Term | Meaning |
|------|---------|
| **Package** | A directory (or `.tgz`) with `Package.yaml`, `values.yaml`, an optional `values.schema.json`, `templates/` and an optional `NOTES.txt`. |
| **Values** | The settings of a package: its defaults, overridden with `-f file.yaml` and `--set key=value`. |
| **Resources** | What templates render to: `Workload` (a container), `Volume`, `Network`, `Config` (files), `Secret`. |
| **Release** | A package installed under a name, e.g. `shop`. Its containers are named `<release>-<name>` (`shop-web`). |
| **Revision** | Each install, upgrade and rollback is a new, immutable revision of the release. |
| **Plan** | What a command will change: created (`+`), updated (`~`) and deleted (`-`) resources, and for workloads how (recreate or replace). |
| **Gateway** | On Linux, one Envoy container (`karga-gateway`) serves every release's host ports and HTTP(S) routes. |
| **Release proxy** | On Linux, one Envoy per release (`<release>-karga-proxy`) that lets other containers keep working while a workload is replaced. |

Where things live:

- Docker runs the containers. karga is a CLI: when it is not running, nothing of karga runs except
  your containers, the gateway and the release proxies, all restarted by Docker.
- `$KARGA_HOME` (default `~/.local/share/karga`) holds the state database `karga.db`, the key
  `state.key`, rendered Config and Secret files, and the Envoy configuration. Keep it private; it is
  created with mode 0700.

## 2. Install karga

### From a release

The repository is private, so downloads need GitHub credentials: an authenticated GitHub CLI
(`gh auth login`) or a `GITHUB_TOKEN` with read access. The install script checks the archive
against the release checksums:

```console
$ gh api repos/hasankaradmir/karga/contents/scripts/install.sh -H "Accept: application/vnd.github.raw" | sh
karga v0.1.0 installed to /usr/local/bin/karga
```

`KARGA_VERSION=v0.1.0` picks a version, `KARGA_BIN_DIR=~/bin` another directory. Without the
script: `gh release download v0.1.0 -R hasankaradmir/karga -p 'karga_*_linux_amd64.tar.gz'`.

### From source

```console
$ make build        # needs Go 1.27; produces bin/karga
```

### Connect to Docker

karga uses `DOCKER_HOST`, then `DOCKER_CONTEXT`, then the current Docker context, so it works with
Docker on Linux and with OrbStack, Docker Desktop or colima on macOS without extra setup.

### Shell completion

```console
$ karga completion zsh > "${fpath[1]}/_karga"     # bash: karga completion bash > /etc/bash_completion.d/karga
```

Completion offers release, workload, volume and revision names from your state.

### What works where

| Feature | Linux, local Docker | macOS, or a remote Docker host |
|---------|--------------------|-------------------------------|
| Install, upgrade, rollback, status, logs, exec, backups | yes | yes |
| Host ports | through the gateway | published by Docker |
| HTTP routes, TLS, several releases on port 80 | yes | refused by the plan |
| Replace without dropping requests | from the host and from other containers | only without host ports; containers switch by DNS |
| `tcp`/`http` health checks | always | only on published ports |

## 3. Your first release

Create a package to start from, check it, look at what it renders and what it would do:

```console
$ karga create shop
Created package shop. Try: karga lint shop && karga install <release> shop

$ karga lint shop
1 package(s) linted, 0 failed

$ karga template demo shop --set service.port=18080
# Source: shop/templates/web.yaml
kind: Workload
metadata:
  name: web
spec:
  image: "nginx:1.27-alpine"
  ports:
    - containerPort: 80
      hostPort: 18080
      hostIP: 127.0.0.1
  healthCheck:
    type: http
...

$ karga install demo shop --set service.port=18080 --dry-run
Deployment Plan

Release:  demo
Revision: 1

  + NETWORK net
  + WORKLOAD web

2 to create, 0 to update, 0 to delete, 0 unchanged.
```

Install it. Progress goes to stderr; the package's notes are printed at the end:

```console
$ karga install demo shop --set service.port=18080
[  0s] workload web: image nginx:1.27-alpine (if-not-present)
[  0s] workload web: creating demo-web
[  2s] workload demo-web: waiting for demo-web to be ready (...)
Release "demo" installed (revision 1).
  create network net
  create workload web

NOTES:
demo (revision 1) runs nginx:1.27-alpine.

Open http://127.0.0.1:18080/
Logs:   karga logs demo -f
Shell:  karga exec demo web
```

Look at it:

```console
$ karga status demo
Release:  demo
Status:   deployed
Revision: 1

KIND      NAME  RUNTIME NAME  REVISION  DESIRED  ACTUAL   RESTARTS  HEALTH/BACKEND
Network   net   demo-net      1         applied  present
Workload  web   demo-web      1         applied  running  0         healthy

$ karga logs demo --tail 3
192.0.2.1 - - [17/Nov/2026:10:18:06 +0000] "GET / HTTP/1.1" 200 615 "-" "curl/8.7.1" "-"

$ karga exec demo web -- nginx -v
nginx version: nginx/1.27.5

$ karga list
NAME  REVISION  STATUS    PACKAGE  VERSION  UPDATED
demo  1         deployed  shop     0.1.0    2026-11-17 13:18:06
```

`karga exec demo web` without a command opens an interactive shell; `-it` gives any command a
terminal. `karga status demo --watch` refreshes every two seconds.

Installing the same package with the same values again does nothing: `Release "demo" is already
installed with the same package and values`.

## 4. Configure with values

See a package's defaults with `karga show values <package>`. Override them, later sources winning:

```console
$ karga install db examples/postgres -f prod.yaml --set network.port=15432 --set database.password=s3cret-pw
```

- `-f file.yaml` (repeatable), then `--set a.b=c` (types inferred), then `--set-string a=007`.
- `--set key=null` removes a default.
- `upgrade` starts from the package defaults; add `--reuse-values` to keep the last revision's
  overrides and change only what you pass.

Values are checked against the package's schema before anything happens:

```console
$ karga install demo2 shop --set service.port=99999
Error: values do not match values.schema.json:
- at '/service/port': maximum: got 99,999, want 65,535
```

What a release was installed with (`--all` adds the defaults, `--revision N` an older one):

```console
$ karga get values db
database:
  password: <redacted>
network:
  port: 15432
```

Values the schema marks `"writeOnly": true` (passwords, tokens) are used to render but stored and
shown as `<redacted>`; `--reuse-values` cannot bring them back, so pass them again on each upgrade
(a values file kept outside git works well).

## 5. Upgrade safely

### Read the plan first

```console
$ karga upgrade demo shop --reuse-values --set image.tag=1.27.3-alpine --dry-run
Revision: 1 -> 2

  ~ WORKLOAD web (recreate: publishes hostPort 18080/tcp without the gateway (Linux, tcp))

0 to create, 1 to update, 0 to delete, 1 unchanged. --verbose shows the changed fields.
```

`--verbose` lists every changed field. The text in parentheses is the update strategy and, when a
workload asked for `replace` but cannot have it, why:

- **recreate**: the old container stops, then the new one starts. A short outage.
- **replace**: the new container starts next to the old one and takes over once healthy. Needs a
  health check, read-only volumes, and on macOS no host port (see [section 7](#7-upgrades-without-downtime)).

`--strategy recreate|replace` overrides the packages' choice for one upgrade or rollback.

### Upgrade

```console
$ karga upgrade demo shop --reuse-values --set image.tag=1.27.3-alpine
[  0s] workload web: image nginx:1.27.3-alpine (if-not-present)
[  9s] workload web: stopping and removing demo-web
[  9s] workload web: creating demo-web
Release "demo" upgraded (revision 2).
  update workload web (recreate: ...)
```

Images are pulled before anything running is touched. A tag that does not exist changes nothing:

```console
$ karga upgrade demo shop --reuse-values --set image.tag=does-not-exist
Error: Workload "web": image nginx:does-not-exist was not found in its registry; check the repository and tag (...)
revision 3 failed; nothing that was running changed and revision 2 is still serving
```

The same holds when a replaced workload's new container never becomes healthy. When a recreate
fails, the new container is stopped (not removed, so `karga logs` still works), the revision is
marked failed and the previous revision stays current; go back with a rollback.

### History, diff and rollback

```console
$ karga history demo --diff
REVISION  STATUS    PACKAGE  VERSION  CREATED              DESCRIPTION       CHANGES
1         deployed  shop     0.1.0    2026-11-17 13:18:00  Install complete  first
2         deployed  shop     0.1.0    2026-11-17 13:18:06  Upgrade complete  +0 ~1 -0, 1 values
3         failed    shop     0.1.0    2026-11-17 13:18:21  Upgrade failed: ...

$ karga diff demo 1
Release demo: revision 1 -> 2

Values:
  image.tag: "1.27-alpine" -> "1.27.3-alpine"

Resources:
  ~ WORKLOAD web
      spec.image: "nginx:1.27-alpine" -> "nginx:1.27.3-alpine"

$ karga rollback demo --dry-run
Revision: 2 -> 4
  ~ WORKLOAD web (recreate: ...)
```

`karga rollback demo` returns to the last good revision before the current one; `karga rollback
demo 1` to a given one. A rollback is a new revision. It re-applies configuration; it cannot undo
changes your application made to its data (database migrations, for example).

## 6. Expose services through the gateway (Linux)

On Linux every host port and route of every release is served by the gateway, so several releases
can share port 80 and upgrades do not drop host traffic.

### Host ports and routes

A workload port is either a tcp **host port** (one release owns it) or a list of HTTP **routes**
(shared by host name and path prefix):

```yaml
ports:
  - containerPort: 5432
    hostPort: 5432               # 127.0.0.1 unless hostIP says otherwise
  - containerPort: 80
    routes:
      - host: shop.local         # path defaults to /, hostPort to 80 (443 with tls), hostIP to 127.0.0.1
        path: /
```

Two releases on one port:

```console
$ karga install shop shop --set route.host=shop.local
$ karga install blog blog --set route.host=blog.local

$ curl -s -o /dev/null -w "%{http_code}\n" -H "Host: shop.local" http://127.0.0.1/
200
$ curl -s -o /dev/null -w "%{http_code}\n" -H "Host: nobody.local" http://127.0.0.1/
404

$ karga gateway status
Gateway:    running, ready (Envoy 1.36.13)
Image:      envoyproxy/envoy:distroless-v1.36.13@sha256:...
Access log: off

LISTENER      ROUTE        RELEASE  WORKLOAD  UPSTREAM
127.0.0.1:80  blog.local/  blog     web       127.0.0.1:35593
127.0.0.1:80  shop.local/  shop     web       127.0.0.1:39435
```

A host and path that another release already serves is refused before anything changes:

```console
$ karga install other shop --set route.host=shop.local
Error: Workload "web": route shop.local/ on 127.0.0.1:80 is already routed to shop: port already in use
```

The release is then listed as failed with revision 0; remove it with `karga uninstall other`.

Set `hostIP: 0.0.0.0` on a port or route to listen on every interface. Applications behind routes
receive the client address in `x-forwarded-for`.

### TLS

Add `tls: {secret: <name>}` to a route. The Secret holds the PEM certificate chain as `tls.crt` and
the key as `tls.key`; the route moves to port 443 and plain HTTP on port 80 redirects to it:

```yaml
kind: Secret
metadata: {name: blog-cert}
spec:
  values:
    tls.crt: |
      -----BEGIN CERTIFICATE-----
      ...
    tls.key: |
      -----BEGIN PRIVATE KEY-----
      ...
---
# in the workload
      routes:
        - host: blog.local
          tls: {secret: blog-cert}
```

```console
$ curl -s -o /dev/null -w "%{http_code}\n" --resolve blog.local:443:127.0.0.1 --cacert cert.pem https://blog.local/
200
$ curl -s -o /dev/null -w "%{http_code} %{redirect_url}\n" -H "Host: blog.local" http://127.0.0.1/
301 https://blog.local/
```

Keep the certificate out of the package: render the Secret from values passed with `-f` from a
file outside git, and mark them `writeOnly`. To renew a certificate, upgrade with the new one; the
workload is replaced and the gateway serves the new certificate. There is no automatic certificate
(ACME) yet.

### Gateway access log

```console
$ karga gateway set access-log on
$ karga logs --gateway -f
[2026-11-17T04:58:08.117Z] "GET /logged HTTP/1.1" 404 - 0 124 2 1 "192.0.2.57" "Go-http-client/1.1" ...
```

It is off by default to keep the log small; turning it on or off does not interrupt traffic.

### On macOS

Docker publishes host ports itself and `routes` are refused (`routes need the gateway (Linux with a
local Docker)`). Use host ports, one per release.

## 7. Upgrades without downtime

Ask for replace in the workload:

```yaml
healthCheck:
  type: http        # or tcp, or exec with a command
  port: 80
  path: /
  interval: 5s
  startPeriod: 10s  # failures in the first 10s do not count
updateStrategy:
  type: replace
  drainTimeout: 10s # how long open connections may finish on the old container
```

A replace on Linux:

```console
$ karga upgrade shop shop --reuse-values --set image.tag=1.27.3-alpine
[ 11s] workload web: starting shop-web-next next to shop-web
[ 13s] workload shop-web-next: waiting for shop-web-next to be ready (...)
[ 16s] workload web: switching traffic to shop-web-next
[ 16s] workload web: waiting up to 10s for connections to shop-web to finish
[ 16s] workload web: stopping shop-web
Release "shop" upgraded (revision 2).
  update workload web (replace)
```

The gateway and the release proxy only send traffic to the new container once it is healthy, and
the old one keeps its open connections until they finish or `drainTimeout` passes. If the new
container never becomes healthy it is stopped and kept for its logs, and nothing that was running
changed.

Rules to know:

- **Health checks.** `exec` runs a command inside the container, so the image needs the tool (curl,
  wget, pg_isready...). `tcp` and `http` are run by karga from the host and need nothing in the
  image.
- **Volumes.** A workload that mounts a volume read-write is always recreated: two containers
  writing the same data are unsafe.
- **Release proxy.** `karga status` shows it with the backend it routes to:

  ```
  Proxy     proxy  shop-karga-proxy  1  applied  running  0  -> karga-web-r1
  ```

  Replace workloads of one release cannot share a container port behind it; the later one (by name)
  switches by DNS and the plan says so. Adding or removing a replace workload briefly interrupts the
  proxy's open connections on that network.

## 8. Databases and other stateful services

Databases mount a volume read-write, so they upgrade with **recreate** (a short outage) and keep
their data in the volume across upgrades, rollbacks and reinstalls:

```console
$ karga install db examples/postgres --set network.port=15432 --set database.password=s3cret-pw
  create network postgres-network
  create volume postgres-data
  create secret postgres-credentials
  create workload postgres
```

Volumes have `retention: retain` by default: `uninstall` keeps them and a later install of the same
release name reuses them. `retention: delete` removes a volume when the release no longer has it.

### Back up and restore a volume

```console
$ karga volume backup db postgres-data db.tar.gz --stop
volume postgres-data of db written to db.tar.gz

$ karga volume restore db postgres-data db.tar.gz
volume postgres-data of db restored from db.tar.gz
```

- `--stop` stops the workloads that mount the volume during the copy, so the files are consistent;
  without it the copy is taken live. For databases prefer `--stop`, or the database's own dump:
  `karga exec db postgres -- pg_dump -U app app > app.sql`.
- Restore stops those workloads, writes the files and starts them again. Files in the backup
  overwrite files with the same name; other files in the volume stay.
- The backup file is mode 0600 and names its release and volume; restoring it into another one needs
  `--force`.

## 9. Secrets

There are two kinds of secret input:

- **Secret resources** become files mounted read-only in the container (e.g.
  `POSTGRES_PASSWORD_FILE: /run/secrets/postgres/password`).
- **Values marked `"writeOnly": true`** in the package schema.

What karga keeps:

| Where | Secret values |
|-------|---------------|
| Output, logs, `get values`, `get manifest`, `diff` | `<redacted>` or `(changed)` |
| State database | HMAC digests keyed by `$KARGA_HOME/state.key`, to detect changes |
| `$KARGA_HOME/releases/<release>/<revision>/` | the files mounted into the container (directory 0700) |

`karga template` shows secrets only with `--show-secrets`. Values that are not marked `writeOnly`
are stored as written, so mark every password and token in your schemas.

## 10. Resources and hardening

Defaults are written into every workload; set them in the package to change them:

```yaml
resources:
  memory: 512Mi     # also the swap limit: the container cannot swap beyond it
  cpu: 0.5
  pids: 1024        # default; 0 removes the limit
logging:
  maxSize: 10m      # default: the container log rotates at 10m, 3 files kept
  maxFiles: 3
security:
  noNewPrivileges: true          # default
  readOnlyRootFilesystem: true   # the image becomes read-only; /tmp stays writable (64m tmpfs)
  capDrop: [ALL]
  capAdd: [NET_BIND_SERVICE]
  user: "1000:1000"
```

A container that runs out of memory is killed and restarted; `karga status` shows it:

```
Workload  worker  shop-worker  3  applied  running  4  healthy, oom-killed
```

An image that needs setuid (e.g. one calling `sudo`) needs `noNewPrivileges: false`. Upgrading a
release installed by an earlier karga applies its workloads once to add these defaults; the plan
lists the new fields.

## 11. Operate and troubleshoot

| Look at | Command |
|---------|---------|
| Desired versus actual state, health, restarts | `karga status <release> [--watch]` |
| Logs | `karga logs <release> [workload...] [-f] [--tail N]`, `--proxy`, `--gateway` |
| A shell or a command inside | `karga exec <release> <workload> [-- command]` |
| What changed | `karga history <release> --diff`, `karga diff <release> <rev>` |
| The gateway | `karga gateway status` |

Tables are colored on a terminal (`--color never` or `NO_COLOR=1` turns it off); `-o json` gives
machine-readable output for every read command.

### Common errors

| Error | Meaning and fix |
|-------|-----------------|
| `values do not match values.schema.json` | A value has the wrong type or range; fix the `--set` or file. |
| `hostPort ... is taken by another process on this host` / `is used by release X` | Another program or release holds the port; pick another one. |
| `route host/ on ip:80 is already routed to X` | Another release serves that host and path. |
| `image ... was not found in its registry` | Wrong repository or tag; nothing running changed. |
| `workload X is unhealthy: http check on ... failed` | The application did not answer its health check; `karga logs` the stopped container. |
| `another operation is running on this release` | Another karga command works on the release; wait for it. |
| `a previous operation on this release was interrupted` | A karga process died mid-operation; check `karga status`, then rerun with `--force`. |
| `gateway: proxy does not route to ... : <reason>` | The gateway did not accept or reach the new backend; the reason (and Envoy's own message, if it refused the configuration) follows. |
| `routes need the gateway (Linux with a local Docker)` | Routes are Linux-only; use a host port. |
| `healthCheck type tcp needs port N published here` | On macOS karga cannot reach an unpublished port; add a `hostPort` or use `exec`. |

### Things changed behind karga's back

karga plans from what it recorded, not from what Docker has. If a container was removed by hand,
`karga status` shows it as `missing`, but an upgrade with the same values plans no changes:

```console
$ docker rm -f demo-web
$ karga status demo
Workload  web   demo-web      1         applied  missing  0
$ karga upgrade demo shop --reuse-values --dry-run
  No changes.
```

To bring it back, uninstall keeping the history (volumes are kept too) and install again:

```console
$ karga uninstall demo --keep-history
$ karga install demo shop -f my-values.yaml
Release "demo" installed (revision 2).
```

Containers of the release that karga does not know are listed under "Not tracked by karga". A
command that repairs drift in place is planned ([roadmap](docs/roadmap.md)).

## 12. Back up and move a host

A complete backup is three things:

```console
$ karga state backup ~/backup/karga.db
state written to ~/backup/karga.db
The backup does not hold ~/.local/share/karga/state.key, which keys the digests of secret values: copy it too (...)
$ cp ~/.local/share/karga/state.key ~/backup/
$ karga volume backup db postgres-data ~/backup/db-postgres-data.tar.gz --stop
```

Restore on a new host, in this order:

1. Install karga and Docker.
2. Copy `karga.db` and `state.key` into the new `$KARGA_HOME` (mode 0600) before running any karga
   command. `karga list` and `karga history` now show your releases, but their containers do not
   exist on this host yet, and karga plans from its records (see
   [section 11](#things-changed-behind-kargas-back)).
3. For each release: `karga uninstall <release> --keep-history`, then `karga install <release>
   <package> -f values.yaml`. This creates everything and keeps the history.
4. Restore the volumes with `karga volume restore`.

Without the old state (a fresh host), install each release and restore its volumes; only the history
is lost.

Without `state.key`, karga creates a new one and each release applies its Secrets once more on its
next upgrade. Keep both files safe: the database holds your values.

## 13. Write a package

```
shop/
  Package.yaml          name, version, runtime.supported: [docker]
  values.yaml           defaults, with comments
  values.schema.json    types, ranges, "writeOnly": true for secrets
  templates/web.yaml    resources, separated by ---
  templates/_helpers.tpl named templates ({{ define "url" }} ... used with include)
  NOTES.txt             shown after install and upgrade
  files/                read with .Files.Get "name"
```

Templates use Go templates with Sprig. The context is `.Values`, `.Release.Name`,
`.Release.Revision`, `.Package.Name`, `.Package.Version` and `.Files`; `include`, `tpl`,
`required`, `toYaml` and `fromYaml` are there. Functions that read the host or are not
deterministic (`env`, `now`, `randAlpha`, `genPrivateKey`, ...) fail the render, so a package and
its values always give the same result.

Check your work without touching Docker:

```console
$ karga lint shop                      # metadata, schema, render, every resource
$ karga template demo shop -f my.yaml  # what it renders
$ karga install demo shop --dry-run    # what it would do
```

A resource that does not decode names its file, document and line in the rendered output; look at
that line with `karga template`. The package format is `karga.io/v1alpha1` and may still change.

## 14. Uninstall and clean up

```console
$ karga uninstall db
Release "db" uninstalled.
$ docker volume ls --filter label=io.karga.release=db --format '{{.Name}}'
db-postgres-data
```

- Volumes are kept (reinstalling `db` reuses them); `--delete-volumes` removes them.
- `--keep-history` keeps the release's revisions in the state.
- When the last release using the gateway or a release proxy goes, they go too.

## 15. Limits to know

- Traffic features (gateway, routes, TLS, the release proxy) need Linux with a local Docker daemon.
- The gateway runs as root (with only the capabilities to bind ports) and is a single point for host
  traffic; local users can reach the loopback ports behind it.
- Images are pulled anonymously; private registries are not supported yet.
- Packages come from local directories or archives; there is no repository or dependency support.
- There are no lifecycle tasks (e.g. running migrations before an upgrade) yet.

The full list, with what is planned, is in [docs/roadmap.md](docs/roadmap.md).
