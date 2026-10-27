// Package wallet is everything mnemonic knows about Solana: an Address is a
// wallet's 32-byte ed25519 public key, printed in base58; a Claim is what a
// wallet signs to let one device key read as it on one server until a time;
// a Binding is a signed Claim, carried in the device's client certificate and
// checked by the server during the handshake. No other package parses base58
// or knows the message bytes.
package wallet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// Address is a Solana wallet address: the raw ed25519 public key whose
// base58 text is what wallets display.
type Address [ed25519.PublicKeySize]byte

// AuthorPrefix starts every wallet author; config refuses a [[keys]] author
// that starts with it, so a wallet author never names a configured one.
const AuthorPrefix = "sol-"

// ParseAddress reads a base58 Solana address: exactly 32 bytes once decoded,
// no leading or trailing space, the Bitcoin base58 alphabet.
func ParseAddress(s string) (Address, error) {
	panic("not implemented")
}

// String is the base58 text wallets show.
func (a Address) String() string {
	panic("not implemented")
}

// Author is the wallet's author name: AuthorPrefix and 64 lowercase hex
// digits, which matches wire.ValidSegment, so a wallet Identity's author
// is a legal author everywhere one is printed or compared.
func (a Address) Author() string { return AuthorPrefix + hex.EncodeToString(a[:]) }

// PublicKey is a as the ed25519 key its signatures verify under.
func (a Address) PublicKey() ed25519.PublicKey { return ed25519.PublicKey(a[:]) }

// Claim is what a wallet asserts: Device may read as Wallet on the server
// whose key is Server until Expires (UTC, whole seconds).
type Claim struct {
	Wallet  Address
	Device  ed25519.PublicKey
	Server  ed25519.PublicKey
	Expires time.Time
}

// MaxLifetime is the longest a binding may run from the moment the server
// checks it: an expiry further out is refused, so a binding cannot be made
// to last forever.
const MaxLifetime = 366 * 24 * time.Hour

const messagePrefix = "mnemonic wallet binding v1"

// Message is the canonical bytes the wallet signs, one line of printable
// ASCII so solana sign-offchain-message picks format 0:
//
//	mnemonic wallet binding v1 wallet=<base58> device=<base64url> server=<base64url> expires=<RFC 3339 UTC>
//
// The device and server keys are unpadded base64url, as mnemonic prints keys.
func (c Claim) Message() []byte {
	return []byte(messagePrefix +
		" wallet=" + c.Wallet.String() +
		" device=" + base64.RawURLEncoding.EncodeToString(c.Device) +
		" server=" + base64.RawURLEncoding.EncodeToString(c.Server) +
		" expires=" + c.Expires.UTC().Truncate(time.Second).Format(time.RFC3339))
}

// ParseMessage is Message's inverse, for a message pasted back after a
// wallet app signed it. It refuses any line Message would not write.
func ParseMessage(line string) (Claim, error) {
	panic("not implemented")
}

// offchainV0 is the preamble solana sign-offchain-message (CLI 4.2.0) signs
// in front of a format 0 message: "\xffsolana offchain", version 0, format 0,
// then the message length as u16 little-endian.
func offchainV0(msg []byte) []byte {
	out := make([]byte, 0, 20+len(msg))
	out = append(out, "\xffsolana offchain"...)
	out = append(out, 0, 0)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(msg)))
	return append(out, msg...)
}
