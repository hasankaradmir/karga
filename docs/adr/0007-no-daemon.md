# ADR-0007: No mandatory daemon

**Status:** superseded by ADR-0012 (karga-server)

**Decision:** karga is a CLI. Long-lived pieces it starts (workloads, the gateway, release proxies)
are ordinary containers with a restart policy; their configuration is in files and in the state
database.

**Reason:** Lifecycle management on one host needs no separate control plane, and nothing breaks
when karga is not running.
