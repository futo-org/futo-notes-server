-- A Plan is the named set of limits an account is entitled to: a Storage
-- Quota and a per-blob cap. One plan exists, and it carries the two values
-- the server enforced as constants before this migration, so an existing
-- deployment sees no change in what it accepts.
create table if not exists plans (
    id text primary key,
    storage_quota_bytes bigint not null,
    blob_max_bytes bigint not null
);

insert into plans (id, storage_quota_bytes, blob_max_bytes)
values ('default', 10000000000, 104857600)
on conflict (id) do nothing;

alter table users add column if not exists plan_id text not null default 'default'
    references plans (id);

-- Storage Usage: the account's claimed and retained blob bytes. Staged and
-- purgeable bytes are excluded, matching the Storage Quota's definition. The
-- counter is moved by the blob-ledger transitions rather than summed at
-- upload time, and reconciliation recomputes it from the ledger.
--
-- Keyed on the account and never on a collection, so more vaults per account
-- stay an additive migration.
create table if not exists user_usage (
    user_id uuid primary key references users (id) on delete cascade,
    bytes_used bigint not null default 0
);

insert into user_usage (user_id, bytes_used)
select users.id, coalesce(sum(blob_ledger.size_bytes), 0)
from users
left join blob_ledger
    on blob_ledger.user_id = users.id
   and blob_ledger.state in ('claimed', 'retained')
group by users.id
on conflict (user_id) do nothing;
