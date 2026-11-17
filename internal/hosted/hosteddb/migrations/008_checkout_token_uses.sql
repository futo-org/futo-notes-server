-- An app's checkout link (POST /api/billing/checkout with an embedded web
-- provider) opens one checkout. Its nonce is recorded when it does, and
-- kept until the link would have expired anyway.
create table checkout_token_uses (
    nonce text primary key,
    expires_at timestamptz not null
);
