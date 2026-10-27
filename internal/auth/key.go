package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// EncodeKey is the text form of an ed25519 public key in configs and logs:
// unpadded base64url of its 32 bytes.
func EncodeKey(pub ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(pub) }

func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key %q must be unpadded base64url of 32 bytes", s)
	}
	return ed25519.PublicKey(b), nil
}
