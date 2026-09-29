package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	// A deliberately low work factor keeps the unit test fast; production uses
	// DefaultIterations.
	const iter = 1000
	hash, salt, err := HashPassword("s3cret-password", iter)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" || salt == "" {
		t.Fatal("hash and salt must not be empty")
	}
	if !VerifyPassword("s3cret-password", hash, salt, iter) {
		t.Fatal("correct password was rejected")
	}
	if VerifyPassword("wrong-password", hash, salt, iter) {
		t.Fatal("incorrect password was accepted")
	}
	if VerifyPassword("", hash, salt, iter) {
		t.Fatal("empty password was accepted")
	}
}

func TestHashPasswordUsesFreshSalt(t *testing.T) {
	const iter = 1000
	h1, s1, _ := HashPassword("same", iter)
	h2, s2, _ := HashPassword("same", iter)
	if s1 == s2 {
		t.Fatal("salt was reused across hashes")
	}
	if h1 == h2 {
		t.Fatal("identical passwords produced identical hashes")
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, _, err := HashPassword("   ", 1000); err == nil {
		t.Fatal("expected an error for a blank password")
	}
}

func TestVerifyPasswordToleratesCorruptRecords(t *testing.T) {
	if VerifyPassword("x", "not-hex", "also-not-hex", 1000) {
		t.Fatal("corrupt hash must not verify")
	}
}

func TestGenerateAPIKeyShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if !strings.HasPrefix(k, "h3-") {
			t.Fatalf("key %q lacks the h3- prefix", k)
		}
		if len(k) < 20 {
			t.Fatalf("key %q is too short", k)
		}
		if seen[k] {
			t.Fatalf("duplicate key generated: %q", k)
		}
		seen[k] = true
	}
}

func TestKeyPrefixMasksValue(t *testing.T) {
	const k = "h3-abcdefghijklmnopqrstuvwxyz"
	p := KeyPrefix(k)
	if p == k {
		t.Fatal("prefix must not reveal the whole key")
	}
	if !strings.HasPrefix(k, strings.TrimSuffix(p, "…")) {
		t.Fatalf("prefix %q does not match the key", p)
	}
	if KeyPrefix("short") != "short" {
		t.Fatal("short values should pass through unchanged")
	}
}

func TestEqualSecret(t *testing.T) {
	if !EqualSecret("abc", "abc") {
		t.Fatal("equal secrets reported unequal")
	}
	if EqualSecret("abc", "abd") || EqualSecret("abc", "abcd") || EqualSecret("", "a") {
		t.Fatal("unequal secrets reported equal")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	secret := RandomSecret()
	token := SignSession(secret, "admin", "admin", time.Hour)
	sess, err := VerifySession(secret, token)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if sess.Scope != "admin" || sess.Subject != "admin" {
		t.Fatalf("unexpected session %+v", sess)
	}
	if time.Until(sess.Expires) < 55*time.Minute {
		t.Fatalf("expiry too soon: %v", sess.Expires)
	}
}

func TestSessionRejectsTamperingAndWrongSecret(t *testing.T) {
	secret := RandomSecret()
	token := SignSession(secret, "studio", "", time.Hour)

	if _, err := VerifySession(RandomSecret(), token); err == nil {
		t.Fatal("a token signed with another secret was accepted")
	}
	if _, err := VerifySession(secret, token+"x"); err == nil {
		t.Fatal("a tampered token was accepted")
	}
	if _, err := VerifySession(secret, "garbage"); err == nil {
		t.Fatal("a malformed token was accepted")
	}
	// Flipping a byte inside the payload must invalidate the signature. Mutate the
	// decoded payload and re-encode it: the signature covers the decoded bytes, so
	// editing the base64 text is not enough — depending on where the payload ends,
	// the last character's unused bits can decode to the very same bytes and the
	// "tampered" token would still be accepted. The flipped byte sits in the
	// trailing random id, so everything else still parses: only the signature can
	// reject it.
	parts := strings.SplitN(token, ".", 2)
	raw, err := unb64(parts[0])
	if err != nil {
		t.Fatalf("cannot decode the payload of our own token: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	mutated := b64(raw) + "." + parts[1]
	if mutated == token {
		t.Fatal("the mutation did not change the token")
	}
	if _, err := VerifySession(secret, mutated); err == nil {
		t.Fatal("a mutated payload was accepted")
	}
}

func TestSessionExpiry(t *testing.T) {
	secret := RandomSecret()
	token := SignSession(secret, "admin", "admin", -time.Minute)
	if _, err := VerifySession(secret, token); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

func TestRandomSecretIsUnique(t *testing.T) {
	a, b := RandomSecret(), RandomSecret()
	if a == b || len(a) < 32 {
		t.Fatalf("secrets look weak: %q %q", a, b)
	}
}
