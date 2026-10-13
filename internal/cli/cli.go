// Package cli implements gatewayctl, the operator command-line tool. Local
// commands (test cards, synthetic PANs, key generation, signature checks)
// need no server; the others call the admin API.
package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

const usage = `gatewayctl - operator tool for the simulated payment gateway (synthetic data only)

Local commands:
  testcards                         list the synthetic test cards and their scenarios
  gen-pan [-prefix P] [-length N] [-n COUNT]
                                    generate Luhn-valid synthetic PANs
  gen-key [-id ID]                  generate a vault key entry for GATEWAY_VAULT_KEYS
  verify-signature -secret S -header H -body FILE [-tolerance D]
                                    verify a webhook signature

Admin API commands (need -url/GATEWAY_URL and GATEWAY_ADMIN_TOKEN):
  merchant-create -name N -email E -country CC -currency CUR [-activate]
  key-create -merchant ID [-label L]
  settle -date YYYY-MM-DD [-merchant ID] [-open-day]
  reconcile -date YYYY-MM-DD [-file settlement.csv]
  acquirer-file -date YYYY-MM-DD    print the simulated acquirer settlement file
  audit-verify                      verify the audit hash chain
  job JOB                           run a background job now (e.g. expire_authorizations)
  get PATH                          GET any admin path, e.g. /admin/v1/merchants
`

// Env abstracts the process environment for testing.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	HTTP   *http.Client
}

// Run executes gatewayctl with args and returns the process exit code.
func Run(args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.HTTP == nil {
		env.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(env.Stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "testcards":
		err = testCards(env)
	case "gen-pan":
		err = genPAN(rest, env)
	case "gen-key":
		err = genKey(rest, env)
	case "verify-signature":
		err = verifySignature(rest, env)
	case "merchant-create", "key-create", "settle", "reconcile", "acquirer-file", "audit-verify", "job", "get":
		err = adminCommand(cmd, rest, env)
	default:
		fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(env.Stderr, "usage error:", err)
			return 2
		}
		fmt.Fprintln(env.Stderr, "error:", err)
		return 1
	}
	return 0
}

type usageError string

func (u usageError) Error() string { return string(u) }

func newFlags(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	return nil
}

func testCards(env Env) error {
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NUMBER\tBRAND\tCOUNTRY\tSCENARIO\tDESCRIPTION")
	for _, c := range card.TestCards() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.PAN, c.Brand, c.Country, c.Scenario, c.Description)
	}
	return tw.Flush()
}

func genPAN(args []string, env Env) error {
	fs := newFlags("gen-pan", env)
	prefix := fs.String("prefix", "400000", "BIN prefix (use reserved test ranges only)")
	length := fs.Int("length", 16, "total PAN length")
	n := fs.Int("n", 1, "how many to generate")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *n < 1 || *n > 1000 {
		return usageError("-n must be between 1 and 1000")
	}
	for i := 0; i < *n; i++ {
		pan, err := card.GeneratePAN(*prefix, *length, nil)
		if err != nil {
			return usageError(err.Error())
		}
		fmt.Fprintf(env.Stdout, "%s\t%s\n", pan, card.DetectBrand(pan))
	}
	return nil
}

func genKey(args []string, env Env) error {
	fs := newFlags("gen-key", env)
	id := fs.String("id", "k"+time.Now().UTC().Format("20060102"), "key id")
	if err := parse(fs, args); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s:%s\n", *id, base64.StdEncoding.EncodeToString(vault.GenerateKey()))
	return nil
}

func verifySignature(args []string, env Env) error {
	fs := newFlags("verify-signature", env)
	secret := fs.String("secret", env.Getenv("WEBHOOK_SECRET"), "endpoint signing secret (or WEBHOOK_SECRET)")
	header := fs.String("header", "", "value of the Gateway-Signature header")
	bodyPath := fs.String("body", "", "file containing the raw request body")
	tolerance := fs.Duration("tolerance", 0, "maximum signature age (0 = do not check)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *secret == "" || *header == "" || *bodyPath == "" {
		return usageError("-secret, -header and -body are required")
	}
	body, err := os.ReadFile(*bodyPath)
	if err != nil {
		return err
	}
	if err := webhook.Verify(*header, body, *secret, *tolerance, time.Now()); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "signature valid")
	return nil
}

