// Package ids generates prefixed, lexicographically time-ordered identifiers
// (e.g. "pi_01hx...") and random secrets.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Crockford-style lowercase alphabet; preserves byte order under encoding.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var enc = base32.NewEncoding(alphabet).WithPadding(base32.NoPadding)

// Generator produces monotonic IDs: 48-bit millisecond timestamp followed by
// 80 bits of randomness. Within the same millisecond the random part is
// incremented so IDs from one generator always sort in creation order.
type Generator struct {
	mu     sync.Mutex
	lastMS uint64
	last   [10]byte
	now    func() time.Time
}

// NewGenerator creates a generator using the given time source (nil = time.Now).
func NewGenerator(now func() time.Time) *Generator {
	if now == nil {
		now = time.Now
	}
	return &Generator{now: now}
}

var defaultGen = NewGenerator(nil)

// New returns a new ID with the given prefix using the default generator.
func New(prefix string) string { return defaultGen.New(prefix) }

// New returns a new ID with the given prefix.
func (g *Generator) New(prefix string) string {
	g.mu.Lock()
	ms := uint64(g.now().UnixMilli())
	if ms <= g.lastMS {
		ms = g.lastMS
		incr(&g.last)
	} else {
		g.lastMS = ms
		if _, err := rand.Read(g.last[:]); err != nil {
			panic("ids: crypto/rand failed: " + err.Error())
		}
		g.last[0] &= 0x7f // leave headroom so increments rarely overflow
	}
	var raw [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(raw[:6], ts[2:])
	copy(raw[6:], g.last[:])
	g.mu.Unlock()
	return prefix + "_" + enc.EncodeToString(raw[:])
}

func incr(b *[10]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

// HasPrefix reports whether id looks like an ID with the given prefix.
func HasPrefix(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix+"_")
	return ok && len(rest) == 26 && strings.Trim(rest, alphabet) == ""
}

// RandomHex returns n random bytes hex-encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// RandomToken returns a URL-safe random string with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	return enc.EncodeToString(b)
}
