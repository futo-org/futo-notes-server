create table users (
    id TEXT primary key,
    sub TEXT not null unique,
    name TEXT not null,
    email TEXT not null unique
);

create table sessions (
    id TEXT primary key,
    user_id TEXT not null references users (id) on delete cascade,
    access_token_hash BLOB not null unique,
    expires_at TEXT not null
);
create index idx_sessions_token_hash on sessions (access_token_hash);

create table collections (
    id TEXT primary key,
    user_id TEXT not null references users (id) on delete cascade,
    created_at TEXT not null,
    current_version INTEGER not null default 0,
    key_salt TEXT,
    key_kdf TEXT,
    encrypted_vault_key TEXT,
    key_updated_at TEXT
);
create index idx_collections_user on collections (user_id);

create table objects (
    id TEXT primary key,
    collection_id TEXT not null references collections (id) on delete cascade,
    user_id TEXT not null references users (id) on delete cascade,
    version INTEGER not null default 1,
    deleted INTEGER not null default 0,
    blob_key TEXT,
    size_bytes INTEGER,
    created_at TEXT not null,
    updated_at TEXT not null,
    change_seq INTEGER not null default 0
);
create index idx_objects_collection on objects (collection_id, user_id);
create index idx_objects_collection_change_seq on objects (collection_id, user_id, change_seq);

create table server_config (
    key TEXT primary key,
    value TEXT not null
);

create table blob_ledger (
    blob_key TEXT primary key,
    user_id TEXT not null references users (id) on delete cascade,
    size_bytes INTEGER not null,
    state TEXT not null,
    collection_id TEXT,
    object_id TEXT references objects (id) on delete set null,
    object_version INTEGER,
    created_at TEXT not null,
    state_changed_at TEXT not null,
    constraint blob_ledger_state_check check (
        state in ('staged', 'claimed', 'retained', 'purgeable', 'legacy_shared')
    )
);
create index idx_blob_ledger_user_state on blob_ledger (user_id, state);
create index idx_blob_ledger_cleanup on blob_ledger (state, state_changed_at);
create index idx_blob_ledger_object on blob_ledger (object_id);

create table mutation_results (
    user_id TEXT not null references users (id) on delete cascade,
    mutation_id TEXT not null,
    kind TEXT not null,
    collection_id TEXT not null,
    object_id TEXT,
    requested_version INTEGER,
    result TEXT not null,
    created_at TEXT not null,
    constraint mutation_results_pkey primary key (user_id, mutation_id),
    constraint mutation_results_kind_check check (kind in ('create', 'update', 'delete')),
    constraint mutation_results_id_length_check check (
        length(mutation_id) between 1 and 128
    )
);
create index idx_mutation_results_created_at on mutation_results (created_at);
