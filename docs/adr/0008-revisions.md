# ADR-0008: Revision-based release management

**Status:** accepted

**Decision:** Every install, upgrade and rollback creates a new immutable revision (package digest,
values with writeOnly fields redacted, rendered manifest with secret values replaced by keyed
digests, notes). Rollback re-applies a stored revision as a new one.

**Reason:** Reliable history, diff (`karga diff`) and rollback.
