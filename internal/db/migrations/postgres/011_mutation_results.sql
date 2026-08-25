create table if not exists mutation_results (
    user_id uuid not null references users (id) on delete cascade,
    mutation_id text not null,
    kind text not null,
    collection_id uuid not null,
    object_id uuid,
    requested_version bigint,
    result jsonb not null,
    created_at timestamptz not null default now(),
    constraint mutation_results_pkey primary key (user_id, mutation_id),
    constraint mutation_results_kind_check check (kind in ('create', 'update', 'delete')),
    constraint mutation_results_id_length_check check (
        char_length(mutation_id) between 1 and 128
    )
);

create index if not exists idx_mutation_results_created_at on mutation_results (created_at);
