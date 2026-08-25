alter table collections
    add column key_salt text,
    add column key_kdf jsonb,
    add column encrypted_vault_key text,
    add column key_updated_at timestamptz;
