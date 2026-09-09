create table plans (
    id TEXT primary key,
    storage_quota_bytes INTEGER not null,
    blob_max_bytes INTEGER not null
);

insert into plans (id, storage_quota_bytes, blob_max_bytes)
values ('default', 10000000000, 104857600);

-- SQLite refuses to add a column carrying a REFERENCES clause unless its
-- default is NULL, so the link to plans is a plain column here. One plan
-- exists and nothing deletes it.
alter table users add column plan_id TEXT not null default 'default';

create table user_usage (
    user_id TEXT primary key references users (id) on delete cascade,
    bytes_used INTEGER not null default 0
);

insert into user_usage (user_id, bytes_used)
select users.id, coalesce(sum(blob_ledger.size_bytes), 0)
from users
left join blob_ledger
    on blob_ledger.user_id = users.id
   and blob_ledger.state in ('claimed', 'retained')
group by users.id;