// --- admin API --------------------------------------------------------------------

type client struct {
	base  string
	token string
	http  *http.Client
}

func (c client) do(method, path, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(out, &env) == nil && env.Error.Type != "" {
			return nil, fmt.Errorf("%s %s: %d %s: %s", method, path, resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return nil, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return out, nil
}

func (c client) postJSON(path string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return c.do(http.MethodPost, path, "application/json", b)
}

func adminCommand(cmd string, args []string, env Env) error {
	fs := newFlags(cmd, env)
	base := fs.String("url", firstNonEmpty(env.Getenv("GATEWAY_URL"), "http://127.0.0.1:8080"), "gateway base URL")
	var name, email, country, currency, merchantID, label, date, file *string
	var activate, openDay *bool
	switch cmd {
	case "merchant-create":
		name = fs.String("name", "", "merchant name")
		email = fs.String("email", "", "contact email")
		country = fs.String("country", "US", "ISO country code")
		currency = fs.String("currency", "USD", "default currency")
		activate = fs.Bool("activate", false, "activate immediately")
	case "key-create":
		merchantID = fs.String("merchant", "", "merchant id")
		label = fs.String("label", "gatewayctl", "key label")
	case "settle", "reconcile", "acquirer-file":
		date = fs.String("date", time.Now().UTC().Format("2006-01-02"), "settlement day (UTC)")
		if cmd == "settle" {
			merchantID = fs.String("merchant", "", "only this merchant")
			openDay = fs.Bool("open-day", false, "allow settling the current day")
		}
		if cmd == "reconcile" {
			file = fs.String("file", "", "settlement CSV to upload (default: simulator file)")
		}
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	token := env.Getenv("GATEWAY_ADMIN_TOKEN")
	if token == "" {
		return usageError("GATEWAY_ADMIN_TOKEN must be set")
	}
	c := client{base: strings.TrimRight(*base, "/"), token: token, http: env.HTTP}
	var out []byte
	var err error
	switch cmd {
	case "merchant-create":
		out, err = c.postJSON("/admin/v1/merchants", map[string]any{"name": *name, "email": *email,
			"country": *country, "default_currency": *currency, "activate": *activate})
	case "key-create":
		if *merchantID == "" {
			return usageError("-merchant is required")
		}
		out, err = c.postJSON("/admin/v1/merchants/"+url.PathEscape(*merchantID)+"/api_keys", map[string]any{"label": *label})
	case "settle":
		out, err = c.postJSON("/admin/v1/settlements/run", map[string]any{"date": *date, "merchant_id": *merchantID,
			"allow_open_day": *openDay})
	case "reconcile":
		if *file == "" {
			out, err = c.postJSON("/admin/v1/reconciliations", map[string]any{"date": *date})
		} else {
			var body []byte
			if body, err = os.ReadFile(*file); err == nil {
				out, err = c.do(http.MethodPost, "/admin/v1/reconciliations/upload?date="+url.QueryEscape(*date), "text/csv", body)
			}
		}
	case "acquirer-file":
		out, err = c.do(http.MethodGet, "/admin/v1/acquirer/settlement_file?date="+url.QueryEscape(*date), "", nil)
		if err == nil {
			_, err = env.Stdout.Write(out)
			return err
		}
	case "audit-verify":
		out, err = c.do(http.MethodGet, "/admin/v1/audit_logs/verify", "", nil)
	case "job":
		if fs.NArg() != 1 {
			return usageError("job requires exactly one job name")
		}
		out, err = c.do(http.MethodPost, "/admin/v1/jobs/"+url.PathEscape(fs.Arg(0))+"/run", "", nil)
	case "get":
		if fs.NArg() != 1 || !strings.HasPrefix(fs.Arg(0), "/") {
			return usageError("get requires one absolute path")
		}
		out, err = c.do(http.MethodGet, fs.Arg(0), "", nil)
	}
	if err != nil {
		return err
	}
	return printJSON(env.Stdout, out)
}

func printJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = w.Write(raw)
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
