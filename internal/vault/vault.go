package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Service tokenizes cards. Tokens are scoped to the merchant that created them.
type Service struct {
	kr    *Keyring
	clock clock.Clock
}

// NewService builds a tokenization service.
func NewService(kr *Keyring, clk clock.Clock) *Service {
	return &Service{kr: kr, clock: clk}
}

// Keyring exposes the underlying keyring (for sealing other secrets).
func (s *Service) Keyring() *Keyring { return s.kr }

func cardAAD(token, merchantID string) []byte { return []byte(token + "|" + merchantID) }

// Tokenize seals a validated card and stores it, returning the vault record
// (which holds only the ciphertext, never the PAN). The CVC is not stored.
func (s *Service) Tokenize(ctx context.Context, q *store.Queries, merchantID string, c card.Validated) (model.VaultCard, error) {
	token := ids.New("tok")
	kid, sealed, err := s.kr.Seal([]byte(c.PAN), cardAAD(token, merchantID))
	if err != nil {
		return model.VaultCard{}, fmt.Errorf("vault: seal: %w", err)
	}
	vc := model.VaultCard{
		Token:       token,
		MerchantID:  merchantID,
		Fingerprint: s.kr.Fingerprint(c.PAN),
		KeyID:       kid,
		Ciphertext:  sealed,
		Brand:       string(c.Brand),
		BIN:         card.BIN(c.PAN),
		Last4:       card.Last4(c.PAN),
		ExpMonth:    c.ExpMonth,
		ExpYear:     c.ExpYear,
		Country:     card.IssuerCountry(c.PAN),
		Funding:     card.Funding(c.PAN),
		CreatedAt:   s.clock.Now(),
	}
	if err := q.InsertCard(ctx, vc); err != nil {
		return model.VaultCard{}, fmt.Errorf("vault: store card: %w", err)
	}
	return vc, nil
}

// Lookup returns the vault record for a merchant's token without revealing the PAN.
func (s *Service) Lookup(ctx context.Context, q *store.Queries, merchantID, token string) (model.VaultCard, error) {
	vc, err := q.GetCard(ctx, token)
	if errors.Is(err, store.ErrNotFound) || (err == nil && vc.MerchantID != merchantID) {
		// Tokens of other merchants are indistinguishable from missing ones.
		return model.VaultCard{}, apperr.NotFound("card token", token)
	}
	if err != nil {
		return model.VaultCard{}, err
	}
	return vc, nil
}

// Reveal decrypts the PAN of a vault card. Only the acquirer adapter may
// call this; the result must never be logged, persisted or returned.
func (s *Service) Reveal(vc model.VaultCard) (string, error) {
	pt, err := s.kr.Open(vc.KeyID, vc.Ciphertext, cardAAD(vc.Token, vc.MerchantID))
	if err != nil {
		return "", fmt.Errorf("vault: reveal %s: %w", vc.Token, err)
	}
	return string(pt), nil
}

// RewrapResult summarises a key-rotation pass.
type RewrapResult struct {
	PrimaryKey string `json:"primary_key"`
	Rewrapped  int    `json:"rewrapped"`
	Failed     int    `json:"failed"`
}

// Rewrap re-encrypts every card sealed with a non-primary key under the
// primary key, in batches, each batch in its own transaction.
func (s *Service) Rewrap(ctx context.Context, db *store.DB, batch int) (RewrapResult, error) {
	if batch <= 0 {
		batch = 100
	}
	res := RewrapResult{PrimaryKey: s.kr.PrimaryID()}
	failedTokens := map[string]bool{}
	for {
		var progressed bool
		err := db.Tx(ctx, func(q *store.Queries) error {
			cards, err := q.ListCardsNotUsingKey(ctx, s.kr.PrimaryID(), batch+len(failedTokens))
			if err != nil {
				return err
			}
			for _, vc := range cards {
				if failedTokens[vc.Token] {
					continue
				}
				pan, err := s.Reveal(vc)
				if err != nil {
					failedTokens[vc.Token] = true
					res.Failed++
					continue
				}
				kid, sealed, err := s.kr.Seal([]byte(pan), cardAAD(vc.Token, vc.MerchantID))
				if err != nil {
					return err
				}
				if err := q.UpdateCardCiphertext(ctx, vc.Token, kid, sealed); err != nil {
					return err
				}
				res.Rewrapped++
				progressed = true
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		if !progressed {
			return res, nil
		}
	}
}
