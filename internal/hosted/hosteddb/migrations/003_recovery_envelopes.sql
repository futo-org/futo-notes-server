-- The vault key wrapped a second time, under a recovery key, beside the
-- password envelope in collections. A key PUT writes all three columns or
-- deletes the row, in the key's transaction; the columns stay nullable so a
-- row missing any of them can exist and be read back as no envelope.
create table collection_recovery_envelopes (
    collection_id uuid primary key references collections (id) on delete cascade,
    salt text,
    kdf jsonb,
    encrypted_vault_key text
);
