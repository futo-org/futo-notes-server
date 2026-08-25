-- Superseded by blob_ledger in 010. The table is retained so that existing
-- databases keep the shape they already have; nothing is written to it.
create table orphaned_blobs (
    blob_key text primary key,
    user_id uuid not null references users (id) on delete cascade,
    size_bytes bigint not null,
    orphaned_at timestamptz not null default now()
);

create index idx_orphaned_blobs_orphaned_at on orphaned_blobs (orphaned_at);
