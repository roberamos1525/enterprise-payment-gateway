// Package vault seals sensitive values with AES-256-GCM and implements PAN
// tokenization. The raw PAN only ever exists in memory: it is sealed before
// it is written and only revealed to the simulated acquirer.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Errors returned by the keyring.
var (
	ErrUnknownKey = errors.New("vault: ciphertext sealed with unknown key")
	ErrDecrypt    = errors.New("vault: decryption failed")
	ErrMalformed  = errors.New("vault: malformed sealed value")
)

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Keyring holds one primary data-encryption key used for sealing and any
// number of older keys that can still open existing ciphertexts, which makes
// key rotation possible without downtime.
type Keyring struct {
	primary string
	aeads   map[string]cipher.AEAD
	order   []string
	fpKey   []byte
}

// ParseKeyring parses "id:base64key[,id:base64key...]" (first = primary) and
// a fingerprint secret. Keys must decode to exactly 32 bytes.
func ParseKeyring(spec, fingerprintSecret string) (*Keyring, error) {
	kr := &Keyring{aeads: map[string]cipher.AEAD{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, b64, ok := strings.Cut(part, ":")
		if !ok || !keyIDRe.MatchString(id) {
			return nil, fmt.Errorf("vault: key entry must be id:base64 with a simple id")
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("vault: key %q is not valid base64", id)
		}
		if err := kr.add(id, raw); err != nil {
			return nil, err
		}
	}
	if len(kr.order) == 0 {
		return nil, errors.New("vault: at least one key is required")
	}
	if len(fingerprintSecret) < 16 {
		return nil, errors.New("vault: fingerprint secret must be at least 16 characters")
	}
	kr.fpKey = []byte(fingerprintSecret)
	return kr, nil
}

// NewKeyring builds a keyring from raw keys (first = primary).
func NewKeyring(fingerprintSecret []byte, keys ...NamedKey) (*Keyring, error) {
	kr := &Keyring{aeads: map[string]cipher.AEAD{}, fpKey: fingerprintSecret}
	for _, k := range keys {
		if err := kr.add(k.ID, k.Key); err != nil {
			return nil, err
		}
	}
	if len(kr.order) == 0 {
		return nil, errors.New("vault: at least one key is required")
	}
	return kr, nil
}

// NamedKey is a raw key with its identifier.
type NamedKey struct {
	ID  string
	Key []byte
}

// GenerateKey returns a random 32-byte key.
func GenerateKey() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("vault: crypto/rand failed: " + err.Error())
	}
	return k
}

func (kr *Keyring) add(id string, raw []byte) error {
	if len(raw) != 32 {
		return fmt.Errorf("vault: key %q must be 32 bytes (AES-256), got %d", id, len(raw))
	}
	if _, dup := kr.aeads[id]; dup {
		return fmt.Errorf("vault: duplicate key id %q", id)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if kr.primary == "" {
		kr.primary = id
	}
	kr.aeads[id] = aead
	kr.order = append(kr.order, id)
	return nil
}

// PrimaryID returns the id of the sealing key.
func (kr *Keyring) PrimaryID() string { return kr.primary }

// KeyIDs returns all key ids, primary first.
func (kr *Keyring) KeyIDs() []string { return append([]string(nil), kr.order...) }

// Seal encrypts plaintext with the primary key. The result is nonce||ciphertext;
// aad binds the ciphertext to its context (e.g. the token it belongs to).
func (kr *Keyring) Seal(plaintext, aad []byte) (keyID string, sealed []byte, err error) {
	aead := kr.aeads[kr.primary]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	return kr.primary, aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value sealed with key keyID.
func (kr *Keyring) Open(keyID string, sealed, aad []byte) ([]byte, error) {
	aead, ok := kr.aeads[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, keyID)
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrMalformed
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// SealString seals a string into a self-describing text form
// "v1.<keyID>.<base64url>" suitable for TEXT columns.
func (kr *Keyring) SealString(plaintext, aad string) (string, error) {
	kid, sealed, err := kr.Seal([]byte(plaintext), []byte(aad))
	if err != nil {
		return "", err
	}
	return "v1." + kid + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// OpenString reverses SealString.
func (kr *Keyring) OpenString(s, aad string) (string, error) {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 || parts[0] != "v1" {
		return "", ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", ErrMalformed
	}
	pt, err := kr.Open(parts[1], raw, []byte(aad))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// SealedKeyID returns the key id of a SealString value.
func SealedKeyID(s string) string {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// Fingerprint returns a stable, keyed, non-reversible identifier for a
// value (used to recognise the same card across tokens without storing the
// PAN). It is independent of the encryption keys, so it survives rotation.
func (kr *Keyring) Fingerprint(value string) string {
	mac := hmac.New(sha256.New, kr.fpKey)
	mac.Write([]byte(value))
	return "fp_" + hex.EncodeToString(mac.Sum(nil))[:32]
}
