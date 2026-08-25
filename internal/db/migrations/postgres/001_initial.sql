create table users (
    id uuid primary key,
    sub text not null unique,
    name text not null,
    email text not null unique
);

create table sessions (
    id uuid primary key,
    user_id uuid not null references users (id) on delete cascade,
    access_token_hash bytea not null unique,
    expires_at timestamptz not null
);

create index idx_sessions_token_hash on sessions (access_token_hash);

create table collections (
    id uuid primary key,
    user_id uuid not null references users (id) on delete cascade,
    created_at timestamptz not null default now()
);

create index idx_collections_user on collections (user_id);

create table objects (
    id uuid primary key,
    collection_id uuid not null references collections (id) on delete cascade,
    user_id uuid not null references users (id) on delete cascade,
    version bigint not null default 1,
    deleted boolean not null default false,
    blob_key text,
    size_bytes bigint,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

create index idx_objects_collection on objects (collection_id, user_id);
