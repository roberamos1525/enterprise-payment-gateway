-- Core schema for the simulated payment gateway.
-- All timestamps are INTEGER unix nanoseconds (UTC). Money columns are
-- INTEGER minor units, always paired with an ISO 4217 currency column.

CREATE TABLE merchants (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    email            TEXT NOT NULL,
    country          TEXT NOT NULL,
    default_currency TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('pending','active','suspended','closed')),
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE UNIQUE INDEX merchants_email ON merchants(email);

CREATE TABLE fee_schedules (
    id                     TEXT PRIMARY KEY,
    merchant_id            TEXT NOT NULL REFERENCES merchants(id),
    percent_bps            INTEGER NOT NULL CHECK (percent_bps BETWEEN 0 AND 10000),
    fixed_minor            INTEGER NOT NULL CHECK (fixed_minor >= 0),
    refund_fixed_minor     INTEGER NOT NULL CHECK (refund_fixed_minor >= 0),
    return_fee_on_refund   INTEGER NOT NULL DEFAULT 0,
    effective_from         INTEGER NOT NULL,
    created_at             INTEGER NOT NULL
);
CREATE INDEX fee_schedules_merchant ON fee_schedules(merchant_id, effective_from);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    merchant_id  TEXT NOT NULL REFERENCES merchants(id),
    prefix       TEXT NOT NULL UNIQUE,
    hash         TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('active','revoked')),
    rotated_from TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    revoked_at   INTEGER,
    last_used_at INTEGER
);
CREATE INDEX api_keys_merchant ON api_keys(merchant_id);

CREATE TABLE webhook_endpoints (
    id                     TEXT PRIMARY KEY,
    merchant_id            TEXT NOT NULL REFERENCES merchants(id),
    url                    TEXT NOT NULL,
    description            TEXT NOT NULL DEFAULT '',
    events                 TEXT NOT NULL,
    status                 TEXT NOT NULL CHECK (status IN ('enabled','disabled')),
    secret_sealed          TEXT NOT NULL,
    prev_secret_sealed     TEXT,
    prev_secret_expires_at INTEGER,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL
);
CREATE INDEX webhook_endpoints_merchant ON webhook_endpoints(merchant_id);

CREATE TABLE vault_cards (
    token        TEXT PRIMARY KEY,
    merchant_id  TEXT NOT NULL REFERENCES merchants(id),
    fingerprint  TEXT NOT NULL,
    key_id       TEXT NOT NULL,
    ciphertext   BLOB NOT NULL,
    brand        TEXT NOT NULL,
    bin          TEXT NOT NULL,
    last4        TEXT NOT NULL,
    exp_month    INTEGER NOT NULL,
    exp_year     INTEGER NOT NULL,
    country      TEXT NOT NULL,
    funding      TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX vault_cards_fingerprint ON vault_cards(fingerprint);
CREATE INDEX vault_cards_key ON vault_cards(key_id);

CREATE TABLE payment_intents (
    id                  TEXT PRIMARY KEY,
    merchant_id         TEXT NOT NULL REFERENCES merchants(id),
    amount              INTEGER NOT NULL CHECK (amount > 0),
    currency            TEXT NOT NULL,
    status              TEXT NOT NULL,
    capture_method      TEXT NOT NULL CHECK (capture_method IN ('automatic','manual')),
    amount_authorized   INTEGER NOT NULL DEFAULT 0,
    amount_captured     INTEGER NOT NULL DEFAULT 0,
    amount_refunded     INTEGER NOT NULL DEFAULT 0,
    card_token          TEXT,
    card_brand          TEXT,
    card_bin            TEXT,
    card_last4          TEXT,
    card_country        TEXT,
    card_fingerprint    TEXT,
    auth_code           TEXT,
    auth_ref            TEXT,
    capture_ref         TEXT,
    decline_code        TEXT,
    decline_message     TEXT,
    risk_score          INTEGER NOT NULL DEFAULT 0,
    risk_action         TEXT,
    risk_reasons        TEXT,
    billing_country     TEXT,
    customer_ip         TEXT,
    challenge_id        TEXT,
    description         TEXT NOT NULL DEFAULT '',
    metadata            TEXT NOT NULL DEFAULT '{}',
    cancellation_reason TEXT,
    version             INTEGER NOT NULL DEFAULT 1,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    authorized_at       INTEGER,
    captured_at         INTEGER,
    canceled_at         INTEGER,
    auth_expires_at     INTEGER,
    CHECK (amount_captured <= amount_authorized),
    CHECK (amount_refunded <= amount_captured)
);
CREATE INDEX payment_intents_merchant ON payment_intents(merchant_id, id);
CREATE INDEX payment_intents_status ON payment_intents(status, auth_expires_at);
CREATE INDEX payment_intents_captured ON payment_intents(captured_at);

CREATE TABLE payment_history (
    id           TEXT PRIMARY KEY,
    payment_id   TEXT NOT NULL REFERENCES payment_intents(id),
    action       TEXT NOT NULL,
    from_status  TEXT NOT NULL,
    to_status    TEXT NOT NULL,
    amount       INTEGER NOT NULL DEFAULT 0,
    detail       TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX payment_history_payment ON payment_history(payment_id, id);

CREATE TABLE refunds (
    id            TEXT PRIMARY KEY,
    payment_id    TEXT NOT NULL REFERENCES payment_intents(id),
    merchant_id   TEXT NOT NULL REFERENCES merchants(id),
    amount        INTEGER NOT NULL CHECK (amount > 0),
    currency      TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('succeeded','failed')),
    reason        TEXT NOT NULL DEFAULT '',
    acquirer_ref  TEXT,
    failure_code  TEXT,
    created_at    INTEGER NOT NULL
);
CREATE INDEX refunds_payment ON refunds(payment_id);
CREATE INDEX refunds_created ON refunds(created_at);

CREATE TABLE idempotency_keys (
    merchant_id     TEXT NOT NULL,
    key             TEXT NOT NULL,
    method          TEXT NOT NULL,
    path            TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('in_progress','completed')),
    response_status INTEGER,
    response_body   BLOB,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    locked_until    INTEGER NOT NULL,
    PRIMARY KEY (merchant_id, key)
);
CREATE INDEX idempotency_expiry ON idempotency_keys(expires_at);

