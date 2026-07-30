package mint

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer = "https://hive.example.test/mint"
	testHiveID = "hive-test-1"
	testSub    = "spoke-alpha"
	testAud    = "//iam.googleapis.com/projects/123/locations/global/wif/providers/hive"
)

func newTestMinter(t *testing.T, opts ...Option) (*Minter, *rsa.PrivateKey) {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	base := []Option{WithHiveID(testHiveID)}
	m, err := NewMinter(key, testIssuer, append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m, key
}

func TestMintVerifyRoundTrip(t *testing.T) {
	m, _ := newTestMinter(t)
	scopes := []string{"registry:pull", "gcs:read"}

	tok, err := m.Mint(testSub, testAud, scopes, 5*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("issuer = %q, want %q", claims.Issuer, testIssuer)
	}
	if claims.Subject != testSub {
		t.Errorf("subject = %q, want %q", claims.Subject, testSub)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != testAud {
		t.Errorf("audience = %v, want [%q]", claims.Audience, testAud)
	}
	if claims.HiveID != testHiveID {
		t.Errorf("hive_id = %q, want %q", claims.HiveID, testHiveID)
	}
	if claims.ID == "" {
		t.Error("jti (ID) is empty")
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatal("exp/iat missing")
	}
}

func TestScopesPreserved(t *testing.T) {
	m, _ := newTestMinter(t)
	scopes := []string{"a", "b", "c"}
	tok, err := m.Mint(testSub, testAud, scopes, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if strings.Join(claims.Scopes, ",") != "a,b,c" {
		t.Errorf("scopes = %v, want [a b c]", claims.Scopes)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	m, key := newTestMinter(t)
	// Hand-craft an already-expired token signed with the same key so we bypass
	// Mint's TTL floor and test Verify's expiry enforcement directly.
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   testSub,
			Audience:  jwt.ClaimStrings{testAud},
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Hour)),
			ID:        "expired-jti",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := m.Verify(signed); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestWrongIssuerRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// A second minter with the SAME key but a different issuer.
	other, err := NewMinter(m.key, "https://evil.example/mint", WithHiveID(testHiveID))
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	tok, err := other.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Verify(tok); err == nil {
		t.Fatal("expected wrong-issuer token to be rejected")
	}
}

func TestWrongSignatureRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// Token minted by a DIFFERENT key but claiming our issuer.
	attacker, _ := newTestMinter(t)
	attacker.issuer = testIssuer
	tok, err := attacker.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Verify(tok); err == nil {
		t.Fatal("expected wrong-signature token to be rejected")
	}
}

func TestAlgNoneRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// Forge an alg=none token — must be rejected (fail closed).
	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   testSub,
		Audience:  jwt.ClaimStrings{testAud},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := m.Verify(signed); err == nil {
		t.Fatal("expected alg=none token to be rejected")
	}
}

func TestTTLCapEnforced(t *testing.T) {
	// Configure a small max; request far above the hard cap.
	m, _ := newTestMinter(t, WithMaxTTL(10*time.Minute))
	before := time.Now()
	tok, err := m.Mint(testSub, testAud, nil, 999*time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	life := claims.ExpiresAt.Time.Sub(before)
	if life > 10*time.Minute+clockSkewLeeway {
		t.Errorf("token life %v exceeds configured max 10m", life)
	}
}

func TestHardCapAlwaysWins(t *testing.T) {
	// Even a misconfigured max above the hard cap is clamped.
	m, _ := newTestMinter(t, WithMaxTTL(999*time.Hour))
	if m.MaxTTL() != HardCapTTL {
		t.Errorf("MaxTTL = %v, want hard cap %v", m.MaxTTL(), HardCapTTL)
	}
	before := time.Now()
	tok, err := m.Mint(testSub, testAud, nil, 999*time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, _ := m.Verify(tok)
	life := claims.ExpiresAt.Time.Sub(before)
	if life > HardCapTTL+clockSkewLeeway {
		t.Errorf("token life %v exceeds hard cap %v", life, HardCapTTL)
	}
}

func TestMintRequiresSubjectAndAudience(t *testing.T) {
	m, _ := newTestMinter(t)
	if _, err := m.Mint("", testAud, nil, time.Minute); err == nil {
		t.Error("expected error for empty subject")
	}
	if _, err := m.Mint(testSub, "", nil, time.Minute); err == nil {
		t.Error("expected error for empty audience")
	}
}

func TestNewMinterValidation(t *testing.T) {
	key, _ := GenerateKey()
	if _, err := NewMinter(nil, testIssuer); err == nil {
		t.Error("expected error for nil key")
	}
	if _, err := NewMinter(key, ""); err == nil {
		t.Error("expected error for empty issuer")
	}
}

func TestJWKSValidatesMintedToken(t *testing.T) {
	m, _ := newTestMinter(t)
	tok, err := m.Mint(testSub, testAud, []string{"x"}, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	jwksBytes, err := m.JWKS()
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}

	var set struct {
		Keys []struct {
			Kty, Use, Alg, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwksBytes, &set); err != nil {
		t.Fatalf("JWKS parse: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("JWKS keys = %d, want 1", len(set.Keys))
	}
	k := set.Keys[0]
	if k.Kty != "RSA" || k.Use != "sig" || k.Alg != "RS256" {
		t.Errorf("unexpected JWKS entry: %+v", k)
	}
	if k.Kid == "" || k.N == "" || k.E == "" {
		t.Errorf("JWKS entry missing kid/n/e: %+v", k)
	}

	// Reconstruct the public key from JWKS and verify the token independently.
	pub := rebuildPublicKey(t, k.N, k.E)
	parsed, err := jwt.Parse(tok, func(tk *jwt.Token) (interface{}, error) {
		if _, ok := tk.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("token failed JWKS-reconstructed verification: err=%v valid=%v", err, parsed.Valid)
	}

	// kid in JWKS matches token header.
	unverified, _, _ := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
	if kid, _ := unverified.Header["kid"].(string); kid != k.Kid {
		t.Errorf("token kid %q != JWKS kid %q", kid, k.Kid)
	}
}

func rebuildPublicKey(t *testing.T, nB64, eB64 string) *rsa.PublicKey {
	t.Helper()
	nBytes := mustB64URL(t, nB64)
	eBytes := mustB64URL(t, eB64)
	n := new(big.Int).SetBytes(nBytes)
	var e int
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{N: n, E: e}
}

func mustB64URL(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", s, err)
	}
	return b
}

func TestLoadOrCreateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mint.pem")

	// Create.
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (create): %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key perms = %o, want 600", perm)
	}

	// Load existing — must be the same key.
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (load): %v", err)
	}
	if k1.N.Cmp(k2.N) != 0 {
		t.Error("reloaded key differs from persisted key")
	}

	if _, err := LoadOrCreateKey(""); err == nil {
		t.Error("expected error for empty key path")
	}
}
