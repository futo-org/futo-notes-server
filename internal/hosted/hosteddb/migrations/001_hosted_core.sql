-- Hosted users sign in through OIDC and are keyed by sub. The identity
-- provider owns email uniqueness, and an address can move between two of its
-- accounts before both have signed in here again, so a stale copy must not
-- block a login.
alter table users drop constraint if exists users_email_key;

alter table users
    add column region_id text,
    add column deletion_requested_at timestamptz,
    add column purge_at timestamptz;

create index idx_users_purge_at on users (purge_at) where purge_at is not null;

alter table sessions
    add column created_at timestamptz not null default now(),
    add column last_used_at timestamptz not null default now(),
    add column user_agent text not null default '',
    add column device_name text,
    -- When the user last proved who they are to the identity provider for
    -- this session: the ID token's auth_time, or the callback time without
    -- one. Account deletion needs it recent.
    add column authenticated_at timestamptz not null default now();

create index idx_sessions_user on sessions (user_id);
create index idx_sessions_expires_at on sessions (expires_at);

-- Every token a session has been issued stays until the session goes, so a
-- rotated one presented again is recognized as reuse.
create table storage_refresh_tokens (
    token_hash bytea primary key,
    session_id uuid not null references sessions (id) on delete cascade,
    created_at timestamptz not null default now(),
    rotated_at timestamptz
);

create index idx_storage_refresh_tokens_session on storage_refresh_tokens (session_id);

-- A login in flight between /api/auth/oidc/start and the callback.
create table oidc_logins (
    state_hash bytea primary key,
    nonce text not null,
    code_verifier text not null,
    app_redirect_uri text,
    app_state text,
    app_code_challenge text,
    -- Set when the login must prove a fresh sign-in (max_age=0).
    reauthenticate boolean not null default false,
    expires_at timestamptz not null
);

create index idx_oidc_logins_expires_at on oidc_logins (expires_at);

-- The one-time code an app swaps for its session at /api/auth/oidc/token.
create table oidc_app_codes (
    code_hash bytea primary key,
    user_id uuid not null references users (id) on delete cascade,
    code_challenge text not null,
    authenticated_at timestamptz not null,
    expires_at timestamptz not null
);

create index idx_oidc_app_codes_expires_at on oidc_app_codes (expires_at);