CREATE TABLE events (
    id          TEXT PRIMARY KEY,
    merchant_id TEXT NOT NULL,
    type        TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    payload     TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX events_merchant ON events(merchant_id, id);

CREATE TABLE webhook_deliveries (
    id               TEXT PRIMARY KEY,
    event_id         TEXT NOT NULL REFERENCES events(id),
    endpoint_id      TEXT NOT NULL REFERENCES webhook_endpoints(id),
    merchant_id      TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('pending','succeeded','failed')),
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  INTEGER NOT NULL,
    locked_until     INTEGER NOT NULL DEFAULT 0,
    last_status_code INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    replay_of        TEXT,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    completed_at     INTEGER
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at);
CREATE INDEX webhook_deliveries_endpoint ON webhook_deliveries(endpoint_id, id);
CREATE INDEX webhook_deliveries_event ON webhook_deliveries(event_id);

CREATE TABLE webhook_attempts (
    id               TEXT PRIMARY KEY,
    delivery_id      TEXT NOT NULL REFERENCES webhook_deliveries(id),
    attempt          INTEGER NOT NULL,
    status_code      INTEGER NOT NULL,
    duration_ms      INTEGER NOT NULL,
    error            TEXT NOT NULL DEFAULT '',
    response_snippet TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL
);
CREATE INDEX webhook_attempts_delivery ON webhook_attempts(delivery_id, attempt);

CREATE TABLE ledger_journals (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    ref         TEXT NOT NULL,
    merchant_id TEXT NOT NULL,
    currency    TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (kind, ref)
);
CREATE INDEX ledger_journals_merchant ON ledger_journals(merchant_id, created_at);

CREATE TABLE ledger_entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    journal_id  TEXT NOT NULL REFERENCES ledger_journals(id),
    account     TEXT NOT NULL,
    merchant_id TEXT NOT NULL,
    currency    TEXT NOT NULL,
    debit       INTEGER NOT NULL CHECK (debit >= 0),
    credit      INTEGER NOT NULL CHECK (credit >= 0),
    created_at  INTEGER NOT NULL
);
CREATE INDEX ledger_entries_account ON ledger_entries(account, merchant_id, currency);

