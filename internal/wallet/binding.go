package wallet

import (
	"crypto/ed25519"
	"time"
)

// Binding is a Claim with a wallet signature over it that verified. Only Sign
// and Bind make one, so holding a Binding means the signature is good.
type Binding struct {
	claim Claim
	sig   [ed25519.SignatureSize]byte
}

func (b Binding) Claim() Claim { return b.claim }

// Sign binds c with the wallet's own key, as `mnemonic wallet bind --keypair`
// does from a Solana id.json; it signs the raw Message.
func Sign(c Claim, key ed25519.PrivateKey) (Binding, error) {
	// TODO: refuse a key whose public half is not c.Wallet.
	panic("not implemented")
}

// Bind checks sig over c in either form a wallet produces (the raw Message,
// as Phantom signMessage signs, or offchainV0(Message), as solana
// sign-offchain-message signs) and is the Binding when one verifies. sig is
// 64 bytes; the CLI prints it in base58 and the caller decodes it.
func Bind(c Claim, sig []byte) (Binding, error) {
	panic("not implemented")
}

// Encode is the certificate extension value: the DER of
//
//	Binding ::= SEQUENCE { wallet OCTET STRING (32), expires INTEGER (unix seconds), signature OCTET STRING (64) }
//
// The device and server keys are not carried: the handshake proves both, and
// Verify rebuilds the Claim from them, so a binding moved to another device
// or server names keys its signature does not cover.
func (b Binding) Encode() []byte {
	panic("not implemented")
}

// Decode reads what Encode writes for the given device and server, without
// checking the signature: the client uses it to show its stored binding.
func Decode(ext []byte, device, server ed25519.PublicKey) (Claim, error) {
	panic("not implemented")
}

// Verify is the server's whole check of a certificate extension value: decode
// it, rebuild the Claim with the device key the client proved and this
// server's key, refuse it when now is past Expires or Expires is more than
// MaxLifetime after now, and verify the signature in either form. It returns
// the wallet the device reads as.
func Verify(ext []byte, device, server ed25519.PublicKey, now time.Time) (Address, error) {
	// TODO:
	//   c, err := Decode(ext, device, server)
	//   if !now.Before(c.Expires) -> expired; if c.Expires.Sub(now) > MaxLifetime -> too long
	//   msg := c.Message(); pub := c.Wallet.PublicKey()
	//   ok := ed25519.Verify(pub, msg, sig) || ed25519.Verify(pub, offchainV0(msg), sig)
	panic("not implemented")
}

// LoadKeypair reads a Solana CLI keypair file (id.json): a JSON array of 64
// bytes, seed then public key, which is ed25519.PrivateKey's own layout. It
// refuses a file whose public half does not match its seed.
func LoadKeypair(path string) (ed25519.PrivateKey, error) {
	panic("not implemented")
}
