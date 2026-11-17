-- A paused release is left alone by karga-server's reconciler (karga pause / karga resume).
ALTER TABLE releases ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
