-- Device Pairing relays: a new device publishes a one-time public key here,
-- an already-unlocked device on the same account posts the vault key
-- encrypted to it, and the new device collects it once.
--
-- `ciphertext` is the only thing of value in this table and the server cannot
-- open it: it is sealed to `public_key`, whose private half never leaves the
-- new device. Collecting deletes the row, so a relayed key is not at rest for
-- longer than the collecting poll takes, and the window is minutes either way.
create table if not exists device_pairings (
    id text primary key,
    user_id uuid not null references users (id) on delete cascade,
    public_key text not null,
    device_name text not null,
    platform text not null,
    ciphertext text,
    expires_at timestamptz not null
);

create index if not exists idx_device_pairings_expires_at on device_pairings (expires_at);
