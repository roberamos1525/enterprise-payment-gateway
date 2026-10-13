package acquirer

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
)

var day = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

type fixture struct {
	db    *store.DB
	vault *vault.Service
	acq   *Acquirer
	clk   *clock.Fake
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "acq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Q().InsertMerchant(ctx, model.Merchant{ID: "mer_1", Name: "m", Email: "m@x.test", Country: "US",
		DefaultCurrency: "USD", Status: model.MerchantActive, CreatedAt: day, UpdatedAt: day}); err != nil {
		t.Fatal(err)
	}
	kr, _ := vault.NewKeyring([]byte("fingerprint-secret-0123"), vault.NamedKey{ID: "k1", Key: bytes.Repeat([]byte{3}, 32)})
	clk := clock.NewFake(day)
	v := vault.NewService(kr, clk)
	return &fixture{db: db, vault: v, acq: New(db, v, clk, cfg, nil), clk: clk}
}

func (f *fixture) card(t *testing.T, pan string) model.VaultCard {
	t.Helper()
	cvc := "123"
	if card.DetectBrand(pan) == card.Amex {
		cvc = "1234"
	}
	c, err := card.Validate(card.Input{Number: pan, ExpMonth: 12, ExpYear: 2030, CVC: cvc}, day)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := f.vault.Tokenize(context.Background(), f.db.Q(), "mer_1", c)
	if err != nil {
		t.Fatal(err)
	}
	return vc
}

