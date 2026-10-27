package qnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
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

func LoadCredential(keyFile string) (Credential, error) {
	priv, err := LoadOrCreateKey(keyFile)
	if err != nil {
		return Credential{}, err
	}
	b, err := os.ReadFile(keyFile + bindingSuffix)
	if errors.Is(err, fs.ErrNotExist) {
		return Credential{Key: priv}, nil
	}
	if err != nil {
		return Credential{}, err
	}
	return Credential{Key: priv, Binding: b}, nil
}

// SaveBinding stores binding beside keyFile (mode 0600, written to a temp
// file and renamed), replacing any earlier one.
func SaveBinding(keyFile string, binding []byte) error {
	f, err := os.CreateTemp(filepath.Dir(keyFile), filepath.Base(keyFile)+bindingSuffix+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(binding)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o600)
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), keyFile+bindingSuffix)
}

// bindingURI starts the one subjectAltName URI a client certificate carries
// its binding in, followed by the binding bytes in unpadded base64url. It is
// a tag, never a URL anyone fetches.
const bindingURI = "mnemonic:binding-v1:"

func certificate(cred Credential) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if cred.Binding != nil {
		u, err := url.Parse(bindingURI + base64.RawURLEncoding.EncodeToString(cred.Binding))
		if err != nil {
			return tls.Certificate{}, err
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, cred.Key.Public(), cred.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: cred.Key}, nil
}

// peerCert is the key a peer's one self-signed certificate proves and the
// binding bytes its one binding URI carries, nil when it has none. Two
// binding URIs, or one not in canonical unpadded base64url, fail.
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
	var binding []byte
	n := 0
	for _, u := range c.URIs {
		text, ok := strings.CutPrefix(u.String(), bindingURI)
		if !ok {
			continue
		}
		n++
		binding, err = base64.RawURLEncoding.Strict().DecodeString(text)
		if err != nil || base64.RawURLEncoding.EncodeToString(binding) != text {
			return nil, nil, errors.New("peer certificate binding is not canonical base64url")
		}
	}
	if n > 1 {
		return nil, nil, fmt.Errorf("peer certificate carries %d bindings, want at most 1", n)
	}
	return pub, binding, nil
}