CREATE TABLE settlement_batches (
    id               TEXT PRIMARY KEY,
    merchant_id      TEXT NOT NULL REFERENCES merchants(id),
    settlement_date  TEXT NOT NULL,
    currency         TEXT NOT NULL,
    gross_captured   INTEGER NOT NULL,
    refunded         INTEGER NOT NULL,
    fees             INTEGER NOT NULL,
    net              INTEGER NOT NULL,
    capture_count    INTEGER NOT NULL,
    refund_count     INTEGER NOT NULL,
    fee_schedule_id  TEXT NOT NULL,
    status           TEXT NOT NULL,
    created_at       INTEGER NOT NULL
);
CREATE INDEX settlement_batches_merchant_day ON settlement_batches(merchant_id, settlement_date);

CREATE TABLE settlement_items (
    batch_id    TEXT NOT NULL REFERENCES settlement_batches(id),
    kind        TEXT NOT NULL CHECK (kind IN ('capture','refund')),
    ref_id      TEXT NOT NULL,
    payment_id  TEXT NOT NULL,
    amount      INTEGER NOT NULL,
    fee         INTEGER NOT NULL,
    occurred_at INTEGER NOT NULL,
    PRIMARY KEY (kind, ref_id)
);
CREATE INDEX settlement_items_batch ON settlement_items(batch_id);

CREATE TABLE payouts (
    id            TEXT PRIMARY KEY,
    batch_id      TEXT NOT NULL UNIQUE REFERENCES settlement_batches(id),
    merchant_id   TEXT NOT NULL REFERENCES merchants(id),
    amount        INTEGER NOT NULL CHECK (amount > 0),
    currency      TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('scheduled','paid')),
    scheduled_for TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    paid_at       INTEGER
);
CREATE INDEX payouts_merchant ON payouts(merchant_id, id);

CREATE TABLE acquirer_transactions (
    ref          TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('authorization','capture','void','refund')),
    parent_ref   TEXT,
    merchant_id  TEXT NOT NULL,
    amount       INTEGER NOT NULL,
    currency     TEXT NOT NULL,
    approved     INTEGER NOT NULL,
    response     TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX acquirer_transactions_day ON acquirer_transactions(kind, created_at);

CREATE TABLE reconciliation_runs (
    id                 TEXT PRIMARY KEY,
    settlement_date    TEXT NOT NULL,
    source             TEXT NOT NULL,
    internal_count     INTEGER NOT NULL,
    acquirer_count     INTEGER NOT NULL,
    matched_count      INTEGER NOT NULL,
    discrepancy_count  INTEGER NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('balanced','discrepancies')),
    created_at         INTEGER NOT NULL
);

CREATE TABLE reconciliation_discrepancies (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES reconciliation_runs(id),
    kind             TEXT NOT NULL,
    acquirer_ref     TEXT NOT NULL,
    merchant_id      TEXT NOT NULL,
    internal_amount  INTEGER,
    acquirer_amount  INTEGER,
    currency         TEXT NOT NULL,
    detail           TEXT NOT NULL
);
CREATE INDEX reconciliation_discrepancies_run ON reconciliation_discrepancies(run_id);

CREATE TABLE fraud_bin_blocklist (
    bin        TEXT PRIMARY KEY,
    reason     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE audit_logs (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    id            TEXT NOT NULL UNIQUE,
    created_at    INTEGER NOT NULL,
    actor_type    TEXT NOT NULL,
    actor_id      TEXT NOT NULL,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    merchant_id   TEXT NOT NULL DEFAULT '',
    request_id    TEXT NOT NULL DEFAULT '',
    ip            TEXT NOT NULL DEFAULT '',
    metadata      TEXT NOT NULL DEFAULT '{}',
    prev_hash     TEXT NOT NULL,
    hash          TEXT NOT NULL
);
CREATE INDEX audit_logs_resource ON audit_logs(resource_type, resource_id);
CREATE INDEX audit_logs_merchant ON audit_logs(merchant_id, seq);

-- The audit log is append-only: the database itself rejects edits.
CREATE TRIGGER audit_logs_no_update BEFORE UPDATE ON audit_logs
BEGIN
    SELECT RAISE(ABORT, 'audit_logs is append-only');
END;
CREATE TRIGGER audit_logs_no_delete BEFORE DELETE ON audit_logs
BEGIN
    SELECT RAISE(ABORT, 'audit_logs is append-only');
END;
