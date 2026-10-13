package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

const endpointCols = `id, merchant_id, url, description, events, status, secret_sealed, prev_secret_sealed,
	prev_secret_expires_at, created_at, updated_at`

func scanEndpoint(row interface{ Scan(...any) error }) (model.WebhookEndpoint, error) {
	var e model.WebhookEndpoint
	var events string
	var prev sql.NullString
	var prevExp sql.NullInt64
	var created, updated int64
	err := row.Scan(&e.ID, &e.MerchantID, &e.URL, &e.Description, &events, &e.Status, &e.SecretSealed, &prev,
		&prevExp, &created, &updated)
	if err != nil {
		return e, notFound(err)
	}
	if err := json.Unmarshal([]byte(events), &e.Events); err != nil {
		return e, err
	}
	e.PrevSecretSealed = prev.String
	e.PrevSecretExpiresAt = fromNullTS(prevExp)
	e.CreatedAt, e.UpdatedAt = fromTS(created), fromTS(updated)
	return e, nil
}

// InsertEndpoint stores a webhook endpoint.
func (q *Queries) InsertEndpoint(ctx context.Context, e model.WebhookEndpoint) error {
	events, err := json.Marshal(e.Events)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO webhook_endpoints (`+endpointCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.MerchantID, e.URL, e.Description, string(events), e.Status, e.SecretSealed,
		nullStr(e.PrevSecretSealed), nullTS(e.PrevSecretExpiresAt), ts(e.CreatedAt), ts(e.UpdatedAt))
	return err
}

// UpdateEndpoint overwrites the mutable fields of an endpoint.
func (q *Queries) UpdateEndpoint(ctx context.Context, e model.WebhookEndpoint) error {
	events, err := json.Marshal(e.Events)
	if err != nil {
		return err
	}
	res, err := q.db.ExecContext(ctx, `UPDATE webhook_endpoints SET url = ?, description = ?, events = ?, status = ?,
		secret_sealed = ?, prev_secret_sealed = ?, prev_secret_expires_at = ?, updated_at = ? WHERE id = ?`,
		e.URL, e.Description, string(events), e.Status, e.SecretSealed, nullStr(e.PrevSecretSealed),
		nullTS(e.PrevSecretExpiresAt), ts(e.UpdatedAt), e.ID)
	return affectedOne(res, err)
}

// GetEndpoint loads an endpoint by id.
func (q *Queries) GetEndpoint(ctx context.Context, id string) (model.WebhookEndpoint, error) {
	return scanEndpoint(q.db.QueryRowContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = ?`, id))
}

// ListEndpoints returns a merchant's endpoints; enabledOnly filters disabled ones.
func (q *Queries) ListEndpoints(ctx context.Context, merchantID string, enabledOnly bool) ([]model.WebhookEndpoint, error) {
	query := `SELECT ` + endpointCols + ` FROM webhook_endpoints WHERE merchant_id = ?`
	if enabledOnly {
		query += ` AND status = 'enabled'`
	}
	rows, err := q.db.QueryContext(ctx, query+` ORDER BY id`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.WebhookEndpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- events (outbox) ----------------------------------------------------------

const eventCols = `id, merchant_id, type, resource_id, payload, created_at`

func scanEvent(row interface{ Scan(...any) error }) (model.Event, error) {
	var e model.Event
	var payload string
	var created int64
	if err := row.Scan(&e.ID, &e.MerchantID, &e.Type, &e.ResourceID, &payload, &created); err != nil {
		return e, notFound(err)
	}
	e.Payload = json.RawMessage(payload)
	e.CreatedAt = fromTS(created)
	return e, nil
}

// InsertEvent appends an event to the outbox.
func (q *Queries) InsertEvent(ctx context.Context, e model.Event) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO events (`+eventCols+`) VALUES (?,?,?,?,?,?)`,
		e.ID, e.MerchantID, e.Type, e.ResourceID, string(e.Payload), ts(e.CreatedAt))
	return err
}

// GetEvent loads an event by id.
func (q *Queries) GetEvent(ctx context.Context, id string) (model.Event, error) {
	return scanEvent(q.db.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE id = ?`, id))
}

// EventFilter narrows ListEvents.
type EventFilter struct {
	MerchantID string
	Type       string
	ResourceID string
	Page       Page
}

// ListEvents pages through events, newest first.
func (q *Queries) ListEvents(ctx context.Context, f EventFilter) ([]model.Event, bool, error) {
	where := []string{"merchant_id = ?"}
	args := []any{f.MerchantID}
	if f.Type != "" {
		where, args = append(where, "type = ?"), append(args, f.Type)
	}
	if f.ResourceID != "" {
		where, args = append(where, "resource_id = ?"), append(args, f.ResourceID)
	}
	where, args, tail := pageClause(f.Page, where, args)
	rows, err := q.db.QueryContext(ctx, `SELECT `+eventCols+` FROM events`+whereSQL(where)+tail, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, f.Page)
	return out, more, nil
}

// --- deliveries ---------------------------------------------------------------

const deliveryCols = `id, event_id, endpoint_id, merchant_id, status, attempts, next_attempt_at, locked_until,
	last_status_code, last_error, replay_of, created_at, updated_at, completed_at`

