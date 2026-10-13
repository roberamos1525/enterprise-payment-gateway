package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeClock(t *testing.T) {
	start := time.Date(2026, 3, 2, 23, 59, 0, 0, time.FixedZone("X", 3600))
	f := NewFake(start)
	if f.Now().Location() != time.UTC || !f.Now().Equal(start) {
		t.Fatalf("fake clock must normalise to UTC: %v", f.Now())
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.Advance(time.Minute) }()
	}
	wg.Wait()
	if got := f.Now().Sub(start.UTC()); got != 10*time.Minute {
		t.Fatalf("advanced %s", got)
	}
	f.Set(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if f.Now().Year() != 2030 {
		t.Fatal("Set ignored")
	}
	if (Real{}).Now().Location() != time.UTC {
		t.Fatal("real clock must be UTC")
	}
}

func TestDays(t *testing.T) {
	d := StartOfDay(time.Date(2026, 3, 2, 22, 30, 0, 0, time.FixedZone("Y", -5*3600)))
	if d != time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("start of day uses UTC: %v", d)
	}
	p, err := ParseDate("2026-02-28")
	if err != nil || p.Format(DateLayout) != "2026-02-28" || p.Location() != time.UTC {
		t.Fatalf("ParseDate = %v %v", p, err)
	}
	if _, err := ParseDate("2026-02-30"); err == nil {
		t.Fatal("invalid date accepted")
	}
}
