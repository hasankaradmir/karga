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
- [6. Services and the gateway](#6-services-and-the-gateway)
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
| **Resources** | What templates render to: `Workload` (a container), `Service`, `Volume`, `Network`, `Config` (files), `Secret`. |
| **Release** | A package installed under a name, e.g. `shop`. Its containers are named `<release>-<name>` (`shop-web`). |
| **Revision** | Each install, upgrade and rollback is a new, immutable revision of the release. |
| **Plan** | What a command will change: created (`+`), updated (`~`) and deleted (`-`) resources, and for workloads how (recreate or replace). |
| **Service** | The only way to reach a workload: a name inside the release (`web:80`), and optionally an exposure on a gateway listener. A workload alone publishes nothing. |
| **Release proxy** | One Envoy per release with Services (`<release>-karga-proxy`). It answers to the Service names and forwards to the current container, so upgrades switch without losing requests. |
| **Gateway** | One Envoy per host (`karga-gateway`), the only way in from outside. The host's administrator defines where it listens and its **listeners** (a name, a port, a protocol); packages can only expose Services on them. |

Where things live:

- Docker runs the containers. karga is a CLI: when it is not running, nothing of karga runs except
  your containers, the gateway and the release proxies, all restarted by Docker.
- `$KARGA_HOME` (default `~/.local/share/karga`) holds the state database `karga.db`, the key
  `state.key`, rendered Config and Secret files, the gateway settings `gateway.json` and the Envoy
  configuration. Keep it private; it is created with mode 0700.

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

karga needs Linux with a local Docker daemon: its Envoys share files, unix sockets and the host
network with it. karga uses `DOCKER_HOST`, then `DOCKER_CONTEXT`, then the current Docker context.
On macOS or with a remote Docker host, `lint` and `template` work, but a plan with Services is
refused (`services ... need karga's gateway and release proxy (Linux with a local Docker)`).

### Shell completion

```console
$ karga completion zsh > "${fpath[1]}/_karga"     # bash: karga completion bash > /etc/bash_completion.d/karga
```

Completion offers release, workload, volume and revision names from your state.

## 3. Your first release

Create a package to start from, check it and look at what it would do:

```console
$ karga create shop
Created package shop. Try: karga lint shop && karga install <release> shop

$ karga lint shop
1 package(s) linted, 0 failed

$ karga install demo shop --dry-run
Deployment Plan

Release:  demo
Revision: 1

  + NETWORK net
  + WORKLOAD web
  + SERVICE web
  + PROXY proxy

4 to create, 0 to update, 0 to delete, 0 unchanged.
```

The package has a workload `web` (nginx) and a Service `web` for it; karga adds the release proxy
that serves the Service. `karga template demo shop` shows the rendered resources. Install it.
Progress goes to stderr; the package's notes are printed at the end:

```console
$ karga install demo shop
[  0s] workload web: image nginx:1.27-alpine (if-not-present)
[  0s] workload web: creating demo-web
[  2s] workload demo-web: waiting for demo-web to be ready (...)
[  5s] proxy proxy: create done
Release "demo" installed (revision 1).
  create network net
  create workload web
  create service web
  create proxy proxy

NOTES:
demo (revision 1) runs nginx:1.27-alpine.

Open http://web:80/ from the release network (not exposed on the host)
Logs:   karga logs demo -f
Shell:  karga exec demo web
```

Nothing is reachable from the host yet: the workload publishes no port. Inside the release, the
Service name answers:

```console
$ karga exec demo web -- wget -qO- http://web:80/
<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
...

$ karga status demo
Release:  demo
Status:   deployed
Revision: 1

KIND      NAME   RUNTIME NAME      REVISION  DESIRED  ACTUAL   RESTARTS  HEALTH/BACKEND
Network   net    demo-net          1         applied  present
Proxy     proxy  demo-karga-proxy  1         applied  running  0         -> karga-web-r1
Service   web    demo-karga-proxy  1         applied  routed             -> karga-web-r1
Workload  web    demo-web          1         applied  running  0         healthy

$ karga list
NAME  REVISION  STATUS    PACKAGE  VERSION  UPDATED
demo  1         deployed  shop     0.1.0    2026-11-17 17:59:08
```

[Section 6](#6-services-and-the-gateway) exposes it on the host. `karga exec demo web` without a
command opens an interactive shell; `-it` gives any command a terminal. `karga logs demo -f`
follows the logs; `karga status demo --watch` refreshes every two seconds.

Installing the same package with the same values again does nothing: `Release "demo" is already
installed with the same package and values`.

## 4. Configure with values

See a package's defaults with `karga show values <package>`. Override them, later sources winning:

```console
$ karga install db examples/postgres -f prod.yaml --set service.listener=pg --set database.password=s3cret-pw
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
service:
  listener: pg
```

Values the schema marks `"writeOnly": true` (passwords, tokens) are used to render but stored and
shown as `<redacted>`; `--reuse-values` cannot bring them back, so pass them again on each upgrade
(a values file kept outside git works well).

## 5. Upgrade safely

### Read the plan first

```console
$ karga upgrade demo shop --set image.tag=1.27.3-alpine --dry-run
Revision: 1 -> 2

  ~ WORKLOAD web (replace)

0 to create, 1 to update, 0 to delete, 3 unchanged. --verbose shows the changed fields.
```

`--verbose` lists every changed field. The text in parentheses is the update strategy and, when a
workload asked for `replace` but cannot have it, why:

- **recreate**: the old container stops, then the new one starts. A short outage.
- **replace**: the new container starts next to the old one and takes over once healthy. Needs a
  health check and read-only volumes (see [section 7](#7-upgrades-without-downtime)).

`--strategy recreate|replace` overrides the packages' choice for one upgrade or rollback.

### Upgrade

```console
$ karga upgrade demo shop --set image.tag=1.27.3-alpine
[  0s] workload web: image nginx:1.27.3-alpine (if-not-present)
[  0s] workload web: starting demo-web-next next to demo-web
[  5s] workload web: switching traffic to demo-web-next
[  5s] workload web: waiting up to 10s for connections to demo-web to finish
[  5s] workload web: stopping demo-web
Release "demo" upgraded (revision 2).
  update workload web (replace)
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
  ~ WORKLOAD web (replace)
```

`karga rollback demo` returns to the last good revision before the current one; `karga rollback
demo 1` to a given one. A rollback is a new revision. It re-applies configuration; it cannot undo
changes your application made to its data (database migrations, for example). A rollback that
would delete a volume's data needs `--delete-volumes` (see
[section 8](#8-databases-and-other-stateful-services)).

## 6. Services and the gateway

A workload publishes nothing. Two things make it reachable:

- A **Service** gives it a name inside the release, on the networks the workload joins:

  ```yaml
  kind: Service
  metadata: {name: web}           # other workloads connect to web:80
  spec:
    workload: web
    ports:
      - {name: http, port: 80, targetPort: http}   # targetPort: the workload's port name or number
  ```

  A Service name resolves only on its release's networks: two releases can both have a Service
  `web` on port 80 without seeing each other. One release proxy serves all Services of a release, so
  a port number belongs to one Service of the release.
- An **exposure** puts a Service port on a listener of the host gateway. Listeners belong to the
  host's administrator, not to packages.

### Listeners and listen addresses (the administrator)

```console
$ karga gateway listener list
NAME       PORT  PROTOCOL  SERVICES
web        80    http
websecure  443   https

$ karga gateway listener add pg --port 15432 --protocol tcp
listener pg added (tcp on port 15432)
```

- `http` and `https` listeners are shared: each exposure selects a Service by host name and path
  prefix. A `tcp` listener belongs to one Service.
- Every listener binds the addresses of `karga gateway set listen`, by default `127.0.0.1` only:
  nothing is reachable from other machines until you say so.

  ```console
  $ karga gateway set listen 0.0.0.0          # every interface; or a list: 10.0.0.5,127.0.0.1
  gateway listens on 0.0.0.0
  ```

  Then make sure DNS points the host names at the server and the firewall lets the ports through.
- A listener a Service uses cannot be removed:

  ```console
  $ karga gateway listener remove pg
  Error: gateway listener "pg" is used by db/postgres; remove those exposures first
  ```

### Expose a Service (the package)

```yaml
spec:
  expose:
    - {port: http, listener: web, host: shop.local}        # path defaults to /
    - {port: postgres, listener: pg}                         # tcp: no host
```

The scaffold from `karga create` exposes its Service with `--set expose.host=...`; the examples with
`--set service.listener=...`:

```console
$ karga upgrade demo shop --reuse-values --set expose.host=shop.local --dry-run
  ~ SERVICE web
  ~ PROXY proxy (recreate: image, limits or exposed ports changed: brief interruption)

$ karga upgrade demo shop --reuse-values --set expose.host=shop.local
Release "demo" upgraded (revision 3).
  update service web
  update proxy proxy (recreate: image, limits or exposed ports changed: brief interruption)

$ curl -s -o /dev/null -w "%{http_code}\n" -H "Host: shop.local" http://127.0.0.1/
200

$ karga gateway status
Gateway:    running, ready (Envoy 1.36.13)
Image:      envoyproxy/envoy:distroless-v1.36.13@sha256:...
Listen:     127.0.0.1
Access log: off

LISTENER  ROUTE        RELEASE  SERVICE  UPSTREAM
web:80    shop.local/  demo     web:80   127.0.0.1:34359
```

Exposing a new port recreates the release proxy (it publishes the port on a loopback upstream for
the gateway); changing a host or path does not. An exposure the gateway cannot serve is refused
before anything changes:

```console
$ karga install other shop --set expose.host=shop.local
Error: Service "web": expose http on listener web: shop.local/ is already routed to demo/web: port already in use
revision 1 failed; nothing was installed. Fix the problem and run 'karga install' again

$ karga install db examples/postgres --set database.password=s3cret-pw --set service.listener=pg
Error: Service "postgres": expose postgres on listener pg: no such gateway listener; the host's administrator adds it with 'karga gateway listener add pg --port <port> --protocol tcp|http|https'
```

Applications behind http and https listeners receive the client address in `x-forwarded-for`.

### TLS

Expose on an https listener (`websecure` on a new host) with a Secret that holds the PEM certificate
chain as `tls.crt` and the key as `tls.key`. Plain HTTP on the port-80 http listener redirects to it,
unless that listener serves the host itself:

```yaml
kind: Secret
metadata: {name: blog-cert}
spec:
  values:
    tls.crt: {{ .Values.tls.cert | quote }}     # from a values file kept outside git
    tls.key: {{ .Values.tls.key | quote }}
---
# in the Service
  expose:
    - {port: http, listener: websecure, host: blog.local, tls: {secret: blog-cert}}
```

```console
$ curl -s -o /dev/null -w "%{http_code}\n" --resolve blog.local:443:127.0.0.1 --cacert cert.pem https://blog.local/
200
$ curl -s -o /dev/null -w "%{http_code} %{redirect_url}\n" -H "Host: blog.local" http://127.0.0.1/
301 https://blog.local/
```

Mark the certificate values `writeOnly` in the schema. To renew a certificate, upgrade with the new
one; the gateway serves it after the upgrade. There is no automatic certificate (ACME) yet.

### Gateway access log

```console
$ karga gateway set access-log on
$ karga logs --gateway -f
[2026-11-17T04:58:08.117Z] "GET /logged HTTP/1.1" 404 - 0 124 2 1 "192.0.2.57" "Go-http-client/1.1" ...
```

It is off by default to keep the log small; turning it on or off does not interrupt traffic.

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

A replace:

```console
$ karga upgrade demo shop --set image.tag=1.27.3-alpine
[  0s] workload web: starting demo-web-next next to demo-web
[  2s] workload demo-web-next: waiting for demo-web-next to be ready (...)
[  5s] workload web: switching traffic to demo-web-next
[  5s] workload web: waiting up to 10s for connections to demo-web to finish
[  5s] workload web: stopping demo-web
Release "demo" upgraded (revision 2).
  update workload web (replace)
```

Every client reaches the workload through its Service, that is through the release proxy, from the
release's networks and through the gateway alike. The proxy sends traffic to the new container only
once it is healthy, and the old one keeps its open connections until they finish or `drainTimeout`
passes. If the new
container never becomes healthy it is stopped and kept for its logs, and nothing that was running
changed.

Rules to know:

- **Health checks.** `exec` runs a command inside the container, so the image needs the tool (curl,
  wget, pg_isready...). `tcp` and `http` are run by karga from the host and need nothing in the
  image.
- **Volumes.** A workload that mounts a volume read-write, or keeps an image VOLUME in an anonymous
  volume (see [section 8](#what-happens-to-data-on-an-upgrade)), is always recreated: two
  containers writing the same data are unsafe.
- **Release proxy.** `karga status` shows it, and each Service, with the backend it routes to:

  ```
  Proxy     proxy  demo-karga-proxy  1  applied  running  0  -> karga-web-r1
  Service   web    demo-karga-proxy  1  applied  routed      -> karga-web-r1
  ```

  Adding or removing a Service briefly interrupts the proxy's open connections on that network.
- A workload no Service targets receives no traffic; a replace just starts the new container and
  stops the old one once it is healthy.

## 8. Databases and other stateful services

Databases mount a volume read-write, so they upgrade with **recreate** (a short outage) and keep
their data in the volume across upgrades, rollbacks and reinstalls:

```console
$ karga install db examples/postgres --set database.password=s3cret-pw --set service.listener=pg
Release "db" installed (revision 1).
  create network postgres-network
  create volume postgres-data
  create secret postgres-credentials
  create workload postgres
  create service postgres
  create proxy proxy

$ docker port db-postgres                  # the database publishes nothing
$ docker port db-karga-proxy               # the proxy: only a loopback upstream for the gateway
5432/tcp -> 127.0.0.1:37115
```

Other workloads of the release connect to `postgres:5432`; outside clients to the `pg` listener
(`127.0.0.1:15432` until `karga gateway set listen` says otherwise).

Volumes have `retention: retain` by default: `uninstall` keeps them and a later install of the same
release name reuses them, and a volume removed from the package stays in Docker. `retention: delete`
removes the volume when the release no longer has it, after a renamed volume, a removed one, or a
rollback to a revision without it. Such an upgrade or rollback stops before changing anything
unless you confirm with `--delete-volumes`:

```console
$ karga rollback s 1 --dry-run
Warning: this rollback deletes volume cache and its data; it needs --delete-volumes
  - VOLUME cache (its data is deleted)

$ karga rollback s 1
Error: this rollback deletes volume cache and its data (retention: delete); nothing was changed. Rerun with --delete-volumes to go ahead

$ karga rollback s 1 --delete-volumes
Release "s" rolled back (revision 3).
  delete volume cache (its data is deleted)
```

### What happens to data on an upgrade

- A Volume is named `<release>-<volume>` in Docker (`db-postgres-data`), whatever the revision.
  The new container mounts the same volume, so upgrades, rollbacks and reinstalls under the same
  release name see the same data. Changing a Volume's settings is refused by the plan.
- The old container is stopped before the new one starts: it gets SIGTERM (or the image's stop
  signal) and `stopTimeout` (default 10s) to shut down before it is killed. Databases that write a
  checkpoint on shutdown need more; `examples/postgres` sets `stopTimeout: 60s`. The same time
  applies to `uninstall`, `volume backup --stop` and `docker stop`.
- Data written anywhere else in the container is lost when it is recreated, unless the image
  declares that path as a `VOLUME`. Docker then keeps it in an anonymous volume, which karga moves
  to the new container on every upgrade. It is not a karga Volume: `uninstall` deletes it and
  `volume backup` cannot reach it, so karga warns:

  ```console
  $ karga install cache ./cache
  Warning: workload redis keeps image VOLUME /data in an anonymous volume: upgrades keep it, but uninstall deletes it and it is not in volume backups. Mount a Volume there.
  ```

  Mount a Volume on that path to keep the data like any other.
- Upgrades cannot undo what the new version does to the data (a database migration, a new file
  format); back up before upgrading across such versions.

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
| `no such gateway listener` | The Service exposes on a listener the host does not have; the administrator adds it (`karga gateway listener add`) or the package uses another one. |
| `... is already routed to R/S` / `the listener is used by R/S` | Another release's Service holds that host and path, or that tcp listener. |
| `...:N is taken by another process on this host` | Another program holds a listener's port on a listen address. |
| `is a tcp listener and takes no host` / `needs a host` / `needs tls.secret` | The exposure does not fit the listener's protocol. |
| `image ... was not found in its registry` | Wrong repository or tag; nothing running changed. |
| `workload X is unhealthy: http check on ... failed` | The application did not answer its health check; `karga logs` the stopped container. |
| `another operation is running on this release` | Another karga command works on the release; wait for it. |
| `a previous operation on this release was interrupted` | A karga process died mid-operation; check `karga status`, then rerun with `--force`. |
| `gateway: proxy does not route to ... : <reason>` | The gateway did not accept or reach the new backend; the reason (and Envoy's own message, if it refused the configuration) follows. |
| `services ... need karga's gateway and release proxy (Linux with a local Docker)` | karga runs on Linux with a local Docker daemon. |
| `a Workload no longer exposes ports; a Service does` | The package uses the removed `hostPort`, `hostIP` or `routes`; move them to a Service. |

### Things changed behind karga's back

If a container, network or volume of a release was removed outside karga, `karga status` shows it
as `missing` and the next upgrade creates it again:

```console
$ docker rm -f demo-web
$ karga status demo
Workload  web   demo-web      1         applied  missing  0
$ karga upgrade demo shop --reuse-values --dry-run
  + WORKLOAD web

1 to create, 0 to update, 0 to delete, 1 unchanged.
```

Other changes made behind karga's back (a container recreated by hand with other settings) are not
detected; the plan compares with what karga applied. Containers of the release that karga does not
know are listed under "Not tracked by karga".

## 12. Back up and move a host

A complete backup is the state, its key, the gateway settings and the volumes:

```console
$ karga state backup ~/backup/karga.db
state written to ~/backup/karga.db
The backup does not hold ~/.local/share/karga/state.key, which keys the digests of secret values: copy it too (...)
$ cp ~/.local/share/karga/state.key ~/.local/share/karga/gateway.json ~/backup/
$ karga volume backup db postgres-data ~/backup/db-postgres-data.tar.gz --stop
```

Restore on a new host, in this order:

1. Install karga and Docker.
2. Copy `karga.db` and `state.key` into the new `$KARGA_HOME` (mode 0600) before running any karga
   command. `karga list` and `karga history` now show your releases.
3. For each release, upgrade from its package; the plan creates everything missing on this host:

   ```console
   $ karga upgrade demo shop -f values.yaml
   Release "demo" upgraded (revision 3).
     create network net
     create workload web
   ```

   Copy `gateway.json` too, so the new host has the same listeners and listen addresses.

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
  templates/web.yaml    resources, separated by --- (a Workload, its Service, ...)
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

Give every workload other workloads or users talk to a Service, and expose it through a values
setting (a listener name, a host) rather than a fixed one: the listeners are the host's, and the
person installing the package knows them (`karga gateway listener list`).

## 14. Uninstall and clean up

```console
$ karga uninstall db
Release "db" uninstalled.
$ docker volume ls --filter label=io.karga.release=db --format '{{.Name}}'
db-postgres-data
```

- Volumes are kept (reinstalling `db` reuses them); `--delete-volumes` removes them.
- `--keep-history` keeps the release's revisions in the state.
- The release proxy goes with the release; the gateway goes when nothing is exposed anymore. The
  gateway's listeners and listen addresses stay: they are the host's settings.

## 15. Limits to know

- karga needs Linux with a local Docker daemon.
- Services carry TCP only; a release's Service ports must be distinct numbers.
- Workloads on the same network of a release can reach each other by IP (and Docker resolves
  container names); there is no network policy.
- The gateway runs as root (with only the capabilities to bind ports) and is a single point for
  outside traffic; local users can reach the loopback ports behind it.
- Images are pulled anonymously; private registries are not supported yet.
- Packages come from local directories or archives; there is no repository or dependency support.
- There are no lifecycle tasks (e.g. running migrations before an upgrade) yet.

The full list, with what is planned, is in [docs/roadmap.md](docs/roadmap.md).
