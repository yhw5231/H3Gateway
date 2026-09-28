// Package auth implements the gateway's credential primitives: PBKDF2 password
// hashing, API key minting and HMAC-signed session cookies. Everything is built
// on the standard library so the whole gateway stays dependency-free and builds
// offline inside a container.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultIterations is the PBKDF2 work factor for admin passwords.
const DefaultIterations = 210_000

// keyLen is the derived key and salt size in bytes.
const keyLen = 32

// ErrInvalidToken is returned when a session cookie fails verification.
var ErrInvalidToken = errors.New("invalid session token")

// ---------------------------------------------------------------------------
// password hashing
// ---------------------------------------------------------------------------

// pbkdf2SHA256 is a self-contained PBKDF2 (RFC 8018) over HMAC-SHA256. It keeps
// the module compatible with Go versions that predate crypto/pbkdf2.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		prf.Write(buf[:])
		u = prf.Sum(u[:0])

		t := make([]byte, hashLen)
		copy(t, u)
		for n := 1; n < iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// HashPassword derives a salted hash. The returned values are hex strings safe
// to persist in the JSON store.
func HashPassword(password string, iterations int) (hashHex, saltHex string, err error) {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	if strings.TrimSpace(password) == "" {
		return "", "", errors.New("password must not be empty")
	}
	salt := make([]byte, keyLen)
	if _, err = rand.Read(salt); err != nil {
		return "", "", err
	}
	sum := pbkdf2SHA256([]byte(password), salt, iterations, keyLen)
	return hex.EncodeToString(sum), hex.EncodeToString(salt), nil
}

// VerifyPassword compares a candidate password against a stored hash in constant
// time.
func VerifyPassword(password, hashHex, saltHex string, iterations int) bool {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(hashHex)
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------------------------------------------------------------------------
// api keys
// ---------------------------------------------------------------------------

// GenerateAPIKey returns a fresh bearer token such as
// "h3-3f9a...". 32 bytes of entropy keeps it safe for programmatic use.
func GenerateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "h3-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateID returns a short opaque identifier for stored records.
func GenerateID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is fatal for identity generation; fall back to a
		// time-derived value rather than returning an empty id.
		return "id" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// KeyPrefix is the display form of an API key: enough to recognise it, not
// enough to use it.
func KeyPrefix(key string) string {
	if len(key) <= 11 {
		return key
	}
	return key[:11] + "…"
}

// MaskKey renders a key for list endpoints.
func MaskKey(key string) string { return KeyPrefix(key) }

// EqualSecret compares two secrets without leaking timing information.
func EqualSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// Session is a verified session cookie payload.
type Session struct {
	Scope    string // "admin" or "studio"
	Subject  string // admin username, empty for studio
	IssuedAt time.Time
	Expires  time.Time
}

// SignSession mints a signed, stateless session token.
func SignSession(secret, scope, subject string, ttl time.Duration) string {
	now := time.Now()
	exp := now.Add(ttl)
	payload := strings.Join([]string{
		scope,
		b64([]byte(subject)),
		strconv.FormatInt(now.Unix(), 10),
		strconv.FormatInt(exp.Unix(), 10),
		GenerateID(),
	}, "|")
	sig := sign(secret, payload)
	return b64([]byte(payload)) + "." + b64(sig)
}

// VerifySession validates a token and returns its payload.
func VerifySession(secret, token string) (*Session, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, ErrInvalidToken
	}
	rawPayload, err := unb64(parts[0])
	if err != nil {
		return nil, ErrInvalidToken
	}
	sig, err := unb64(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	if !hmac.Equal(sig, sign(secret, string(rawPayload))) {
		return nil, ErrInvalidToken
	}
	fields := strings.Split(string(rawPayload), "|")
	if len(fields) != 5 {
		return nil, ErrInvalidToken
	}
	subject, err := unb64(fields[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	iat, err1 := strconv.ParseInt(fields[2], 10, 64)
	exp, err2 := strconv.ParseInt(fields[3], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, ErrInvalidToken
	}
	s := &Session{
		Scope:    fields[0],
		Subject:  string(subject),
		IssuedAt: time.Unix(iat, 0),
		Expires:  time.Unix(exp, 0),
	}
	if time.Now().After(s.Expires) {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	return s, nil
}

func sign(secret, payload string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// RandomSecret returns a fresh 32-byte secret, hex encoded.
func RandomSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "fallback-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}
