-- A subscription can be hidden from the main feeds list without
-- affecting its schedule or whether it's served — purely a tidy-away
-- for shows the admin doesn't want cluttering the dashboard.
ALTER TABLE subscriptions ADD COLUMN hidden_at TIMESTAMP;
