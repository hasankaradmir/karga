# Changelog

All notable changes are listed here, newest first ([Keep a Changelog](https://keepachangelog.com),
[Semantic Versioning](https://semver.org)).

## [Unreleased]

First release.

### Packages and releases
- Packages: Go templates with Sprig, `values.yaml` and JSON Schema (writeOnly values are never
  stored), `karga create` to start one, `lint`, `template`, notes (`NOTES.txt`).
- Resources: Workload, Volume, Network, Config, Secret.
- Releases: install, upgrade (`--reuse-values`, `--install`), rollback, uninstall; immutable
  revisions in SQLite; `history`, `diff`, `status --watch`, `get values|manifest|notes`.
- Plans as a summary (`--verbose` for fields) and live progress while applying.
- An upgrade recreates containers, networks and volumes removed outside karga; an install that
  fails before creating anything leaves no release behind.

### Upgrades without downtime (Linux)
- `updateStrategy: replace` starts the new container next to the old one and switches over.
- A host gateway (Envoy) serves tcp host ports and HTTP(S) routes by host and path, with TLS from
  a Secret and redirects from port 80; a release proxy switches network clients.
- `tcp`/`http` health checks need no tool in the image.

### Operations
- `karga logs`, `karga exec`, `karga gateway status`, `karga state backup`,
  `karga volume backup|restore`, shell completion.
- Memory (without extra swap), CPU and process limits; OOM kills and restarts in `status`;
  container log rotation; `no-new-privileges` by default and a `security` block.
