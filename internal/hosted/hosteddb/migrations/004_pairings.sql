-- The pairing relay: a new device's one-time public key, and the vault key an
-- unlocked device of the same account sealed to it. Collecting the ciphertext
-- deletes the row; the hourly sweep deletes the ones nobody collected.
create table pairings (
    id uuid primary key,
    user_id uuid not null references users (id) on delete cascade,
    public_key text not null,
    device_name text not null,
    platform text not null,
    ciphertext text,
    created_at timestamptz not null default now(),
    expires_at timestamptz not null
);

create index idx_pairings_expires_at on pairings (expires_at);
