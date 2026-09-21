package hivectl

import (
	"bytes"
	"testing"
	"time"
)

func TestProfileBundleRoundTripEncrypted(t *testing.T) {
	profile := Profile{
		Name:              "acme",
		Hub:               "wss://acme.example/contribute",
		ContributorID:     "contrib_1",
		RegistrationToken: "secret-token",
		Session:           "review",
		AddedAt:           time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
	}
	bundle, err := EncryptProfileBundle(profile, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("EncryptProfileBundle: %v", err)
	}
	if bytes.Contains(bundle, []byte("secret-token")) {
		t.Fatalf("encrypted bundle contains the plaintext token:\n%s", bundle)
	}
	got, err := DecryptProfileBundle(bundle, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("DecryptProfileBundle: %v", err)
	}
	if got != profile {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got, profile)
	}
	if _, err := DecryptProfileBundle(bundle, []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase decrypted the bundle")
	}
}
