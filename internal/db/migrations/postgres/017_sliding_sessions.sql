-- A session opened by redeeming a Login Hand-off ticket is a Sliding Session:
-- it expires 90 days after its last authenticated request, so a device that
-- syncs at least that often never logs in again. The hand-off is the hosted
-- service's only login path, so a fixed 7-day session would bounce every app
-- into a browser weekly.
--
-- Browser cookie sessions, and every password- and dev-mode login, keep the
-- fixed lifetime. That is the default, so sessions issued before this
-- migration keep the lifetime they were opened with.
alter table sessions add column sliding boolean not null default false;
