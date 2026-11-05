package nodeserver

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"
)

// fernetEncrypt 实现标准 Fernet（AES-128-CBC + HMAC-SHA256），
// key 为 base64url 编码的 32 字节（前 16 签名、后 16 加密）。
func fernetEncrypt(key, plaintext []byte) (string, error) {
	raw, err := decodeFernetKey(key)
	if err != nil {
		return "", err
	}
	signingKey, encryptionKey := raw[:16], raw[16:]

	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return "", err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}

	// PKCS7 填充
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytesRepeat(byte(pad), pad)...)

	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	// version | timestamp | IV | ciphertext
	header := make([]byte, 0, 1+8)
	header = append(header, 0x80)
	ts := uint64(time.Now().Unix())
	for shift := 56; shift >= 0; shift -= 8 {
		header = append(header, byte(ts>>uint(shift)))
	}
	body := append(header, iv...)
	body = append(body, ciphertext...)

	mac := hmac.New(sha256.New, signingKey)
	mac.Write(body)
	token := append(body, mac.Sum(nil)...)
	return base64.URLEncoding.EncodeToString(token), nil
}

func bytesRepeat(b byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = b
	}
	return out
}
