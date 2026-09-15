-- Whether a session's expiry slides forward with use. See the Postgres
-- migration of the same name.
alter table sessions add column sliding INTEGER not null default 0;
