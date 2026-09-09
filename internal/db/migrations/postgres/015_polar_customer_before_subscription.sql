-- A Polar customer exists before a subscription does. The checkout and
-- portal routes create one lazily on first use, keyed by the account's sub as
-- Polar's external id, and both need its id afterwards — the checkout to
-- attach the order to it, the portal to open a session for it.
--
-- That id belongs on this row, so the account's whole billing link lives in
-- one place. The rest of the row describes a subscription that has not
-- happened yet, so those two columns stop being required rather than carrying
-- a sentinel that reads like a real subscription and a real delivery.
alter table subscriptions alter column polar_subscription_id drop not null;
alter table subscriptions alter column last_event drop not null;
