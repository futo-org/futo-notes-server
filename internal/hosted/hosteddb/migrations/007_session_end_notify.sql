-- An open sync event stream must end with its session, on whichever replica
-- holds it. Every way a session ends deletes its row (revocation, sign-out,
-- account deletion signing the other devices out, refresh-token reuse,
-- reaping, purge), and an account pending deletion may not sync at all.
-- Both are announced on notes_session_ends, which every API replica's
-- events listener turns into closing the matching streams.
create function notify_session_end() returns trigger language plpgsql as $$
begin
    perform pg_notify('notes_session_ends', json_build_object('userId', old.user_id, 'sessionId', old.id)::text);
    return null;
end
$$;

create trigger sessions_notify_end after delete on sessions
    for each row execute function notify_session_end();

create function notify_deletion_requested() returns trigger language plpgsql as $$
begin
    perform pg_notify('notes_session_ends', json_build_object('userId', new.id)::text);
    return null;
end
$$;

create trigger users_notify_deletion_requested after update of deletion_requested_at on users
    for each row when (old.deletion_requested_at is null and new.deletion_requested_at is not null)
    execute function notify_deletion_requested();
