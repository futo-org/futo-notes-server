-- The second vault-key envelope, wrapped under the Recovery Key. See the
-- Postgres migration of the same name for why it gets its own columns.
alter table collections add column recovery_key_salt TEXT;
alter table collections add column recovery_key_kdf TEXT;
alter table collections add column recovery_encrypted_vault_key TEXT;
