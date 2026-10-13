package merchant

import (
	"context"
	"errors"
	"strings"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// EndpointWithSecret is returned when a signing secret is created or
// rotated; it is the only time the plaintext secret is shown.
type EndpointWithSecret struct {
	model.WebhookEndpoint
	Secret string `json:"secret"`
}

// EndpointInput creates or updates an endpoint (nil fields are unchanged on update).
type EndpointInput struct {
	URL         *string   `json:"url,omitempty"`
	Description *string   `json:"description,omitempty"`
	Events      *[]string `json:"events,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

func (s *Service) validateEndpoint(v *apperr.Validator, in EndpointInput, creating bool) {
	if in.URL != nil || creating {
		u := ""
		if in.URL != nil {
			u = strings.TrimSpace(*in.URL)
		}
		if v.Required("url", u) {
			if err := webhook.ValidateURL(u, s.opts.WebhookURLPolicy); err != nil {
				v.Add("url", "%s", err.Error())
			}
		}
	}
	if in.Description != nil {
		v.MaxLen("description", *in.Description, 500)
	}
	if in.Events != nil || creating {
		var evs []string
		if in.Events != nil {
			evs = *in.Events
		}
		if v.Check(len(evs) > 0 && len(evs) <= 32, "events", "must list 1-32 event types (or \"*\")") {
			for _, e := range evs {
				v.Check(webhook.KnownEvent(e), "events", "unknown event type %q", e)
			}
		}
	}
}

func newSecret() string { return "whsec_" + ids.RandomToken(24) }

// CreateEndpoint registers a webhook endpoint with a fresh signing secret.
func (s *Service) CreateEndpoint(ctx context.Context, merchantID string, in EndpointInput) (EndpointWithSecret, error) {
	var v apperr.Validator
	s.validateEndpoint(&v, in, true)
	if err := v.Err(); err != nil {
		return EndpointWithSecret{}, err
	}
	now := s.clock.Now()
	ep := model.WebhookEndpoint{
		ID: ids.New("we"), MerchantID: merchantID, URL: strings.TrimSpace(*in.URL), Events: dedupe(*in.Events),
		Status: model.EndpointEnabled, CreatedAt: now, UpdatedAt: now,
	}
	if in.Description != nil {
		ep.Description = *in.Description
	}
	if in.Enabled != nil && !*in.Enabled {
		ep.Status = model.EndpointDisabled
	}
	secret := newSecret()
	sealed, err := s.kr.SealString(secret, webhook.SecretAAD(ep.ID))
	if err != nil {
		return EndpointWithSecret{}, err
	}
	ep.SecretSealed = sealed
	err = s.db.Tx(ctx, func(q *store.Queries) error {
		existing, err := q.ListEndpoints(ctx, merchantID, false)
		if err != nil {
			return err
		}
		if len(existing) >= s.opts.MaxEndpoints {
			return apperr.New(apperr.CodeConflict, "merchant already has the maximum of %d webhook endpoints", s.opts.MaxEndpoints)
		}
		if err := q.InsertEndpoint(ctx, ep); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "webhook_endpoint.create", ResourceType: "webhook_endpoint", ResourceID: ep.ID, MerchantID: merchantID,
			Metadata: map[string]any{"url": ep.URL, "events": ep.Events},
		})
	})
	if err != nil {
		return EndpointWithSecret{}, err
	}
	return EndpointWithSecret{WebhookEndpoint: ep, Secret: secret}, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range in {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// GetEndpoint loads one of a merchant's endpoints.
func (s *Service) GetEndpoint(ctx context.Context, merchantID, id string) (model.WebhookEndpoint, error) {
	ep, err := s.db.Q().GetEndpoint(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ep.MerchantID != merchantID) {
		return model.WebhookEndpoint{}, apperr.NotFound("webhook endpoint", id)
	}
	return ep, err
}

// ListEndpoints lists a merchant's endpoints.
func (s *Service) ListEndpoints(ctx context.Context, merchantID string) ([]model.WebhookEndpoint, error) {
	return s.db.Q().ListEndpoints(ctx, merchantID, false)
}

// UpdateEndpoint changes an endpoint's URL, description, events or status.
func (s *Service) UpdateEndpoint(ctx context.Context, merchantID, id string, in EndpointInput) (model.WebhookEndpoint, error) {
	var v apperr.Validator
	s.validateEndpoint(&v, in, false)
	if err := v.Err(); err != nil {
		return model.WebhookEndpoint{}, err
	}
	var out model.WebhookEndpoint
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		ep, err := q.GetEndpoint(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ep.MerchantID != merchantID) {
			return apperr.NotFound("webhook endpoint", id)
		}
		if err != nil {
			return err
		}
		changed := map[string]any{}
		if in.URL != nil {
			ep.URL = strings.TrimSpace(*in.URL)
			changed["url"] = ep.URL
		}
		if in.Description != nil {
			ep.Description = *in.Description
		}
		if in.Events != nil {
			ep.Events = dedupe(*in.Events)
			changed["events"] = ep.Events
		}
		if in.Enabled != nil {
			ep.Status = model.EndpointDisabled
			if *in.Enabled {
				ep.Status = model.EndpointEnabled
			}
			changed["status"] = ep.Status
		}
		ep.UpdatedAt = s.clock.Now()
		if err := q.UpdateEndpoint(ctx, ep); err != nil {
			return err
		}
		out = ep
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "webhook_endpoint.update", ResourceType: "webhook_endpoint", ResourceID: id, MerchantID: merchantID,
			Metadata: changed,
		})
	})
	return out, err
}

// RotateSecret issues a new signing secret. The previous secret keeps being
// included in signatures for the configured grace period.
func (s *Service) RotateSecret(ctx context.Context, merchantID, id string) (EndpointWithSecret, error) {
	var out EndpointWithSecret
	secret := newSecret()
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		ep, err := q.GetEndpoint(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ep.MerchantID != merchantID) {
			return apperr.NotFound("webhook endpoint", id)
		}
		if err != nil {
			return err
		}
		sealed, err := s.kr.SealString(secret, webhook.SecretAAD(ep.ID))
		if err != nil {
			return err
		}
		now := s.clock.Now()
		ep.PrevSecretSealed = ep.SecretSealed
		exp := now.Add(s.opts.SecretRotationGrace)
		ep.PrevSecretExpiresAt = &exp
		if s.opts.SecretRotationGrace <= 0 {
			ep.PrevSecretSealed, ep.PrevSecretExpiresAt = "", nil
		}
		ep.SecretSealed = sealed
		ep.UpdatedAt = now
		if err := q.UpdateEndpoint(ctx, ep); err != nil {
			return err
		}
		out = EndpointWithSecret{WebhookEndpoint: ep, Secret: secret}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "webhook_endpoint.rotate_secret", ResourceType: "webhook_endpoint", ResourceID: id,
			MerchantID: merchantID, Metadata: map[string]any{"grace_seconds": int64(s.opts.SecretRotationGrace.Seconds())},
		})
	})
	return out, err
}
