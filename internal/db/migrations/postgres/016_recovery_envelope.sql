-- The vault key is wrapped twice (ADR 0005): under the Vault Password, and
-- under the Recovery Key that is the way back in when no device is still
-- unlocked. The second envelope gets its own columns rather than sharing the
-- first's, so no column ever holds two shapes that have to be told apart by a
-- value somewhere else.
--
-- Nullable, and null together: a vault created by a client that knows only the
-- Vault Password has no recovery door, and password-mode self-hosted leaves
-- all three empty forever.
alter table collections
    add column recovery_key_salt text,
    add column recovery_key_kdf jsonb,
    add column recovery_encrypted_vault_key text;
