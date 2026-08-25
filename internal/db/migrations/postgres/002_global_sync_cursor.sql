alter table collections add column current_version bigint not null default 0;

alter table objects add column change_seq bigint not null default 0;

update objects
set change_seq = version;

update collections
set current_version = coalesce((
    select max(objects.change_seq)
    from objects
    where objects.collection_id = collections.id
), 0);

create index idx_objects_collection_change_seq
    on objects (collection_id, user_id, change_seq);
