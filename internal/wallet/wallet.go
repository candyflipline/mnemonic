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
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// Address is a Solana wallet address: the raw ed25519 public key whose
// base58 text is what wallets display. Only AddressOf and ParseAddress make
// one, so it is never a small-order key.
type Address struct{ key [ed25519.PublicKeySize]byte }

// smallOrder is every encoding of a point of order 1, 2, 4 or 8 with the sign
// bit cleared, canonical or not: libsodium's ge25519_has_small_order blocklist.
var smallOrder = [...][ed25519.PublicKeySize]byte{
	{},
	{0x01},
	{0x26, 0xe8, 0x95, 0x8f, 0xc2, 0xb2, 0x27, 0xb0, 0x45, 0xc3, 0xf4, 0x89, 0xf2, 0xef, 0x98, 0xf0, 0xd5, 0xdf, 0xac, 0x05, 0xd3, 0xc6, 0x33, 0x39, 0xb1, 0x38, 0x02, 0x88, 0x6d, 0x53, 0xfc, 0x05},
	{0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f, 0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f, 0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6, 0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a},
	{0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xee, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
}

// AddressOf is the wallet whose public key is pub. It refuses a key of small
// order in any encoding: ed25519.Verify accepts signatures nobody made under
// one, so such a key proves no holder.
func AddressOf(pub []byte) (Address, error) {
	var a Address
	if len(pub) != len(a.key) {
		return a, fmt.Errorf("a Solana address is %d bytes, not %d", len(a.key), len(pub))
	}
	copy(a.key[:], pub)
	k := a.key
	k[31] &= 0x7f
	if slices.Contains(smallOrder[:], k) {
		return Address{}, fmt.Errorf("%s is a small-order ed25519 key, under which signatures can be forged", a)
	}
	return a, nil
}

// AuthorPrefix starts every wallet author; config refuses a [[keys]] author
// that starts with it, so a wallet author never names a configured one.
const AuthorPrefix = "sol-"

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var radix = big.NewInt(58)

func decode58(s string) ([]byte, error) {
	n := new(big.Int)
	zeros := 0
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(alphabet, s[i])
		if d < 0 {
			return nil, fmt.Errorf("%q is not base58", s)
		}
		if d == 0 && zeros == i {
			zeros++
		}
		n.Mul(n, radix).Add(n, big.NewInt(int64(d)))
	}
	return append(make([]byte, zeros), n.Bytes()...), nil
}

func encode58(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(b)
	var out []byte
	m := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, radix, m)
		out = append(out, alphabet[m.Int64()])
	}
	out = append(out, strings.Repeat("1", zeros)...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// ParseAddress reads a base58 Solana address: exactly 32 bytes once decoded,
// no leading or trailing space, the Bitcoin base58 alphabet, and a key
// AddressOf accepts.
func ParseAddress(s string) (Address, error) {
	b, err := decode58(s)
	if err != nil {
		return Address{}, err
	}
	if len(b) != ed25519.PublicKeySize {
		return Address{}, fmt.Errorf("%q is %d bytes, a Solana address is 32", s, len(b))
	}
	return AddressOf(b)
}

func (a Address) String() string { return encode58(a.key[:]) }

// Author is the wallet's author name: AuthorPrefix and 64 lowercase hex
// digits, which matches wire.ValidSegment, so a wallet Identity's author
// is a legal author everywhere one is printed or compared.
func (a Address) Author() string { return AuthorPrefix + hex.EncodeToString(a.key[:]) }

func (a Address) PublicKey() ed25519.PublicKey { return ed25519.PublicKey(a.key[:]) }

// ParseSignature reads a 64-byte ed25519 signature as a wallet prints it:
// base58 (solana sign-offchain-message) or standard base64.
func ParseSignature(s string) ([]byte, error) {
	if b, err := decode58(s); err == nil && len(b) == ed25519.SignatureSize {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == ed25519.SignatureSize {
		return b, nil
	}
	return nil, errors.New("signature must be 64 bytes in base58 or base64")
}

// Claim is what a wallet asserts: Device may read as Wallet on the server
// whose key is Server until Expires (UTC, whole seconds).
type Claim struct {
	Wallet  Address
	Device  ed25519.PublicKey
	Server  ed25519.PublicKey
	Expires time.Time
}

const DefaultLifetime = 30 * 24 * time.Hour

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
	var c Claim
	rest, ok := strings.CutPrefix(line, messagePrefix+" ")
	fields := strings.Split(rest, " ")
	if !ok || len(fields) != 4 {
		return c, errors.New("not a mnemonic wallet binding v1 message")
	}
	vals := make([]string, 4)
	for i, name := range []string{"wallet", "device", "server", "expires"} {
		v, ok := strings.CutPrefix(fields[i], name+"=")
		if !ok {
			return c, fmt.Errorf("binding message field %d is not %s=", i+1, name)
		}
		vals[i] = v
	}
	var err error
	if c.Wallet, err = ParseAddress(vals[0]); err != nil {
		return c, err
	}
	if c.Device, err = parseKey(vals[1]); err != nil {
		return c, fmt.Errorf("device: %w", err)
	}
	if c.Server, err = parseKey(vals[2]); err != nil {
		return c, fmt.Errorf("server: %w", err)
	}
	if c.Expires, err = time.Parse(time.RFC3339, vals[3]); err != nil {
		return c, err
	}
	c.Expires = c.Expires.UTC()
	if string(c.Message()) != line {
		return c, errors.New("binding message is not in canonical form")
	}
	return c, nil
}

func parseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%q is not an unpadded base64url ed25519 key", s)
	}
	return b, nil
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
