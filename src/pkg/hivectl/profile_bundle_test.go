package hivectl

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

func TestProfileBundleRefusesEmptyPassphrase(t *testing.T) {
	profile := Profile{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "t"}
	if _, err := EncryptProfileBundle(profile, nil); err == nil {
		t.Error("EncryptProfileBundle accepted an empty passphrase")
	}
	if _, err := DecryptProfileBundle([]byte("{}"), nil); err == nil {
		t.Error("DecryptProfileBundle accepted an empty passphrase")
	}
}

func TestProfileBundleRefusesAnInvalidProfile(t *testing.T) {
	// A profile that fails ProfileSet.Validate must not be exportable; the
	// bundle would otherwise carry a record the importer has to reject.
	bad := Profile{Name: "acme", Hub: "ftp://not-a-hub", RegistrationToken: "t"}
	if _, err := EncryptProfileBundle(bad, []byte("pw")); err == nil {
		t.Error("EncryptProfileBundle accepted a profile with a bad hub scheme")
	}
}

func TestProfileBundleRejectsMalformedBundles(t *testing.T) {
	profile := Profile{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "secret-token"}
	pw := []byte("pw")
	good, err := EncryptProfileBundle(profile, pw)
	if err != nil {
		t.Fatalf("EncryptProfileBundle: %v", err)
	}
	var bundle map[string]any
	if err := json.Unmarshal(good, &bundle); err != nil {
		t.Fatalf("bundle is not JSON: %v", err)
	}
	mutate := func(t *testing.T, change func(m map[string]any)) []byte {
		t.Helper()
		m := make(map[string]any, len(bundle))
		for k, v := range bundle {
			m[k] = v
		}
		change(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := map[string][]byte{
		"not json":        []byte("not json at all"),
		"wrong version":   mutate(t, func(m map[string]any) { m["version"] = ProfileBundleVersion + 1 }),
		"unknown kdf":     mutate(t, func(m map[string]any) { m["kdf"] = "scrypt" }),
		"unknown cipher":  mutate(t, func(m map[string]any) { m["cipher"] = "chacha20" }),
		"zero iterations": mutate(t, func(m map[string]any) { m["iterations"] = 0 }),
		"bad salt":        mutate(t, func(m map[string]any) { m["salt"] = "***" }),
		"bad nonce":       mutate(t, func(m map[string]any) { m["nonce"] = "***" }),
		"bad ciphertext":  mutate(t, func(m map[string]any) { m["ciphertext"] = "***" }),
		"tampered ciphertext": mutate(t, func(m map[string]any) {
			raw, _ := base64.StdEncoding.DecodeString(m["ciphertext"].(string))
			raw[len(raw)-1] ^= 0x01
			m["ciphertext"] = base64.StdEncoding.EncodeToString(raw)
		}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecryptProfileBundle(data, pw); err == nil {
				t.Errorf("%s bundle was accepted", name)
			}
		})
	}
}

func TestProfileBundleRejectsADecryptedProfileThatFailsValidation(t *testing.T) {
	// Build a bundle by hand around a plaintext that decrypts fine but names
	// no hub, so the post-decrypt Validate is what refuses it.
	pw := []byte("pw")
	salt := []byte("0123456789abcdef")
	nonce := []byte("0123456789ab")
	plain, _ := json.Marshal(profileBundlePlain{Name: "acme", Hub: "", RegistrationToken: "t"})
	key := pbkdf2Key(pw, salt, profileBundleIters, 32, sha256.New)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	bundle := encryptedProfileBundle{
		Version: ProfileBundleVersion, KDF: profileBundleKDF, Iterations: profileBundleIters, Cipher: profileBundleCipher,
		Salt: base64.StdEncoding.EncodeToString(salt), Nonce: base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil)),
	}
	data, _ := json.Marshal(bundle)
	if _, err := DecryptProfileBundle(data, pw); err == nil {
		t.Error("a decrypted profile with no hub was accepted")
	}
	// And a plaintext that is not the expected JSON shape.
	bundle.Ciphertext = base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, []byte("not json"), nil))
	data, _ = json.Marshal(bundle)
	if _, err := DecryptProfileBundle(data, pw); err == nil {
		t.Error("a non-JSON plaintext was accepted")
	}
}

// RFC 6070 test vector 2 for PBKDF2-HMAC-SHA256 is not published there, so this
// pins the widely reproduced vector (password "password", salt "salt", 1
// iteration, 32 bytes) to catch a regression in the hand-rolled KDF, and
// exercises the multi-block path with a 40-byte key.
func TestPBKDF2KnownAnswer(t *testing.T) {
	got := pbkdf2Key([]byte("password"), []byte("salt"), 1, 32, sha256.New)
	want := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if hex.EncodeToString(got) != want {
		t.Errorf("pbkdf2Key = %x, want %s", got, want)
	}
	got = pbkdf2Key([]byte("password"), []byte("salt"), 2, 32, sha256.New)
	want = "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"
	if hex.EncodeToString(got) != want {
		t.Errorf("pbkdf2Key(2 iters) = %x, want %s", got, want)
	}
	if long := pbkdf2Key([]byte("password"), []byte("salt"), 1, 40, sha256.New); len(long) != 40 {
		t.Errorf("multi-block key length = %d, want 40", len(long))
	}
}
