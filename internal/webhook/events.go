// Package webhook implements event notifications: a transactional outbox,
// HMAC-SHA256 signing, an SSRF-guarded HTTP dispatcher with exponential
// backoff, delivery logs and replay.
package webhook

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
)

// Event types emitted by the gateway.
const (
	EventPaymentCreated        = "payment_intent.created"
	EventPaymentRequiresAction = "payment_intent.requires_action"
	EventPaymentRequiresReview = "payment_intent.requires_review"
	EventPaymentAuthorized     = "payment_intent.authorized"
	EventPaymentCaptured       = "payment_intent.captured"
	EventPaymentDeclined       = "payment_intent.declined"
	EventPaymentBlocked        = "payment_intent.blocked"
	EventPaymentVoided         = "payment_intent.voided"
	EventPaymentCanceled       = "payment_intent.canceled"
	EventRefundSucceeded       = "refund.succeeded"
	EventRefundFailed          = "refund.failed"
	EventSettlementCreated     = "settlement.created"
	EventPayoutScheduled       = "payout.scheduled"
	EventPayoutPaid            = "payout.paid"
	EventPing                  = "webhook_endpoint.ping"
)

var knownEvents = map[string]bool{
	EventPaymentCreated: true, EventPaymentRequiresAction: true, EventPaymentRequiresReview: true,
	EventPaymentAuthorized: true, EventPaymentCaptured: true, EventPaymentDeclined: true,
	EventPaymentBlocked: true, EventPaymentVoided: true, EventPaymentCanceled: true,
	EventRefundSucceeded: true, EventRefundFailed: true, EventSettlementCreated: true,
	EventPayoutScheduled: true, EventPayoutPaid: true, EventPing: true,
}

// KnownEvent reports whether t is an event type (or the "*" wildcard).
func KnownEvent(t string) bool { return t == "*" || knownEvents[t] }

// EventTypes returns all event types, sorted.
func EventTypes() []string {
	out := make([]string, 0, len(knownEvents))
	for t := range knownEvents {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// URLPolicy controls which webhook URLs are acceptable. The defaults are the
// safe choice: HTTPS only and no private, loopback or link-local targets.
type URLPolicy struct {
	AllowInsecureHTTP    bool
	AllowPrivateNetworks bool
}

// ValidateURL checks a webhook URL against the policy. Hostnames are checked
// again at dial time (see SafeDialer) so DNS rebinding cannot bypass it.
func ValidateURL(raw string, p URLPolicy) error {
	if len(raw) > 2048 {
		return fmt.Errorf("must be at most 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("must be an absolute URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecureHTTP {
			return fmt.Errorf("must use https")
		}
	default:
		return fmt.Errorf("scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("must not contain credentials")
	}
	if u.Fragment != "" {
		return fmt.Errorf("must not contain a fragment")
	}
	host := u.Hostname()
	if p.AllowPrivateNetworks {
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("must not target localhost")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !PublicAddr(ip) {
		return fmt.Errorf("must not target a private or reserved address")
	}
	return nil
}

// PublicAddr reports whether ip is a globally routable unicast address.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64
}

// dialGuard rejects connections to non-public addresses after DNS
// resolution; it is installed as net.Dialer.Control.
func dialGuard(allowPrivate bool) func(network, address string) error {
	return func(network, address string) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("webhook: refusing to dial unresolved address %q", host)
		}
		if !PublicAddr(ip) {
			return fmt.Errorf("webhook: refusing to dial non-public address %s", ip)
		}
		return nil
	}
}
