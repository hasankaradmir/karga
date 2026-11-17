# ADR-0001: Single-node architecture

**Status:** accepted

**Decision:** karga runs on one host. It does not schedule across machines.

**Reason:** Distributed orchestration, scheduling and cluster management are out of scope; the goal
is package management for containers on a single host with as few moving parts as possible.
