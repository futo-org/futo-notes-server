-- Login Hand-off tickets. A row records which account authenticated, never a
-- session token: the session is opened when the ticket is redeemed, so a leak
-- of this table yields no usable bearer credential.
create table if not exists login_handoffs (
    ticket text primary key,
    expires_at timestamptz not null,
    user_id uuid references users (id) on delete cascade
);

create index if not exists idx_login_handoffs_expires_at on login_handoffs (expires_at);
