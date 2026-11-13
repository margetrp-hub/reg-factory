package fernet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// fernetDecrypt is the inverse of fernetEncrypt and accepts both raw and
// padded URL-safe Fernet keys/tokens. Artifact keys are shared with Team, so
// this implementation intentionally follows the standard Fernet wire format.
func Decrypt(key, token []byte, maxAge time.Duration) ([]byte, error) {
	rawKey, err := DecodeKey(key)
	if err != nil {
		return nil, err
	}
	rawToken, err := decodeFernetToken(token)
	if err != nil {
		return nil, err
	}
	if len(rawToken) < 1+8+aes.BlockSize+sha256.Size {
		return nil, errors.New("fernet token too short")
	}
	if rawToken[0] != 0x80 {
		return nil, errors.New("unsupported fernet version")
	}
	stamp := int64(0)
	for _, value := range rawToken[1:9] {
		stamp = stamp<<8 | int64(value)
	}
	if maxAge > 0 {
		age := time.Now().Unix() - stamp
		if age < -300 || age > int64(maxAge/time.Second) {
			return nil, errors.New("fernet token expired")
		}
	}
	body, suppliedMAC := rawToken[:len(rawToken)-sha256.Size], rawToken[len(rawToken)-sha256.Size:]
	mac := hmac.New(sha256.New, rawKey[:16])
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), suppliedMAC) {
		return nil, errors.New("fernet token authentication failed")
	}
	ciphertext := body[9+aes.BlockSize:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("fernet ciphertext invalid")
	}
	block, err := aes.NewCipher(rawKey[16:])
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, body[9:9+aes.BlockSize]).CryptBlocks(plain, ciphertext)
	if len(plain) == 0 {
		return nil, errors.New("fernet plaintext empty")
	}
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("fernet padding invalid")
	}
	for _, value := range plain[len(plain)-pad:] {
		if int(value) != pad {
			return nil, errors.New("fernet padding invalid")
		}
	}
	return plain[:len(plain)-pad], nil
}

func DecodeKey(value []byte) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		if decoded, err := encoding.DecodeString(string(value)); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	if len(value) == 32 {
		return append([]byte(nil), value...), nil
	}
	return nil, fmt.Errorf("artifact key must be a Fernet 32-byte key")
}

func decodeFernetToken(value []byte) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		if decoded, err := encoding.DecodeString(string(value)); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("fernet token is not base64url")
}
