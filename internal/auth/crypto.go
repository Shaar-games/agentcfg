package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

type protector interface {
	Scheme() string
	Seal(identity string, plaintext []byte) ([]byte, error)
	Open(identity string, ciphertext []byte) ([]byte, error)
}

type aesProtector struct{ key []byte }

func (*aesProtector) Scheme() string { return "external-aes-gcm-v1" }
func (p *aesProtector) Seal(identity string, plain []byte) ([]byte, error) {
	return sealAES(p.key, identity, plain)
}
func (p *aesProtector) Open(identity string, encrypted []byte) ([]byte, error) {
	return openAES(p.key, identity, encrypted)
}

func cipherFor(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption requires 256-bit keys")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealAES(key []byte, identity string, plain []byte) ([]byte, error) {
	aead, err := cipherFor(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, []byte("agentcfg.oauth.v1\x00"+identity)), nil
}

func openAES(key []byte, identity string, encrypted []byte) ([]byte, error) {
	aead, err := cipherFor(key)
	if err != nil {
		return nil, err
	}
	if len(encrypted) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrStoreCorrupt
	}
	nonce := encrypted[:aead.NonceSize()]
	return aead.Open(nil, nonce, encrypted[aead.NonceSize():], []byte("agentcfg.oauth.v1\x00"+identity))
}
