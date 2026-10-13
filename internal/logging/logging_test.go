package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestLoggerRedactsPANs(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.With("component", "test").Info("charging 4242424242424242 now",
		"card", "5555-5555-5555-4444", "err", errors.New("bad card 378282246310005"), "order", "12345")
	out := buf.String()
	for _, pan := range []string{"4242424242424242", "5555-5555-5555-4444", "378282246310005"} {
		if strings.Contains(out, pan) {
			t.Fatalf("log leaked %s: %s", pan, out)
		}
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "charging 424242******4242 now" || rec["order"] != "12345" || rec["component"] != "test" {
		t.Fatalf("record = %v", rec)
	}
	if ts, _ := rec["time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Fatalf("timestamp not UTC: %v", rec["time"])
	}
}

func TestLoggerOptions(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "warn", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.WithGroup("g").Warn("shown", "k", "v")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "g.k=v") {
		t.Fatalf("output = %s", buf.String())
	}
	if _, err := New(&buf, "loud", "json"); err == nil {
		t.Fatal("bad level accepted")
	}
	if _, err := New(&buf, "info", "xml"); err == nil {
		t.Fatal("bad format accepted")
	}
	Discard().Error("nothing")
}
