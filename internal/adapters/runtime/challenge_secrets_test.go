package runtime

import (
	"bytes"
	"errors"
	"testing"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func otpTestSecrets(t *testing.T) *ChallengeCrypto {
	t.Helper()
	c, err := NewChallengeCrypto(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChallengeCodePreservesZerosAndRejectsBiasedSample(t *testing.T) {
	c := otpTestSecrets(t)
	c.random = bytes.NewReader([]byte{0xff, 0xff, 0xff, 0, 0, 7})
	code, err := c.NewCode()
	if err != nil || code != "000007" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	c.random = bytes.NewReader([]byte{0x0f, 0x42, 0x3f})
	code, err = c.NewCode()
	if err != nil || code != "999999" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if _, err := c.NewCode(); err == nil {
		t.Fatal("exhausted entropy source accepted")
	}
}

func TestChallengeSecretsBindCiphertextAndDigestToPurpose(t *testing.T) {
	c := otpTestSecrets(t)
	sealed, err := c.Seal("id:operator-registration:code", "000007")
	if err != nil {
		t.Fatal(err)
	}
	value, err := c.Open("id:operator-registration:code", sealed)
	if err != nil || value != "000007" {
		t.Fatalf("open=%q err=%v", value, err)
	}
	if _, err := c.Open("other:operator-registration:code", sealed); err == nil {
		t.Fatal("wrong AAD accepted")
	}
	sealed.Ciphertext[0] ^= 1
	if _, err := c.Open("id:operator-registration:code", sealed); err == nil {
		t.Fatal("tamper accepted")
	}
	if _, err := c.Open("id", domain.SealedValue{Nonce: []byte{1}}); err == nil {
		t.Fatal("short nonce accepted")
	}
	a := c.Digest("code", "000007")
	b := c.Digest("proof", "000007")
	if len(a) != 32 || c.Equal(a, b) || c.Equal(a, nil) || !c.Equal(a, c.Digest("code", "000007")) {
		t.Fatal("digest binding failed")
	}
}

func TestChallengeSecretsValidateKeysAndUUIDEntropy(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := NewChallengeCrypto(bytes.Repeat([]byte{1}, size), bytes.Repeat([]byte{2}, 32)); err == nil {
			t.Fatal("invalid pepper accepted")
		}
		if _, err := NewChallengeCrypto(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, size)); err == nil {
			t.Fatal("invalid encryption key accepted")
		}
	}
	c := otpTestSecrets(t)
	id, err := c.NewID()
	if err != nil || !domain.ValidID(domain.SubmissionID(id)) {
		t.Fatalf("invalid UUID %q %v", id, err)
	}
	one, err := c.Seal("scope", "value")
	if err != nil {
		t.Fatal(err)
	}
	two, err := c.Seal("scope", "value")
	if err != nil || bytes.Equal(one.Nonce, two.Nonce) {
		t.Fatal("nonce reuse")
	}
	c.random = brokenEntropy{}
	if _, err := c.NewID(); err == nil {
		t.Fatal("UUID entropy failure accepted")
	}
	if _, err := c.Seal("scope", "value"); err == nil {
		t.Fatal("nonce entropy failure accepted")
	}
}

type brokenEntropy struct{}

func (brokenEntropy) Read([]byte) (int, error) { return 0, errors.New("test entropy unavailable") }
