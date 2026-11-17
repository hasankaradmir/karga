-- Who started an operation: the user of the CLI that asked for it, or karga-server itself.
ALTER TABLE operations ADD COLUMN actor TEXT NOT NULL DEFAULT '';
