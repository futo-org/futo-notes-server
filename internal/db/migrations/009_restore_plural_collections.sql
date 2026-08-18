-- Restores the plural-collection schema for databases that ran the original
-- destructive 008 from a development image. Databases that ran the no-op 008
-- have no constraint to drop and already have the index, so both statements
-- are conditional and neither touches row data.
alter table collections drop constraint if exists collections_user_id_unique;

create index if not exists idx_collections_user on collections (user_id);
