package money

import (
	"fmt"
	"math/big"
)

// Rounding selects how fractional minor units are resolved.
type Rounding int

const (
	// HalfEven rounds to nearest, ties to even (banker's rounding).
	HalfEven Rounding = iota
	// HalfUp rounds to nearest, ties away from zero.
	HalfUp
	// Down truncates toward zero.
	Down
)

// ParseRounding converts a config string into a Rounding mode.
func ParseRounding(s string) (Rounding, error) {
	switch s {
	case "", "half_even":
		return HalfEven, nil
	case "half_up":
		return HalfUp, nil
	case "down":
		return Down, nil
	}
	return 0, fmt.Errorf("money: unknown rounding mode %q", s)
}

func (r Rounding) String() string {
	switch r {
	case HalfUp:
		return "half_up"
	case Down:
		return "down"
	}
	return "half_even"
}

// ApplyBps returns amount * bps / 10_000 rounded with the given mode. It uses
// arbitrary precision so it cannot overflow for any int64 inputs; an error is
// returned only when the result itself does not fit in an int64.
func ApplyBps(amount, bps int64, mode Rounding) (int64, error) {
	num := new(big.Int).Mul(big.NewInt(amount), big.NewInt(bps))
	den := big.NewInt(10_000)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() != 0 && mode != Down {
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		cmp := twice.Cmp(den)
		roundAway := cmp > 0 || (cmp == 0 && (mode == HalfUp || q.Bit(0) == 1))
		if roundAway {
			if num.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// Fee computes a percentage-plus-fixed fee on an amount: amount*bps/10000 + fixed.
func Fee(amount, bps, fixed int64, mode Rounding) (int64, error) {
	pct, err := ApplyBps(amount, bps, mode)
	if err != nil {
		return 0, err
	}
	total, ok := AddInt64(pct, fixed)
	if !ok {
		return 0, ErrOverflow
	}
	return total, nil
}

// Sum adds a list of amounts that must share a currency. An empty list sums
// to zero in the given currency.
func Sum(currency string, amounts ...Amount) (Amount, error) {
	total := Zero(currency)
	for _, a := range amounts {
		var err error
		if total, err = total.Add(a); err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}
