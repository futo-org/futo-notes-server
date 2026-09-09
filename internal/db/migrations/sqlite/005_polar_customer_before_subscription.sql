-- A Polar customer exists before a subscription does. See the Postgres
-- migration of the same name for why polar_subscription_id and last_event
-- stop being required.
--
-- SQLite cannot drop a NOT NULL, so the table is rebuilt. Nothing else
-- references subscriptions, and the foreign key it holds is reproduced in the
-- new definition.
create table subscriptions_rebuilt (
    user_id TEXT primary key references users (id) on delete cascade,
    polar_customer_id TEXT not null,
    polar_subscription_id TEXT,
    state TEXT not null,
    current_period_end TEXT,
    grace_until TEXT,
    last_event TEXT,
    updated_at TEXT not null
);

insert into subscriptions_rebuilt
select user_id, polar_customer_id, polar_subscription_id, state,
       current_period_end, grace_until, last_event, updated_at
from subscriptions;

drop table subscriptions;
alter table subscriptions_rebuilt rename to subscriptions;
