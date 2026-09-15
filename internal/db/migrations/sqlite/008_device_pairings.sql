-- See the Postgres migration of the same name.
create table device_pairings (
    id TEXT primary key,
    user_id TEXT not null references users (id) on delete cascade,
    public_key TEXT not null,
    device_name TEXT not null,
    platform TEXT not null,
    ciphertext TEXT,
    expires_at TEXT not null
);
create index idx_device_pairings_expires_at on device_pairings (expires_at);
