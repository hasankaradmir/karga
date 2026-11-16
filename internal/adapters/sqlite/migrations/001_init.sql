CREATE TABLE releases (
    name             TEXT PRIMARY KEY,
    package_name     TEXT NOT NULL,
    package_version  TEXT NOT NULL,
    current_revision INTEGER NOT NULL,
    status           TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE TABLE revisions (
    release         TEXT NOT NULL REFERENCES releases(name) ON DELETE CASCADE,
    revision        INTEGER NOT NULL,
    package_name    TEXT NOT NULL,
    package_version TEXT NOT NULL,
    package_digest  TEXT NOT NULL,
    values_digest   TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    values_json     TEXT NOT NULL,
    values_all_json TEXT NOT NULL,
    manifest_json   TEXT NOT NULL,
    status          TEXT NOT NULL,
    description     TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (release, revision)
);

-- What is applied on the runtime right now, one row per logical resource (tech design §14.2).
CREATE TABLE resources (
    release      TEXT NOT NULL REFERENCES releases(name) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    name         TEXT NOT NULL,
    revision     INTEGER NOT NULL,
    runtime_name TEXT NOT NULL,
    spec_json    TEXT NOT NULL,
    spec_digest  TEXT NOT NULL,
    PRIMARY KEY (release, kind, name)
);

-- No foreign key: an install operation starts before its release row exists.
CREATE TABLE operations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    release     TEXT NOT NULL,
    type        TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    status      TEXT NOT NULL,
    pid         INTEGER NOT NULL,
    hostname    TEXT NOT NULL,
    steps_json  TEXT NOT NULL,
    error       TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL
);

CREATE INDEX operations_running ON operations (release, status);
