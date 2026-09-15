-- One subscription record per account, whose subscription fields are written
-- only by the Polar webhook handler and read by the Entitlement check on every
-- gated write. Truth about a subscription lives here rather than at Polar, so
-- a sync request never makes an outbound call. Migration 015 adds the other
-- writer: the checkout and portal routes store polar_customer_id here, and
-- touch nothing else on the row.
--
-- Keyed on the account and never on a collection, so more vaults per account
-- stay an additive migration.
--
-- state is Polar's own subscription status vocabulary (active, trialing,
-- past_due, canceled, ...), stored as it arrives. grace_until is set to the
-- Grace Period past the moment the first past-due delivery is applied, and
-- cleared when payment recovers. That is receipt time rather than the moment
-- the payment failed, which is what a delivery retried after a restart makes
-- visible; see docs/Hosted service plan.md. last_event is the delivery that
-- produced this row, kept so an operator can see what Polar actually said;
-- jsonb normalizes key order and whitespace, so it is the same event rather
-- than the same bytes.
create table if not exists subscriptions (
    user_id uuid primary key references users (id) on delete cascade,
    polar_customer_id text not null,
    polar_subscription_id text not null,
    state text not null,
    current_period_end timestamptz,
    grace_until timestamptz,
    last_event jsonb not null,
    updated_at timestamptz not null
);
