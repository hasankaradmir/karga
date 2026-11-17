# ADR-0005: Go templates

**Status:** accepted

**Decision:** Packages are rendered with Go `text/template` and a selected set of Sprig functions.
Non-deterministic functions (time, random, generated keys) are removed so a render is reproducible.

**Reason:** A widely known template language in the Go ecosystem; determinism makes plans and
digests meaningful.
