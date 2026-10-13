package card

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"sort"
)

// Scenario is the deterministic behaviour the simulated acquirer applies to
// a test card.
type Scenario string

// Scenarios understood by the acquirer simulator.
const (
	ScenarioApprove           Scenario = "approve"
	ScenarioDecline           Scenario = "decline_do_not_honor"
	ScenarioInsufficientFunds Scenario = "decline_insufficient_funds"
	ScenarioLostCard          Scenario = "decline_lost_card"
	ScenarioStolenCard        Scenario = "decline_stolen_card"
	ScenarioExpiredCard       Scenario = "decline_expired_card"
	ScenarioIncorrectCVC      Scenario = "decline_incorrect_cvc"
	ScenarioProcessingError   Scenario = "processing_error"
	Scenario3DSApprove        Scenario = "3ds_required"
	Scenario3DSDecline        Scenario = "3ds_required_then_decline"
	ScenarioSlow              Scenario = "slow_network"
	ScenarioRefundFails       Scenario = "refund_fails"
)

// TestCard is a synthetic, publicly documented test PAN. None of these
// numbers can be charged on a real network.
type TestCard struct {
	PAN         string   `json:"number"`
	Brand       Brand    `json:"brand"`
	Country     string   `json:"country"`
	Funding     string   `json:"funding"`
	Scenario    Scenario `json:"scenario"`
	Description string   `json:"description"`
}

// testCards is the catalog of deterministic test cards.
var testCards = []TestCard{
	{"4242424242424242", Visa, "US", "credit", ScenarioApprove, "Visa: approved"},
	{"4000056655665556", Visa, "US", "debit", ScenarioApprove, "Visa debit: approved"},
	{"5555555555554444", Mastercard, "US", "credit", ScenarioApprove, "Mastercard: approved"},
	{"2223003122003222", Mastercard, "US", "credit", ScenarioApprove, "Mastercard 2-series: approved"},
	{"378282246310005", Amex, "US", "credit", ScenarioApprove, "American Express: approved"},
	{"6011111111111117", Discover, "US", "credit", ScenarioApprove, "Discover: approved"},
	{"3566002020360505", JCB, "JP", "credit", ScenarioApprove, "JCB: approved"},
	{"36227206271667", Diners, "US", "credit", ScenarioApprove, "Diners Club (14 digit): approved"},
	{"6200000000000005", UnionPay, "CN", "credit", ScenarioApprove, "UnionPay: approved"},
	{"4000000760000002", Visa, "BR", "credit", ScenarioApprove, "Visa issued in Brazil: approved"},
	{"4000008260000000", Visa, "GB", "credit", ScenarioApprove, "Visa issued in the UK: approved"},
	{"4000002760000016", Visa, "DE", "credit", ScenarioApprove, "Visa issued in Germany: approved"},
	{"4000000000000002", Visa, "US", "credit", ScenarioDecline, "Declined: do_not_honor"},
	{"4000000000009995", Visa, "US", "credit", ScenarioInsufficientFunds, "Declined: insufficient_funds"},
	{"4000000000009987", Visa, "US", "credit", ScenarioLostCard, "Declined: lost_card"},
	{"4000000000009979", Visa, "US", "credit", ScenarioStolenCard, "Declined: stolen_card"},
	{"4000000000000069", Visa, "US", "credit", ScenarioExpiredCard, "Declined: expired_card"},
	{"4000000000000127", Visa, "US", "credit", ScenarioIncorrectCVC, "Declined: incorrect_cvc"},
	{"4000000000000119", Visa, "US", "credit", ScenarioProcessingError, "Acquirer processing error"},
	{"4000000000003220", Visa, "US", "credit", Scenario3DSApprove, "3-D Secure challenge required; approved after success"},
	{"4000008400001629", Visa, "US", "credit", Scenario3DSDecline, "3-D Secure challenge required; declined afterwards"},
	{"4000000000000341", Visa, "US", "credit", ScenarioSlow, "Approved after extra simulated network latency"},
	{"4000000000005126", Visa, "US", "credit", ScenarioRefundFails, "Approved, but refunds fail at the acquirer"},
	{"4100000000000019", Visa, "US", "credit", ScenarioApprove, "Approved by the network; BIN 410000 is on the default fraud blocklist"},
}

var testCardIndex = func() map[string]TestCard {
	m := make(map[string]TestCard, len(testCards))
	for _, c := range testCards {
		m[c.PAN] = c
	}
	return m
}()

// LookupTestCard returns the catalog entry for a PAN, if any.
func LookupTestCard(pan string) (TestCard, bool) {
	c, ok := testCardIndex[pan]
	return c, ok
}

// TestCards returns a copy of the catalog sorted by scenario then PAN.
func TestCards() []TestCard {
	out := append([]TestCard(nil), testCards...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scenario != out[j].Scenario {
			return out[i].Scenario < out[j].Scenario
		}
		return out[i].PAN < out[j].PAN
	})
	return out
}

// IssuerCountry returns the simulated issuing country for a PAN. Catalog
// cards carry an explicit country; other PANs default by brand.
func IssuerCountry(pan string) string {
	if c, ok := testCardIndex[pan]; ok {
		return c.Country
	}
	switch DetectBrand(pan) {
	case JCB:
		return "JP"
	case UnionPay:
		return "CN"
	case Maestro:
		return "GB"
	}
	return "US"
}

// Funding returns the simulated funding type for a PAN.
func Funding(pan string) string {
	if c, ok := testCardIndex[pan]; ok {
		return c.Funding
	}
	return "credit"
}

// GeneratePAN builds a random Luhn-valid PAN that starts with prefix and has
// the given total length. Use reserved test prefixes only (e.g. "400000").
func GeneratePAN(prefix string, length int, rnd io.Reader) (string, error) {
	if !IsDigits(prefix) || length < 12 || length > 19 || len(prefix) >= length {
		return "", fmt.Errorf("card: invalid prefix %q or length %d", prefix, length)
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	buf := []byte(prefix)
	for len(buf) < length-1 {
		n, err := rand.Int(rnd, big.NewInt(10))
		if err != nil {
			return "", err
		}
		buf = append(buf, byte('0'+n.Int64()))
	}
	buf = append(buf, LuhnCheckDigit(string(buf)))
	return string(buf), nil
}
