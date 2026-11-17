-- The rendered NOTES.txt of a revision, shown after install and upgrade and by `karga get notes`.
ALTER TABLE revisions ADD COLUMN notes TEXT NOT NULL DEFAULT '';