func (f *fixture) authorize(t *testing.T, pan string, amount int64, threeDS bool) AuthResponse {
	t.Helper()
	resp, err := f.acq.Authorize(context.Background(), AuthRequest{MerchantID: "mer_1", PaymentID: "pi_1",
		Card: f.card(t, pan), Amount: amount, Currency: "USD", ThreeDSCompleted: threeDS})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthorizeScenarios(t *testing.T) {
	f := newFixture(t, Config{})
	cases := []struct {
		pan, decline string
		approved     bool
	}{
		{"4242424242424242", "", true},
		{"4111111111111111", "", true}, // not in the catalog: approve
		{"4000000000000002", DeclineDoNotHonor, false},
		{"4000000000009995", DeclineInsufficientFunds, false},
		{"4000000000000069", DeclineExpiredCard, false},
		{"4000000000000119", DeclineProcessingError, false},
	}
	for _, c := range cases {
		r := f.authorize(t, c.pan, 1000, false)
		if r.Approved != c.approved || r.DeclineCode != c.decline || r.Ref == "" {
			t.Errorf("%s: %+v", c.pan, r)
		}
		if c.approved && len(r.AuthCode) != 6 {
			t.Errorf("%s: auth code %q", c.pan, r.AuthCode)
		}
		if !c.approved && r.Message != DeclineMessage(c.decline) {
			t.Errorf("%s: message %q", c.pan, r.Message)
		}
	}
	if r := f.authorize(t, "4000000000003220", 1000, false); !r.Requires3DS || r.Ref != "" {
		t.Fatalf("3ds required: %+v", r)
	}
	if r := f.authorize(t, "4000000000003220", 1000, true); !r.Approved {
		t.Fatalf("3ds completed: %+v", r)
	}
	if r := f.authorize(t, "4000008400001629", 1000, true); r.Approved || r.DeclineCode != DeclineDoNotHonor {
		t.Fatalf("3ds then decline: %+v", r)
	}
	if authCode("acq_x") != authCode("acq_x") || authCode("acq_x") == authCode("acq_y") {
		t.Fatal("auth codes must be deterministic per reference")
	}
}

func TestCaptureVoidRefundBookkeeping(t *testing.T) {
	f := newFixture(t, Config{})
	ctx := context.Background()
	auth := f.authorize(t, "4242424242424242", 5000, false)
	if _, err := f.acq.Capture(ctx, auth.Ref, 6000); !errors.Is(err, ErrOverCapture) {
		t.Fatalf("over-capture: %v", err)
	}
	capRef, err := f.acq.Capture(ctx, auth.Ref, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.acq.Capture(ctx, auth.Ref, 1001); !errors.Is(err, ErrOverCapture) {
		t.Fatalf("second capture beyond remaining: %v", err)
	}
	if _, err := f.acq.Void(ctx, auth.Ref); err == nil {
		t.Fatal("void after capture should fail")
	}
	vc := f.card(t, "4242424242424242")
	r1, err := f.acq.Refund(ctx, capRef, vc, 3000)
	if err != nil || !r1.Approved {
		t.Fatalf("refund: %+v %v", r1, err)
	}
	if _, err := f.acq.Refund(ctx, capRef, vc, 1001); !errors.Is(err, ErrOverRefund) {
		t.Fatalf("over-refund: %v", err)
	}
	if _, err := f.acq.Refund(ctx, auth.Ref, vc, 1); !errors.Is(err, ErrUnknownRef) {
		t.Fatalf("refund against authorization ref: %v", err)
	}
	if _, err := f.acq.Capture(ctx, "acq_missing", 1); !errors.Is(err, ErrUnknownRef) {
		t.Fatalf("unknown ref: %v", err)
	}
	declined := f.authorize(t, "4000000000000002", 100, false)
	if _, err := f.acq.Capture(ctx, declined.Ref, 100); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("capture of declined: %v", err)
	}
	a2 := f.authorize(t, "5555555555554444", 700, false)
	if _, err := f.acq.Void(ctx, a2.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.acq.Capture(ctx, a2.Ref, 700); !errors.Is(err, ErrAlreadyFinal) {
		t.Fatalf("capture after void: %v", err)
	}

	// Refund-failure scenario card.
	failCard := f.card(t, "4000000000005126")
	a3 := f.authorize(t, "4000000000005126", 900, false)
	c3, _ := f.acq.Capture(ctx, a3.Ref, 900)
	r3, err := f.acq.Refund(ctx, c3, failCard, 100)
	if err != nil || r3.Approved || r3.DeclineCode != DeclineRefundFailed {
		t.Fatalf("refund failure scenario: %+v %v", r3, err)
	}
}

func TestLatencyAndTimeout(t *testing.T) {
	f := newFixture(t, Config{BaseLatency: time.Millisecond, SlowLatency: 2 * time.Second})
	var slept []time.Duration
	f.acq.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		if d > time.Second {
			return context.DeadlineExceeded
		}
		return nil
	}
	f.authorize(t, "4242424242424242", 100, false)
	_, err := f.acq.Authorize(context.Background(), AuthRequest{MerchantID: "mer_1", Card: f.card(t, "4000000000000341"),
		Amount: 100, Currency: "USD"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow card should time out, got %v", err)
	}
	if len(slept) != 2 || slept[0] != time.Millisecond || slept[1] != 2*time.Second+time.Millisecond {
		t.Fatalf("slept = %v", slept)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx ignores cancellation: %v", err)
	}
}

func TestSettlementFileAndPerturbation(t *testing.T) {
	f := newFixture(t, Config{})
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		a := f.authorize(t, "4242424242424242", int64(1000+i), false)
		c, err := f.acq.Capture(ctx, a.Ref, int64(1000+i))
		if err != nil {
			t.Fatal(err)
		}
		if i%5 == 0 {
			if _, err := f.acq.Refund(ctx, c, f.card(t, "4242424242424242"), 10); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := f.acq.SettlementFile(ctx, day)
	if err != nil || len(rows) != 24 {
		t.Fatalf("faithful file has %d rows, %v", len(rows), err)
	}
	refunds := 0
	for _, r := range rows {
		if r.RecordType == RecordRefund {
			refunds++
		}
	}
	if refunds != 4 {
		t.Fatalf("refund rows = %d", refunds)
	}
	other, _ := f.acq.SettlementFile(ctx, day.AddDate(0, 0, 1))
	if len(other) != 0 {
		t.Fatal("next day should be empty")
	}

	f.acq.cfg = Config{DiscrepancyRate: 0.5, DiscrepancySeed: "s1"}
	p1, _ := f.acq.SettlementFile(ctx, day)
	p2, _ := f.acq.SettlementFile(ctx, day)
	if len(p1) != len(p2) {
		t.Fatal("perturbation must be deterministic")
	}
	same := len(p1) == len(rows)
	for i := 0; same && i < len(rows); i++ {
		same = p1[i] == rows[i]
	}
	if same {
		t.Fatal("perturbation had no effect at rate 0.5")
	}
}

func TestCSVRoundTripAndValidation(t *testing.T) {
	rows := []FileRow{
		{RecordType: RecordCapture, Ref: "acq_1", ParentRef: "acq_0", MerchantID: "mer_1", Amount: 1234, Currency: "EUR", Time: day},
		{RecordType: RecordRefund, Ref: "acq_2", ParentRef: "acq_1", MerchantID: "mer_1", Amount: 34, Currency: "EUR", Time: day.Add(time.Hour)},
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCSV(&buf)
	if err != nil || len(parsed) != 2 || parsed[0] != rows[0] || parsed[1] != rows[1] {
		t.Fatalf("round trip: %+v %v", parsed, err)
	}
	header := strings.Join(FileHeader, ",") + "\n"
	bad := map[string]string{
		"wrong header":   "a,b,c,d,e,f,g\n",
		"bad type":       header + "SALE,acq_1,,m,1,USD,2026-03-02T00:00:00Z\n",
		"zero amount":    header + "CAPTURE,acq_1,,m,0,USD,2026-03-02T00:00:00Z\n",
		"bad currency":   header + "CAPTURE,acq_1,,m,1,XYZ,2026-03-02T00:00:00Z\n",
		"bad time":       header + "CAPTURE,acq_1,,m,1,USD,yesterday\n",
		"missing ref":    header + "CAPTURE,,,m,1,USD,2026-03-02T00:00:00Z\n",
		"field count":    header + "CAPTURE,acq_1\n",
		"contains a PAN": header + "CAPTURE,acq_1,,4000000000000002,1,USD,2026-03-02T00:00:00Z\n",
		"empty":          "",
	}
	for name, in := range bad {
		_, err := ParseCSV(strings.NewReader(in))
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Errorf("%s: expected ParseError, got %v", name, err)
		}
	}
	_, err = ParseCSV(strings.NewReader(header + "CAPTURE,acq_1,,m,1,USD,bad\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error should name the line: %v", err)
	}
}
