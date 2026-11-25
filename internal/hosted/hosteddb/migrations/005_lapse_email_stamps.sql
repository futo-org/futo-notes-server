-- When each lapse email (internal/hosted/mail) went out to the user. A stamp
-- is set only after the provider accepted the email, and a set stamp skips
-- the send, so a retried job never emails twice.
alter table users
    add column lapse_notified_at timestamptz,
    add column purge_warned_at timestamptz,
    add column purge_notified_at timestamptz;
