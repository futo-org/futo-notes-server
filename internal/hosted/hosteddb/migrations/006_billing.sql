-- Billing (launch plan C1). store_account_token is what the apps hand the App
-- Store (appAccountToken) and Play (obfuscatedAccountId), so a store purchase
-- names its account without exposing the OIDC sub. lapsed_at is when the
-- account last stopped being entitled; the lapse emails and the purge 90
-- days later count from it, and resubscribing clears it.
alter table users
    add column store_account_token uuid not null unique default gen_random_uuid(),
    add column lapsed_at timestamptz;

create index idx_users_lapsed_at on users (lapsed_at) where lapsed_at is not null;

-- One row per subscription, the newest provider event's view of it. An
-- account is entitled while any of its rows is active, or in grace, and not
-- yet past its end. Purging the account deletes its rows, which releases a
-- store subscription to the next account that submits it.
create table entitlements (
    source text not null,
    external_id text not null,
    user_id uuid not null references users (id) on delete cascade,
    product_id text not null,
    status text not null,
    will_renew boolean not null,
    expires_at timestamptz,
    grace_until timestamptz,
    environment text not null,
    event_at timestamptz not null,
    raw jsonb not null,
    updated_at timestamptz not null default now(),
    primary key (source, external_id)
);

create index idx_entitlements_user on entitlements (user_id);

-- Webhook deliveries already applied, so a provider's retry is acknowledged
-- without being applied twice. Pruned after 30 days.
create table billing_deliveries (
    source text not null,
    delivery_id text not null,
    received_at timestamptz not null default now(),
    primary key (source, delivery_id)
);

create index idx_billing_deliveries_received_at on billing_deliveries (received_at);
