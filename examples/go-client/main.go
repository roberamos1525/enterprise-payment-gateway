// Command go-client demonstrates calling the gateway's merchant API from Go
// using only the standard library: tokenize a synthetic 3-D Secure test card,
// authorize, complete the simulated challenge, capture and refund.
//
//	GATEWAY_URL=http://127.0.0.1:8080 GATEWAY_API_KEY=sk_test_... go run ./examples/go-client
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type client struct {
	base, key string
	http      *http.Client
}

func (c client) call(method, path, idemKey string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}

type paymentIntent struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	AmountCaptured int64  `json:"amount_captured"`
	AmountRefunded int64  `json:"amount_refunded"`
	DeclineCode    string `json:"decline_code"`
	NextAction     *struct {
		ChallengeID string `json:"challenge_id"`
	} `json:"next_action"`
}

func main() {
	c := client{base: envOr("GATEWAY_URL", "http://127.0.0.1:8080"), key: os.Getenv("GATEWAY_API_KEY"),
		http: &http.Client{Timeout: 15 * time.Second}}
	if c.key == "" {
		fmt.Fprintln(os.Stderr, "set GATEWAY_API_KEY to a merchant secret key")
		os.Exit(2)
	}
	if err := run(c); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(c client) error {
	var tok struct {
		Token  string `json:"token"`
		Masked string `json:"masked_number"`
	}
	card := map[string]any{"number": "4000000000003220", "exp_month": 12, "exp_year": 2030, "cvc": "123"}
	if err := c.call("POST", "/v1/tokens", "", map[string]any{"card": card}, &tok); err != nil {
		return err
	}
	fmt.Println("tokenized", tok.Masked, "as", tok.Token)

	var pi paymentIntent
	order := fmt.Sprintf("go-client-%d", time.Now().UnixNano())
	if err := c.call("POST", "/v1/payment_intents", order, map[string]any{
		"amount": 2599, "currency": "EUR", "confirm": true, "payment_method": map[string]any{"token": tok.Token},
	}, &pi); err != nil {
		return err
	}
	fmt.Println("created", pi.ID, "->", pi.Status)
	if pi.Status == "requires_action" && pi.NextAction != nil {
		if err := c.call("POST", "/v1/payment_intents/"+pi.ID+"/3ds", "", map[string]any{
			"challenge_id": pi.NextAction.ChallengeID, "outcome": "success"}, &pi); err != nil {
			return err
		}
		fmt.Println("3-D Secure completed ->", pi.Status)
	}
	if pi.Status != "authorized" {
		return fmt.Errorf("unexpected status %s (decline_code=%s)", pi.Status, pi.DeclineCode)
	}
	if err := c.call("POST", "/v1/payment_intents/"+pi.ID+"/capture", "", map[string]any{"amount": 2000}, &pi); err != nil {
		return err
	}
	fmt.Println("captured", pi.AmountCaptured, "->", pi.Status)
	var refund struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := c.call("POST", "/v1/payment_intents/"+pi.ID+"/refunds", "", map[string]any{"amount": 500}, &refund); err != nil {
		return err
	}
	fmt.Println("refund", refund.ID, "->", refund.Status)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
