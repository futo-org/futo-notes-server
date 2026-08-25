create table if not exists blob_ledger (
    blob_key text primary key,
    user_id uuid not null references users (id) on delete cascade,
    size_bytes bigint not null,
    state text not null,
    collection_id uuid,
    object_id uuid references objects (id) on delete set null,
    object_version bigint,
    created_at timestamptz not null default now(),
    state_changed_at timestamptz not null default now(),
    constraint blob_ledger_state_check check (
        state in ('staged', 'claimed', 'retained', 'purgeable', 'legacy_shared')
    )
);

create index if not exists idx_blob_ledger_user_state on blob_ledger (user_id, state);
create index if not exists idx_blob_ledger_cleanup on blob_ledger (state, state_changed_at);
create index if not exists idx_blob_ledger_object on blob_ledger (object_id);

-- Existing object rows are authoritative. Historic duplicate references are
-- preserved as legacy_shared and are never eligible for cleanup.
insert into blob_ledger (
    blob_key,
    user_id,
    size_bytes,
    state,
    collection_id,
    object_id,
    object_version,
    created_at,
    state_changed_at
)
select
    blob_key,
    (array_agg(user_id order by user_id::text))[1],
    coalesce(max(size_bytes), 0),
    case when count(*) = 1 then 'claimed' else 'legacy_shared' end,
    case
        when count(distinct collection_id) = 1
        then (array_agg(collection_id order by collection_id::text))[1]
        else null
    end,
    case when count(*) = 1 then (array_agg(id order by id::text))[1] else null end,
    case when count(*) = 1 then max(version) else null end,
    min(created_at),
    max(updated_at)
from objects
where blob_key is not null
group by blob_key
on conflict (blob_key) do nothing;

-- A key still referenced by an object stays claimed/legacy_shared. Otherwise
-- preserve the old retention timestamp in the canonical ledger.
insert into blob_ledger (
    blob_key,
    user_id,
    size_bytes,
    state,
    collection_id,
    object_id,
    object_version,
    created_at,
    state_changed_at
)
select
    blob_key,
    user_id,
    size_bytes,
    'retained',
    null,
    null,
    null,
    orphaned_at,
    orphaned_at
from orphaned_blobs
on conflict (blob_key) do nothing;
