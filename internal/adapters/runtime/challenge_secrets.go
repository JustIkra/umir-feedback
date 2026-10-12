package runtime

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type ChallengeCrypto struct {
	mu     sync.Mutex
	pepper [32]byte
	aead   cipher.AEAD
	random io.Reader
}

func NewChallengeCrypto(pepper, key []byte) (*ChallengeCrypto, error) {
	if len(pepper) != 32 || len(key) != 32 {
		return nil, errors.New("OTP keys must each contain 32 bytes")
	}
	if subtle.ConstantTimeCompare(pepper, key) == 1 {
		return nil, errors.New("OTP keys must be separate")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize OTP cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize OTP encryption: %w", err)
	}
	c := &ChallengeCrypto{aead: aead, random: rand.Reader}
	copy(c.pepper[:], pepper)
	return c, nil
}

func (c *ChallengeCrypto) NewID() (string, error) {
	var id [16]byte
	c.mu.Lock()
	_, err := io.ReadFull(c.random, id[:])
	c.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("OTP identity entropy unavailable: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func (c *ChallengeCrypto) NewCode() (string, error) {
	c.mu.Lock()
	n, err := rand.Int(c.random, big.NewInt(1_000_000))
	c.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("OTP entropy unavailable: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (c *ChallengeCrypto) Digest(scope, value string) []byte {
	if !validSecretScope(scope) || len(value) > 128 {
		return nil
	}
	mac := hmac.New(sha256.New, c.pepper[:])
	if _, err := mac.Write([]byte(scope)); err != nil {
		return nil
	}
	if _, err := mac.Write([]byte{0}); err != nil {
		return nil
	}
	data := []byte(value)
	defer clear(data)
	if _, err := mac.Write(data); err != nil {
		return nil
	}
	return mac.Sum(nil)
}

func (*ChallengeCrypto) Equal(a, b []byte) bool {
	return len(a) == sha256.Size && len(b) == sha256.Size && subtle.ConstantTimeCompare(a, b) == 1
}

func (c *ChallengeCrypto) Seal(scope, value string) (domain.SealedValue, error) {
	if !validSecretScope(scope) || value == "" || len(value) > 128 {
		return domain.SealedValue{}, errors.New("invalid OTP secret payload")
	}
	nonce := make([]byte, c.aead.NonceSize())
	c.mu.Lock()
	_, err := io.ReadFull(c.random, nonce)
	c.mu.Unlock()
	if err != nil {
		return domain.SealedValue{}, fmt.Errorf("OTP nonce entropy unavailable: %w", err)
	}
	data := []byte(value)
	defer clear(data)
	return domain.SealedValue{Nonce: nonce, Ciphertext: c.aead.Seal(nil, nonce, data, []byte(scope))}, nil
}

func (c *ChallengeCrypto) Open(scope string, sealed domain.SealedValue) (string, error) {
	if !validSecretScope(scope) || len(sealed.Nonce) != c.aead.NonceSize() || len(sealed.Ciphertext) < c.aead.Overhead()+1 || len(sealed.Ciphertext) > 128+c.aead.Overhead() {
		return "", errors.New("invalid OTP encrypted payload")
	}
	data, err := c.aead.Open(nil, sealed.Nonce, sealed.Ciphertext, []byte(scope))
	if err != nil {
		return "", errors.New("OTP encrypted payload authentication failed")
	}
	defer clear(data)
	return string(data), nil
}

func validSecretScope(scope string) bool {
	if scope == "" || len(scope) > 256 || !utf8.ValidString(scope) {
		return false
	}
	for _, r := range scope {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
