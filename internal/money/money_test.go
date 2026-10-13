package money

import (
	"errors"
	"math"
	"testing"
)

func TestNewAndNormalize(t *testing.T) {
	a, err := New(1234, "usd")
	if err != nil || a.Currency != "USD" || a.Minor != 1234 {
		t.Fatalf("New = %+v, %v", a, err)
	}
	if _, err := New(1, "XXX"); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("expected unknown currency, got %v", err)
	}
}

func TestAddSubCmp(t *testing.T) {
	a := Must(1000, "EUR")
	b := Must(250, "EUR")
	s, err := a.Add(b)
	if err != nil || s.Minor != 1250 {
		t.Fatalf("add = %v %v", s, err)
	}
	d, err := a.Sub(b)
	if err != nil || d.Minor != 750 {
		t.Fatalf("sub = %v %v", d, err)
	}
	if c, _ := a.Cmp(b); c != 1 {
		t.Fatal("cmp wrong")
	}
	if c, _ := b.Cmp(a); c != -1 {
		t.Fatal("cmp wrong")
	}
	if c, _ := a.Cmp(a); c != 0 {
		t.Fatal("cmp wrong")
	}
	if _, err := a.Add(Must(1, "USD")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatal("expected mismatch")
	}
	if _, err := a.Cmp(Must(1, "USD")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatal("expected mismatch")
	}
	if _, err := Must(math.MaxInt64, "USD").Add(Must(1, "USD")); !errors.Is(err, ErrOverflow) {
		t.Fatal("expected overflow")
	}
	if _, err := Must(0, "USD").Sub(Amount{Minor: math.MinInt64, Currency: "USD"}); !errors.Is(err, ErrOverflow) {
		t.Fatal("expected overflow on MinInt64 negation")
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		minor int64
		cur   string
		want  string
	}{
		{1234, "USD", "12.34"},
		{5, "USD", "0.05"},
		{-5, "USD", "-0.05"},
		{1234, "JPY", "1234"},
		{1, "KWD", "0.001"},
		{0, "EUR", "0.00"},
		{math.MinInt64, "USD", "-92233720368547758.08"},
	}
	for _, c := range cases {
		if got := FormatMinor(c.minor, c.cur); got != c.want {
			t.Errorf("FormatMinor(%d,%s) = %s, want %s", c.minor, c.cur, got, c.want)
		}
	}
	if Must(1999, "GBP").String() != "19.99 GBP" {
		t.Fatal("String() wrong")
	}
}

func TestParseDecimal(t *testing.T) {
	ok := map[string]int64{"12.34": 1234, "12.3": 1230, "12": 1200, "-0.05": -5, "0.00": 0}
	for in, want := range ok {
		a, err := ParseDecimal(in, "USD")
		if err != nil || a.Minor != want {
			t.Errorf("ParseDecimal(%q) = %v, %v; want %d", in, a, err, want)
		}
	}
	for _, bad := range []string{"12.345", "1.", ".5", "1e3", "abc", "", "99999999999999999999"} {
		if _, err := ParseDecimal(bad, "USD"); err == nil {
			t.Errorf("ParseDecimal(%q) should fail", bad)
		}
	}
	if a, err := ParseDecimal("500", "JPY"); err != nil || a.Minor != 500 {
		t.Fatalf("JPY parse = %v %v", a, err)
	}
	if _, err := ParseDecimal("5.5", "JPY"); err == nil {
		t.Fatal("JPY has no minor units")
	}
}

func TestApplyBpsRounding(t *testing.T) {
	cases := []struct {
		amount, bps int64
		mode        Rounding
		want        int64
	}{
		{10000, 290, HalfEven, 290},
		{1050, 1000, HalfEven, 105},
		{25, 2000, HalfEven, 5},
		{125, 2000, HalfEven, 25},
		{5, 1000, HalfEven, 0},  // 0.5 -> 0 (even)
		{15, 1000, HalfEven, 2}, // 1.5 -> 2 (even)
		{5, 1000, HalfUp, 1},    // 0.5 -> 1
		{-5, 1000, HalfUp, -1},  // -0.5 -> -1
		{-15, 1000, HalfEven, -2},
		{19, 1000, Down, 1},
		{-19, 1000, Down, -1},
		{999, 290, HalfEven, 29}, // 28.971 -> 29
	}
	for _, c := range cases {
		got, err := ApplyBps(c.amount, c.bps, c.mode)
		if err != nil || got != c.want {
			t.Errorf("ApplyBps(%d,%d,%s) = %d,%v want %d", c.amount, c.bps, c.mode, got, err, c.want)
		}
	}
	if _, err := ApplyBps(math.MaxInt64, 20000, HalfEven); !errors.Is(err, ErrOverflow) {
		t.Fatal("expected overflow")
	}
}

func TestFeeAndSum(t *testing.T) {
	fee, err := Fee(10000, 290, 30, HalfEven)
	if err != nil || fee != 320 {
		t.Fatalf("fee = %d %v", fee, err)
	}
	total, err := Sum("USD", Must(1, "USD"), Must(2, "USD"))
	if err != nil || total.Minor != 3 {
		t.Fatalf("sum = %v %v", total, err)
	}
	if _, err := Sum("USD", Must(1, "EUR")); err == nil {
		t.Fatal("expected mismatch")
	}
	for _, s := range []string{"half_even", "half_up", "down", ""} {
		if _, err := ParseRounding(s); err != nil {
			t.Fatalf("ParseRounding(%q): %v", s, err)
		}
	}
	if _, err := ParseRounding("ceil"); err == nil {
		t.Fatal("expected error")
	}
}
