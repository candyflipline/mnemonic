package qnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
)

// LoadOrCreateKey reads the ed25519 seed at path, writing a new one (file
// mode 0600, parent directory 0700) when the file does not exist.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: not an unpadded base64url ed25519 seed", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(base64.RawURLEncoding.EncodeToString(priv.Seed()) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return priv, nil
}

// PublicKey is the auth.EncodeKey form of priv's public key.
func PublicKey(priv ed25519.PrivateKey) string {
	return auth.EncodeKey(priv.Public().(ed25519.PublicKey))
}

// Credential is what a client proves: its device key and, on a device a
// wallet bound, the binding bytes wallet.Binding.Encode wrote (nil when none).
type Credential struct {
	Key     ed25519.PrivateKey
	Binding []byte
}

// bindingSuffix names a key file's binding: the binding of ~/.mnemonic/key is
// ~/.mnemonic/key.wallet, so a binding never outlives or crosses its key.
const bindingSuffix = ".wallet"

// LoadCredential is LoadOrCreateKey(keyFile) and the binding stored beside it
// by SaveBinding, nil when there is none.
func LoadCredential(keyFile string) (Credential, error) {
	panic("not implemented")
}

// SaveBinding stores binding beside keyFile (mode 0600, written to a temp
// file and renamed), replacing any earlier one.
func SaveBinding(keyFile string, binding []byte) error {
	panic("not implemented")
}

// bindingOID is the non-critical extension a client certificate carries its
// binding in. It is a placeholder under the IANA private enterprise arc until
// mnemonic has its own enterprise number; only mnemonic reads it, from
// certificates mnemonic made.
var bindingOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}

func certificate(cred Credential) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if cred.Binding != nil {
		tmpl.ExtraExtensions = []pkix.Extension{{Id: bindingOID, Value: cred.Binding}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, cred.Key.Public(), cred.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: cred.Key}, nil
}

// peerCert is the key a peer's one self-signed certificate proves and the value
// of its binding extension, nil when it has none.
func peerCert(raw [][]byte) (ed25519.PublicKey, []byte, error) {
	if len(raw) != 1 {
		return nil, nil, fmt.Errorf("peer sent %d certificates, want 1", len(raw))
	}
	c, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return nil, nil, err
	}
	pub, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, nil, errors.New("peer certificate is not an ed25519 key")
	}
	for _, e := range c.Extensions {
		if e.Id.Equal(bindingOID) {
			return pub, e.Value, nil
		}
	}
	return pub, nil, nil
}
