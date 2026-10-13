package payment

import (
	"sync"
	"testing"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

func TestTransitionTable(t *testing.T) {
	allowed := map[model.PaymentStatus][]Action{
		model.StatusCreated:           {ActAuthorize, ActCancel},
		model.StatusRequiresAction:    {ActComplete3DS, ActCancel},
		model.StatusRequiresReview:    {ActReviewApprove, ActReviewReject, ActCancel},
		model.StatusAuthorized:        {ActCapture, ActVoid, ActExpire},
		model.StatusCaptured:          {ActRefund},
		model.StatusPartiallyRefunded: {ActRefund},
		model.StatusDeclined:          {ActAuthorize, ActCancel},
		model.StatusRefunded:          nil,
		model.StatusVoided:            nil,
		model.StatusCanceled:          nil,
		model.StatusBlocked:           nil,
	}
	for status, want := range allowed {
		got := AllowedActions(status)
		if len(got) != len(want) {
			t.Errorf("%s: allowed %v, want %v", status, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: allowed %v, want %v", status, got, want)
			}
		}
		if Terminal(status) != (len(want) == 0) {
			t.Errorf("%s: Terminal = %v", status, Terminal(status))
		}
	}
}

func TestCheckTransition(t *testing.T) {
	if err := CheckTransition(model.StatusAuthorized, ActCapture, model.StatusCaptured); err != nil {
		t.Fatal(err)
	}
	if err := CheckTransition(model.StatusDeclined, ActAuthorize, model.StatusDeclined); err != nil {
		t.Fatalf("retrying a declined payment may decline again: %v", err)
	}
	if err := CheckTransition(model.StatusPartiallyRefunded, ActRefund, model.StatusPartiallyRefunded); err != nil {
		t.Fatalf("multiple partial refunds: %v", err)
	}
	err := CheckTransition(model.StatusCaptured, ActVoid, model.StatusVoided)
	if !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("void of captured: %v", err)
	}
	err = CheckTransition(model.StatusAuthorized, ActCapture, model.StatusRefunded)
	if !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("capture to refunded: %v", err)
	}
	if CanApply(model.StatusCreated, Action("teleport")) {
		t.Fatal("unknown action allowed")
	}
}

func TestKeyedMutex(t *testing.T) {
	k := newKeyedMutex()
	var wg sync.WaitGroup
	counter := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.Lock("pi_1")
			counter++ // protected by the keyed lock
			unlock()
		}()
	}
	wg.Wait()
	if counter != 50 {
		t.Fatalf("counter = %d", counter)
	}
	if k.size() != 0 {
		t.Fatalf("lock entries leaked: %d", k.size())
	}
	a := k.Lock("a")
	b := k.Lock("b") // different keys do not block each other
	a()
	b()
}
