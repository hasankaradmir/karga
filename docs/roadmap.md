# Roadmap and known limitations

What karga does not do yet, what it does with known limits, and the open questions. The design is in
[`design.md`](design.md); decisions are in [`adr/`](adr/).

## Planned or considered

| Feature | Notes |
|---------|-------|
| Lifecycle tasks | Pre/post install and upgrade tasks (e.g. database migrations); needs an execution model. |
| Automatic certificates | ACME for TLS routes; today certificates come from a Secret. |
| `karga doctor` | One command checking the Docker version, gateway support, `KARGA_HOME` permissions, ports 80/443 and the Envoy image. |
| `karga events` | The operation history (already in the state database) as a readable timeline. |
| Drift repair (`karga reconcile`) | Plans come from the recorded state: a container removed outside karga, or a host restored from a state backup, is not recreated by an upgrade (today: `uninstall --keep-history`, then `install`). Also re-allocating a loopback upstream port taken by another process after a reboot. |
| Private registries | Image pulls are anonymous today; credentials from the Docker configuration. |
| Encrypted secret store | Values that are not `writeOnly` are stored as written. |
| Gateway: UDP and client IP for TCP | UDP host ports are published by Docker directly; TCP backends do not see the client address (HTTP routes pass `x-forwarded-for`). |
| Podman adapter | REST API or Quadlet; the runtime contract suite defines what it must do. |
| Package distribution | OCI artifacts or a repository index, signing and provenance. |
| Package dependencies | One package is one release today. |
| Runtime-specific extensions | A namespaced block for settings outside the portable model. |
| Parallel apply | Independent resources are applied one after the other today. |

## Known limitations

- **Linux-only traffic features.** The gateway, routes, TLS and the release proxy need Linux with a
  local Docker daemon. Elsewhere Docker publishes host ports, routes are refused and replace
  switches network clients by DNS (a request racing the switch can fail).
- **Gateway.** One container serves every release's host traffic; it restarts automatically but is a
  single point. Local users can reach the loopback upstream ports directly. Image or limit changes
  recreate it (a short interruption).
- **Release proxy.** Replace workloads of one release cannot share a container port behind it (the
  later one falls back to DNS switching). Adding or removing a replace workload reconnects it to its
  networks, briefly interrupting proxied connections on them.
- **Replace** needs a health check and read-only volumes; stateful workloads are recreated.
- **Health checks** `tcp`/`http` need the port published on macOS or with a remote Docker host.
- **Volume backups** go through a workload that mounts the volume; a live backup is not consistent
  for databases (use `--stop` or the database's own dump). Restore overwrites files with the same
  names and keeps the others.
- **State backup** does not include `state.key`; without it, Secrets are applied once more after a
  restore.
- **Secrets** that are not marked `writeOnly` in the schema are stored as written in the state.
- **Images** are pulled anonymously.
- **Package format** `karga.io/v1alpha1` may still change.
- **Releases** are installed from local directories or archives only.

## Risks

| Risk | Mitigation |
|------|------------|
| Access to the Docker socket is root-equivalent. | Documented; containers are never privileged; `no-new-privileges` by default. |
| The gateway runs as root (to bind low ports in the host namespace). | Every capability but `NET_BIND_SERVICE` and `DAC_OVERRIDE` is dropped, `no-new-privileges`, digest-pinned image. |
| A failed recreate leaves the workload down. | Pre-flight checks and image pulls happen before anything stops; the plan says recreate; replace where possible; `rollback`. |
| An interrupted operation leaves a release locked or half applied. | Operations record PID and host; `--force` aborts a dead one; interrupted replaces are repaired before the next plan. |
| A Podman adapter may not fit the Docker-shaped capability model. | The contract suite and capabilities are runtime-agnostic. |

## Open questions

- How lifecycle tasks run: as one-off workloads, inside an existing container, or on the host.
- Whether drift repair becomes a command or a daemon (karga has no daemon today, ADR-0007).
- Podman through its API or through Quadlet units.
- How packages are distributed and signed.
