# Changelog

All notable changes are listed here, newest first ([Keep a Changelog](https://keepachangelog.com),
[Semantic Versioning](https://semver.org)).

## [Unreleased]

First release.

### Repositories
- karga is split into three repositories with their history: the karga CLI (this one),
  karga-server (`github.com/hasankaradmir/karga-server`, the server, its install script and the
  design documents) and karga-sdk (`github.com/hasankaradmir/karga-sdk`, the package engine, the
  API and the example packages). Each installs with a public script, without credentials.
- The CLI's tests run against a fake karga-server; the server's tests cover what it does.

### Changed
- `karga state backup` is gone: karga-server keeps its state in files, and a backup is a copy of
  its `$KARGA_HOME` (USAGE.md, "Back up and move a host").
- The module is `github.com/hasankaradmir/karga-cli` (the repository is karga-cli); the binary is
  still `karga`.
- API 2.0: works with any karga-server of the same major API version. A request an older server
  does not understand (an unknown field or endpoint) exits with 3 and says to upgrade it.
- `-o json` and `-o yaml` are the API's views and stable within an API major version.
- Managing a server from another machine is `ssh host karga …`; forwarding its socket is not supported.
- `-o text|json|yaml` on every command (`table` is an older name for `text`); `status --watch` and
  `events -f` stream one JSON document per line, or YAML documents.
- Exit codes tell scripts why karga failed: 2 usage, 3 server unavailable or incompatible, 4 not
  found, 5 conflict, 6 `karga status` not healthy (see USAGE.md).
- `uninstall`, `volume restore`, `gateway set listen` (beyond loopback), `gateway listener remove`
  and `upgrade`/`rollback --delete-volumes` ask first; in scripts they need `--yes`.
- `--request-timeout` (default 30s) bounds commands that only ask karga-server something.
- `uninstall --dry-run`. `get values|manifest|notes <release> [revision]` take the revision as an
  argument instead of `--revision`. `volume restore --from-other-volume` replaces `--force`.
- `--timeout` and `--history-max` are sent only when given: karga-server's defaults apply.
- `karga template` masks writeOnly values wherever they occur, and takes `-o json|yaml`.
- `history --diff` asks karga-server for the differences.
- `karga version` says why it cannot reach the server (not running, permission denied) and exits
  with 3 when the CLI and the server do not work together.

### Release
- Built with Go 1.27.4 (net/http fix GO-2026-6859); CI runs govulncheck 1.10.0.
- Releases carry an SBOM per archive and a keyless (Sigstore) signature of `checksums.txt`;
  `install.sh` verifies it when `cosign` is installed (`KARGA_SKIP_SIGNATURE=1` skips it).
- The release workflow runs the tests and the linter before it publishes; CI runs `govulncheck`, and
  every GitHub Action is pinned by commit.

### Fixed
- `exec -t` without `-i` always failed.
- `events` said newest first but printed oldest first; long messages are cut at a character.
- Status colors covered states that do not exist and missed upgrading, uninstalling and aborted;
  only state columns are colored now.
- A second Ctrl-C ends karga at once.
- Released binaries report `v0.1.0` like local builds.

### Breaking (compared with earlier builds)
- Workloads no longer expose ports: `hostPort`, `hostIP` and `routes` are gone from the Workload
  (a clear error points to Services). Ports are named, and a `kind: Service` gives a workload a name
  inside its release and exposes it on gateway listeners.
- The gateway's listen addresses and listeners belong to the host's administrator: `karga gateway
  listener add|remove|list`, `karga gateway set listen` (default `127.0.0.1`). Whoever used
  `hostIP: 0.0.0.0` sets `karga gateway set listen 0.0.0.0`.
- karga-server owns the state and the runtime; the karga CLI is its client on
  `/run/karga/karga.sock` and stops with `karga-server is not running` without it (`create`,
  `lint`, `template`, `show` work alone). The service runs as the `karga` user with its state in
  `/var/lib/karga`; karga-server's install script sets up the service. The CLI's `--home` is gone (`--socket` instead).
- `dependsOn` on a workload: it starts only once the Services it names are ready behind the release
  proxy; a failed dependency keeps it from starting.
- karga-server repairs releases on its own (Docker events and every 30 seconds): removed or
  hand-changed containers are recreated, stopped ones started, unhealthy ones restarted, without a
  new revision. `karga reconcile`, `karga pause|resume` and `karga events`; `karga status` shows the
  last repair and what it gave up on.
- Operations go on when the CLI is interrupted (Ctrl-C, a lost connection); the CLI says how to
  follow them. A karga-server that is killed mid-operation finishes it when it starts again
  (`resume` in `karga events`).
- The CLI and karga-server check that they speak the same API version (`karga version` shows
  it). Operations record the user of the CLI that asked for them (`USER` in `karga events`).
- Linux only: without karga's gateway and release proxy (macOS, a remote Docker host) a plan with
  Services is refused. Upgrading an installed release with its new package recreates its workloads
  once.

### Packages and releases
- Packages: Go templates with Sprig, `values.yaml` and JSON Schema (writeOnly values are never
  stored), `karga create` to start one, `lint`, `template`, notes (`NOTES.txt`).
- Resources: Workload, Service, Volume, Network, Config, Secret.
- Releases: install, upgrade (`--reuse-values`, `--install`), rollback, uninstall; immutable
  revisions in karga-server's state; `history`, `diff`, `status --watch`, `get values|manifest|notes`.
- Plans as a summary (`--verbose` for fields) and live progress while applying.
- An upgrade recreates containers, networks and volumes removed outside karga; an install that
  fails before creating anything leaves no release behind.
- Data in an image's `VOLUME` path that no Volume covers survives upgrades (the anonymous volume
  moves to the new container), with a warning to mount a Volume there.
- `stopTimeout` on a workload: how long it gets to shut down before it is killed (default 10s);
  the Postgres example uses 60s.
- An upgrade or rollback that would delete a `retention: delete` volume stops unless given
  `--delete-volumes`; the plan marks what happens to a removed volume's data.

### Services and upgrades without downtime
- A release proxy (Envoy) serves the release's Services; `updateStrategy: replace` starts the new
  container next to the old one and switches the proxy over, so no client loses a request.
- A host gateway (Envoy) serves the administrator's listeners: tcp, and HTTP(S) by host and path,
  with TLS from a Secret and redirects from port 80.
- `tcp`/`http` health checks need no tool in the image.

### Operations
- `karga logs`, `karga exec`, `karga gateway status`,
  `karga volume backup|restore`, shell completion.
- Memory (without extra swap), CPU and process limits; OOM kills and restarts in `status`;
  container log rotation; `no-new-privileges` by default and a `security` block.
