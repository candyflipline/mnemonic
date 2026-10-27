package wallet

import (
	"bytes"
	"crypto/ed25519"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Binding is a Claim with a wallet signature over it that verified. Only Sign,
// Bind and Verify make one, so holding a nonzero Binding means the signature
// is good.
type Binding struct {
	claim Claim
	sig   [ed25519.SignatureSize]byte
}

func (b Binding) Claim() Claim { return b.claim }

func normalize(c Claim) Claim {
	c.Expires = c.Expires.UTC().Truncate(time.Second)
	return c
}

// Sign binds c with the wallet's own key, as `mnemonic wallet bind --keypair`
// does from a Solana id.json; it signs the raw Message.
func Sign(c Claim, key ed25519.PrivateKey) (Binding, error) {
	c = normalize(c)
	if !bytes.Equal(key.Public().(ed25519.PublicKey), c.Wallet[:]) {
		return Binding{}, fmt.Errorf("keypair is wallet %s, the claim names %s", encode58(key.Public().(ed25519.PublicKey)), c.Wallet)
	}
	b := Binding{claim: c}
	copy(b.sig[:], ed25519.Sign(key, c.Message()))
	return b, nil
}

// Bind checks sig over c in either form a wallet produces (the raw Message,
// as Phantom signMessage signs, or offchainV0(Message), as solana
// sign-offchain-message signs) and is the Binding when one verifies.
func Bind(c Claim, sig []byte) (Binding, error) {
	c = normalize(c)
	if len(sig) != ed25519.SignatureSize {
		return Binding{}, fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	msg, pub := c.Message(), c.Wallet.PublicKey()
	if !ed25519.Verify(pub, msg, sig) && !ed25519.Verify(pub, offchainV0(msg), sig) {
		return Binding{}, fmt.Errorf("signature does not verify for wallet %s over the binding message", c.Wallet)
	}
	b := Binding{claim: c}
	copy(b.sig[:], sig)
	return b, nil
}

// envelope is the DER the certificate carries:
//
//	Binding ::= SEQUENCE { wallet OCTET STRING (32), expires INTEGER (unix seconds), signature OCTET STRING (64) }
type envelope struct {
	Wallet    []byte
	Expires   int64
	Signature []byte
}

// Encode is the bytes a client certificate carries. The device and server
// keys are not carried: the handshake proves both, and Verify rebuilds the
// Claim from them, so a binding moved to another device or server names keys
// its signature does not cover.
func (b Binding) Encode() []byte {
	der, err := asn1.Marshal(envelope{Wallet: b.claim.Wallet[:], Expires: b.claim.Expires.Unix(), Signature: b.sig[:]})
	if err != nil {
		panic(err)
	}
	return der
}

func decode(ext []byte, device, server ed25519.PublicKey) (Claim, []byte, error) {
	var e envelope
	rest, err := asn1.Unmarshal(ext, &e)
	if err != nil {
		return Claim{}, nil, fmt.Errorf("binding: %w", err)
	}
	if len(rest) != 0 || len(e.Wallet) != ed25519.PublicKeySize || len(e.Signature) != ed25519.SignatureSize {
		return Claim{}, nil, errors.New("binding: malformed")
	}
	if again, _ := asn1.Marshal(e); !bytes.Equal(again, ext) {
		return Claim{}, nil, errors.New("binding: not canonical DER")
	}
	c := Claim{Device: device, Server: server, Expires: time.Unix(e.Expires, 0).UTC()}
	copy(c.Wallet[:], e.Wallet)
	return c, e.Signature, nil
}

// Decode reads what Encode writes for the given device and server, without
// checking the signature: the client uses it to show its stored binding.
func Decode(ext []byte, device, server ed25519.PublicKey) (Claim, error) {
	c, _, err := decode(ext, device, server)
	return c, err
}

// Verify is the server's whole check of a certificate's binding bytes: decode
// them, rebuild the Claim with the device key the client proved and this
// server's key, refuse it when now is past Expires or Expires is more than
// MaxLifetime after now, and verify the signature in either form.
func Verify(ext []byte, device, server ed25519.PublicKey, now time.Time) (Binding, error) {
	c, sig, err := decode(ext, device, server)
	if err != nil {
		return Binding{}, err
	}
	if !now.Before(c.Expires) {
		return Binding{}, fmt.Errorf("binding for wallet %s expired at %s", c.Wallet, c.Expires.Format(time.RFC3339))
	}
	if c.Expires.Sub(now) > MaxLifetime {
		return Binding{}, fmt.Errorf("binding for wallet %s runs past the %d-day limit", c.Wallet, MaxLifetime/(24*time.Hour))
	}
	return Bind(c, sig)
}

// LoadKeypair reads a Solana CLI keypair file (id.json): a JSON array of 64
// bytes, seed then public key, which is ed25519.PrivateKey's own layout. It
// refuses a file whose public half does not match its seed.
func LoadKeypair(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var nums []int
	if err := json.Unmarshal(raw, &nums); err != nil || len(nums) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: not a Solana keypair file (a JSON array of 64 bytes)", path)
	}
	b := make([]byte, len(nums))
	for i, n := range nums {
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("%s: not a Solana keypair file (a JSON array of 64 bytes)", path)
		}
		b[i] = byte(n)
	}
	priv := ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])
	if !bytes.Equal(priv, b) {
		return nil, fmt.Errorf("%s: public half does not match the seed", path)
	}
	return priv, nil
}
