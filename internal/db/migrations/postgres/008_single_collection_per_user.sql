-- Reserved migration number. This briefly enforced one collection per user
-- before the change reached a stable release. Upgrades may still find the name
-- recorded, so it has to stay present, and it must never discard client data.
-- 009 repairs databases that ran the original destructive version.
select 1 where false;
