create table subscriptions (
    user_id TEXT primary key references users (id) on delete cascade,
    polar_customer_id TEXT not null,
    polar_subscription_id TEXT not null,
    state TEXT not null,
    current_period_end TEXT,
    grace_until TEXT,
    last_event TEXT not null,
    updated_at TEXT not null
);
