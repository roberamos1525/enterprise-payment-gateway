package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestStatusMappingAndHelpers(t *testing.T) {
	cases := map[*Error]int{
		New(CodeValidation, "x"):          http.StatusUnprocessableEntity,
		NotFound("payment", "pi_1"):       http.StatusNotFound,
		InvalidState("payment", "a", "b"): http.StatusConflict,
		Declined("do_not_honor", "no"):    http.StatusPaymentRequired,
		New(CodeRateLimited, "slow"):      http.StatusTooManyRequests,
		Internal(errors.New("db")):        http.StatusInternalServerError,
		{Code: Code("weird")}:             http.StatusInternalServerError,
	}
	for e, want := range cases {
		if e.HTTPStatus() != want {
			t.Errorf("%s: status %d, want %d", e.Code, e.HTTPStatus(), want)
		}
	}
	cause := errors.New("disk full")
	w := Wrap(cause, CodeUnavailable, "try %s", "later")
	wrapped := fmt.Errorf("outer: %w", w)
	if !errors.Is(wrapped, cause) || CodeOf(wrapped) != CodeUnavailable || !Is(wrapped, CodeUnavailable) {
		t.Fatal("wrapping lost information")
	}
	if !strings.Contains(w.Error(), "disk full") || w.Message != "try later" {
		t.Fatalf("message = %q / %q", w.Error(), w.Message)
	}
	if CodeOf(errors.New("plain")) != CodeInternal || Is(nil, CodeInternal) {
		t.Fatal("untyped errors are internal; nil is nothing")
	}
	if From(errors.New("plain")).Message != "internal error" {
		t.Fatal("From must hide untyped messages")
	}
}

func TestValidator(t *testing.T) {
	var v Validator
	v.Required("name", "  ")
	v.MaxLen("desc", "héllo", 4)
	v.OneOf("mode", "x", "a", "b")
	v.Range("n", 11, 1, 10)
	v.Email("email", "Bob <bob@x.test>")
	v.Country("country", "us")
	v.Metadata("metadata", map[string]string{"": "v", "k": strings.Repeat("x", 501)})
	if v.Valid() {
		t.Fatal("expected errors")
	}
	e, ok := As(v.Err())
	if !ok || e.Code != CodeValidation || len(e.Fields) != 8 {
		t.Fatalf("fields = %+v", e)
	}
	var ok2 Validator
	ok2.Required("name", "x")
	ok2.Email("email", "bob@x.test")
	ok2.Country("country", "US")
	if ok2.Err() != nil {
		t.Fatalf("valid input rejected: %v", ok2.Err())
	}
	var merged Validator
	merged.Merge(&v)
	if len(merged.fields) != 8 {
		t.Fatal("merge lost fields")
	}
}
