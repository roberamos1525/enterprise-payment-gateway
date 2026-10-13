package fraud

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// GeoIP maps client IPs to countries. The simulator ships with the RFC 5737
// documentation ranges only; operators can add CIDRs via configuration.
// Real geolocation databases are intentionally out of scope.
type GeoIP struct {
	ranges []geoRange
}

type geoRange struct {
	prefix  netip.Prefix
	country string
}

// DefaultGeoRanges are synthetic mappings for documentation address blocks.
var DefaultGeoRanges = map[string]string{
	"192.0.2.0/24":    "US", // TEST-NET-1
	"198.51.100.0/24": "GB", // TEST-NET-2
	"203.0.113.0/24":  "DE", // TEST-NET-3
	"2001:db8::/32":   "FR", // IPv6 documentation
}

// NewGeoIP builds a resolver from "cidr" -> "CC" mappings.
func NewGeoIP(mappings map[string]string) (*GeoIP, error) {
	g := &GeoIP{}
	for cidr, cc := range mappings {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("fraud: bad geo cidr %q: %w", cidr, err)
		}
		cc = strings.ToUpper(cc)
		if len(cc) != 2 {
			return nil, fmt.Errorf("fraud: bad country code %q for %s", cc, cidr)
		}
		g.ranges = append(g.ranges, geoRange{prefix: p.Masked(), country: cc})
	}
	// Longest prefix first so more specific ranges win.
	sort.Slice(g.ranges, func(i, j int) bool { return g.ranges[i].prefix.Bits() > g.ranges[j].prefix.Bits() })
	return g, nil
}

// Country returns the country for ip, or "" when unknown or unparsable.
func (g *GeoIP) Country(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	for _, r := range g.ranges {
		if r.prefix.Contains(addr) {
			return r.country
		}
	}
	return ""
}
