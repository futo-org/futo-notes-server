-- When each background job last started on any replica. Every replica runs
-- the scheduler; a job that ran recently elsewhere is skipped, so it runs at
-- most once per interval across the deployment (hostedjobs.Locked).
create table hosted_job_runs (
    name text primary key,
    last_started_at timestamptz not null
);