func scanDelivery(row interface{ Scan(...any) error }) (model.Delivery, error) {
	var d model.Delivery
	var next, locked, created, updated int64
	var replay sql.NullString
	var completed sql.NullInt64
	err := row.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.MerchantID, &d.Status, &d.Attempts, &next, &locked,
		&d.LastStatusCode, &d.LastError, &replay, &created, &updated, &completed)
	if err != nil {
		return d, notFound(err)
	}
	d.NextAttemptAt, d.LockedUntil = fromTS(next), fromTS(locked)
	d.CreatedAt, d.UpdatedAt = fromTS(created), fromTS(updated)
	d.ReplayOf = replay.String
	d.CompletedAt = fromNullTS(completed)
	return d, nil
}

// InsertDelivery schedules a delivery.
func (q *Queries) InsertDelivery(ctx context.Context, d model.Delivery) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO webhook_deliveries (`+deliveryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.EventID, d.EndpointID, d.MerchantID, d.Status, d.Attempts, ts(d.NextAttemptAt), ts(d.LockedUntil),
		d.LastStatusCode, d.LastError, nullStr(d.ReplayOf), ts(d.CreatedAt), ts(d.UpdatedAt), nullTS(d.CompletedAt))
	return err
}

// GetDelivery loads a delivery.
func (q *Queries) GetDelivery(ctx context.Context, id string) (model.Delivery, error) {
	return scanDelivery(q.db.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE id = ?`, id))
}

// ClaimDueDeliveries leases up to limit pending deliveries whose next attempt
// is due and that are not leased by another dispatcher.
func (q *Queries) ClaimDueDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]model.Delivery, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE status = 'pending' AND next_attempt_at <= ? AND locked_until <= ?
		ORDER BY next_attempt_at, id LIMIT ?`, ts(now), ts(now), limit)
	if err != nil {
		return nil, err
	}
	var out []model.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	until := now.Add(lease)
	for i := range out {
		if _, err := q.db.ExecContext(ctx, `UPDATE webhook_deliveries SET locked_until = ? WHERE id = ?`,
			ts(until), out[i].ID); err != nil {
			return nil, err
		}
		out[i].LockedUntil = until
	}
	return out, nil
}

// UpdateDeliveryResult records the outcome of an attempt.
func (q *Queries) UpdateDeliveryResult(ctx context.Context, d model.Delivery) error {
	res, err := q.db.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, attempts = ?, next_attempt_at = ?,
		locked_until = 0, last_status_code = ?, last_error = ?, updated_at = ?, completed_at = ? WHERE id = ?`,
		d.Status, d.Attempts, ts(d.NextAttemptAt), d.LastStatusCode, d.LastError, ts(d.UpdatedAt),
		nullTS(d.CompletedAt), d.ID)
	return affectedOne(res, err)
}

// DeliveryFilter narrows ListDeliveries.
type DeliveryFilter struct {
	MerchantID string
	EndpointID string
	EventID    string
	Status     model.DeliveryStatus
	Page       Page
}

// ListDeliveries pages through deliveries, newest first.
func (q *Queries) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]model.Delivery, bool, error) {
	where := []string{"merchant_id = ?"}
	args := []any{f.MerchantID}
	if f.EndpointID != "" {
		where, args = append(where, "endpoint_id = ?"), append(args, f.EndpointID)
	}
	if f.EventID != "" {
		where, args = append(where, "event_id = ?"), append(args, f.EventID)
	}
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	where, args, tail := pageClause(f.Page, where, args)
	rows, err := q.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries`+whereSQL(where)+tail, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []model.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, f.Page)
	return out, more, nil
}

// CountDeliveriesByStatus returns counts of deliveries per status (for metrics).
func (q *Queries) CountDeliveriesByStatus(ctx context.Context) (map[model.DeliveryStatus]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM webhook_deliveries GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[model.DeliveryStatus]int64{}
	for rows.Next() {
		var s model.DeliveryStatus
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// --- attempts (delivery log) --------------------------------------------------

// InsertAttempt appends to the delivery log.
func (q *Queries) InsertAttempt(ctx context.Context, a model.DeliveryAttempt) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO webhook_attempts
		(id, delivery_id, attempt, status_code, duration_ms, error, response_snippet, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.DeliveryID, a.Attempt, a.StatusCode, a.DurationMS, a.Error, a.ResponseSnippet, ts(a.CreatedAt))
	return err
}

// ListAttempts returns the delivery log of one delivery, oldest first.
func (q *Queries) ListAttempts(ctx context.Context, deliveryID string) ([]model.DeliveryAttempt, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id, delivery_id, attempt, status_code, duration_ms, error,
		response_snippet, created_at FROM webhook_attempts WHERE delivery_id = ? ORDER BY attempt`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DeliveryAttempt
	for rows.Next() {
		var a model.DeliveryAttempt
		var created int64
		if err := rows.Scan(&a.ID, &a.DeliveryID, &a.Attempt, &a.StatusCode, &a.DurationMS, &a.Error,
			&a.ResponseSnippet, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = fromTS(created)
		out = append(out, a)
	}
	return out, rows.Err()
}
