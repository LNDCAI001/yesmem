package staticplan

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashContent returns a hex-encoded SHA-256 digest of data.
// It is used to detect whether content has changed between requests.
func HashContent(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}
