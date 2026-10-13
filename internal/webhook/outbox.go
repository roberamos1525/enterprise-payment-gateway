package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Outbox records events in the same transaction as the state change that
// caused them, and fans each event out into one pending delivery per
// subscribed endpoint. The dispatcher sends them asynchronously, so a
// committed state change can never lose its notification and a rolled-back
// change can never emit one.
type Outbox struct {
	clock clock.Clock
}

// NewOutbox builds an outbox.
func NewOutbox(clk clock.Clock) *Outbox { return &Outbox{clock: clk} }

// Publish appends an event and schedules its deliveries inside q's transaction.
func (o *Outbox) Publish(ctx context.Context, q *store.Queries, merchantID, eventType, resourceID string, data any) (model.Event, error) {
	if !knownEvents[eventType] {
		return model.Event{}, fmt.Errorf("webhook: unknown event type %q", eventType)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return model.Event{}, fmt.Errorf("webhook: marshal payload: %w", err)
	}
	now := o.clock.Now()
	ev := model.Event{
		ID: ids.New("evt"), MerchantID: merchantID, Type: eventType, ResourceID: resourceID,
		Payload: payload, CreatedAt: now,
	}
	if err := q.InsertEvent(ctx, ev); err != nil {
		return model.Event{}, err
	}
	endpoints, err := q.ListEndpoints(ctx, merchantID, true)
	if err != nil {
		return model.Event{}, err
	}
	for _, ep := range endpoints {
		if !ep.Subscribes(eventType) {
			continue
		}
		if err := q.InsertDelivery(ctx, newDelivery(ev, ep.ID, "", now)); err != nil {
			return model.Event{}, err
		}
	}
	return ev, nil
}

func newDelivery(ev model.Event, endpointID, replayOf string, now time.Time) model.Delivery {
	return model.Delivery{
		ID: ids.New("whd"), EventID: ev.ID, EndpointID: endpointID, MerchantID: ev.MerchantID,
		Status: model.DeliveryPending, NextAttemptAt: now, ReplayOf: replayOf, CreatedAt: now, UpdatedAt: now,
	}
}

// Replay schedules a fresh delivery of an existing event. With endpointID
// empty, the event is replayed to every enabled endpoint that subscribes to
// it now; otherwise only to the given endpoint (which must belong to the
// event's merchant, be enabled and subscribe to the event type).
func (o *Outbox) Replay(ctx context.Context, db *store.DB, merchantID, eventID, endpointID string) ([]model.Delivery, error) {
	var out []model.Delivery
	err := db.Tx(ctx, func(q *store.Queries) error {
		ev, err := q.GetEvent(ctx, eventID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ev.MerchantID != merchantID) {
			return apperr.NotFound("event", eventID)
		}
		if err != nil {
			return err
		}
		var targets []model.WebhookEndpoint
		if endpointID != "" {
			ep, err := q.GetEndpoint(ctx, endpointID)
			if errors.Is(err, store.ErrNotFound) || (err == nil && ep.MerchantID != merchantID) {
				return apperr.NotFound("webhook endpoint", endpointID)
			}
			if err != nil {
				return err
			}
			if ep.Status != model.EndpointEnabled {
				return apperr.New(apperr.CodeInvalidState, "webhook endpoint is disabled")
			}
			if !ep.Subscribes(ev.Type) {
				return apperr.New(apperr.CodeInvalidRequest, "endpoint does not subscribe to %s", ev.Type)
			}
			targets = append(targets, ep)
		} else {
			eps, err := q.ListEndpoints(ctx, merchantID, true)
			if err != nil {
				return err
			}
			for _, ep := range eps {
				if ep.Subscribes(ev.Type) {
					targets = append(targets, ep)
				}
			}
		}
		if len(targets) == 0 {
			return apperr.New(apperr.CodeInvalidRequest, "no enabled endpoint subscribes to %s", ev.Type)
		}
		prior, _, err := q.ListDeliveries(ctx, store.DeliveryFilter{MerchantID: merchantID, EventID: eventID})
		if err != nil {
			return err
		}
		now := o.clock.Now()
		for _, ep := range targets {
			replayOf := ""
			for _, p := range prior { // newest first
				if p.EndpointID == ep.ID {
					replayOf = p.ID
					break
				}
			}
			d := newDelivery(ev, ep.ID, replayOf, now)
			if err := q.InsertDelivery(ctx, d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

// Envelope is the JSON body POSTed to webhook endpoints.
type Envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	MerchantID string          `json:"merchant_id"`
	CreatedAt  time.Time       `json:"created_at"`
	Data       json.RawMessage `json:"data"`
}

// EnvelopeFor renders an event as a webhook body.
func EnvelopeFor(ev model.Event) ([]byte, error) {
	return json.Marshal(Envelope{ID: ev.ID, Type: ev.Type, MerchantID: ev.MerchantID, CreatedAt: ev.CreatedAt, Data: ev.Payload})
}

// Ping records a webhook_endpoint.ping event and schedules a delivery to
// the given endpoint only, regardless of its event subscriptions.
func (o *Outbox) Ping(ctx context.Context, db *store.DB, merchantID, endpointID string) (model.Delivery, error) {
	var out model.Delivery
	err := db.Tx(ctx, func(q *store.Queries) error {
		ep, err := q.GetEndpoint(ctx, endpointID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ep.MerchantID != merchantID) {
			return apperr.NotFound("webhook endpoint", endpointID)
		}
		if err != nil {
			return err
		}
		if ep.Status != model.EndpointEnabled {
			return apperr.New(apperr.CodeInvalidState, "webhook endpoint is disabled")
		}
		now := o.clock.Now()
		payload, err := json.Marshal(map[string]any{"endpoint_id": ep.ID, "message": "ping"})
		if err != nil {
			return err
		}
		ev := model.Event{ID: ids.New("evt"), MerchantID: merchantID, Type: EventPing, ResourceID: ep.ID,
			Payload: payload, CreatedAt: now}
		if err := q.InsertEvent(ctx, ev); err != nil {
			return err
		}
		out = newDelivery(ev, ep.ID, "", now)
		return q.InsertDelivery(ctx, out)
	})
	return out, err
}
