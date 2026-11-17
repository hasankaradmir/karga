# ADR-0003: Runtime abstraction

**Status:** accepted

**Decision:** The container runtime sits behind a port (`internal/ports.Runtime`) with adapters
(Docker today; a fake for tests). A contract test suite runs against every adapter.

**Reason:** The package and resource model must not depend on Docker or Podman details.
