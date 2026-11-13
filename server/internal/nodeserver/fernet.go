package nodeserver

import (
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/fernet"
)

// Artifacts and durable central results use the same standard Fernet codec.
func fernetEncrypt(key, plaintext []byte) (string, error) { return fernet.Encrypt(key, plaintext) }
func fernetDecrypt(key, token []byte, maxAge time.Duration) ([]byte, error) {
	return fernet.Decrypt(key, token, maxAge)
}
func decodeFernetKey(key []byte) ([]byte, error) { return fernet.DecodeKey(key) }
