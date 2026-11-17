# ADR-0009: Envoy proxy for the replace strategy (Linux)

**Status:** superseded by ADR-0010 (the per-workload proxy and `updateStrategy.proxy` were removed)

**Decision:** To switch traffic to a new container without losing requests, karga ran an Envoy in
front of a workload. Its configuration is file based (bootstrap, `lds.json`, `cds.json` with a
watched directory) and replaced atomically with a rename. Backends are named by a per-revision DNS
alias rather than an IP. The drain signal is the cluster's `upstream_cx_active` minus the
`cx_active` of its current hosts. Linux only.

**Reason:** A spike on Linux measured config changes applied in 5–122 ms, 0 failed requests over
20 switches from the host (2419 requests) and from the network (1493), a 30 s connection surviving
a switch, and correct routing after backend and proxy restarts. State in files survives restarts,
unlike a runtime API (HAProxy).

**Constraint:** Envoy does not keep the old cluster when the new alias does not resolve; the
configuration is written only once the new container is healthy and on the network, and the old
alias is written back if the proxy does not report the new host healthy.
