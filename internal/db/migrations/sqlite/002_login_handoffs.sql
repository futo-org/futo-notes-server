create table login_handoffs (
    ticket TEXT primary key,
    expires_at TEXT not null,
    user_id TEXT references users (id) on delete cascade
);
create index idx_login_handoffs_expires_at on login_handoffs (expires_at);
