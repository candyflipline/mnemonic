package main

import (
	"crypto/ed25519"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/wallet"
)

func init() { commands["wallet"] = walletCmd }

func walletCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mnemonic wallet bind|message|show [flags]")
	}
	switch args[0] {
	case "bind":
		return walletBind(args[1:])
	case "message":
		return walletMessage(args[1:])
	case "show":
		return walletShow(args[1:])
	}
	return fmt.Errorf("mnemonic wallet: unknown subcommand %q", args[0])
}

type device struct {
	keyFile string
	key     ed25519.PrivateKey
	server  ed25519.PublicKey
}

func loadDevice(path string) (device, error) {
	cfg, err := config.LoadClient(path)
	if err != nil {
		return device{}, err
	}
	server, err := auth.ParseKey(cfg.ServerKey)
	if err != nil {
		return device{}, err
	}
	key, err := qnet.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		return device{}, err
	}
	return device{keyFile: cfg.KeyFile, key: key, server: server}, nil
}

func (d device) claim(w wallet.Address, expires time.Time) wallet.Claim {
	return wallet.Claim{Wallet: w, Device: d.key.Public().(ed25519.PublicKey), Server: d.server, Expires: expires}
}

func (d device) save(b wallet.Binding) error {
	if err := qnet.SaveBinding(d.keyFile, b.Encode()); err != nil {
		return err
	}
	printBound(b.Claim())
	return nil
}

func printBound(c wallet.Claim) {
	fmt.Printf("wallet %s bound to this machine until %s\n", c.Wallet, c.Expires.Local().Format("Jan 2, 2006 3:04 PM MST"))
}

// maxDays leaves one day under wallet.MaxLifetime for a server clock that
// runs behind this one.
var maxDays = int(wallet.MaxLifetime/(24*time.Hour)) - 1

func expiry(days int) (time.Time, error) {
	if days < 1 || days > maxDays {
		return time.Time{}, fmt.Errorf("--days must be 1 to %d", maxDays)
	}
	return time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Truncate(time.Second), nil
}

var defaultDays = int(wallet.DefaultLifetime / (24 * time.Hour))

func walletBind(args []string) error {
	fs := flag.NewFlagSet("wallet bind", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	keypair := fs.String("keypair", "", "Solana CLI keypair file (id.json) to sign with")
	phantom := fs.Bool("phantom", false, "sign in a browser wallet (Phantom) on a local page")
	days := fs.Int("days", defaultDays, "days until the binding expires (with --keypair or --phantom)")
	message := fs.String("message", "", "the line `mnemonic wallet message` printed, as the wallet signed it")
	signature := fs.String("signature", "", "the wallet's signature over --message, base58 or base64")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	paste := *message != "" || *signature != ""
	modes := 0
	for _, on := range []bool{*keypair != "", *phantom, paste} {
		if on {
			modes++
		}
	}
	if modes != 1 || (paste && (*message == "" || *signature == "")) {
		return errors.New("wallet bind takes one of --keypair, --phantom, or --message with --signature")
	}
	d, err := loadDevice(*configPath)
	if err != nil {
		return err
	}
	switch {
	case *keypair != "":
		expires, err := expiry(*days)
		if err != nil {
			return err
		}
		path, err := config.ExpandHome(*keypair)
		if err != nil {
			return err
		}
		key, err := wallet.LoadKeypair(path)
		if err != nil {
			return err
		}
		w, err := wallet.AddressOf(key.Public().(ed25519.PublicKey))
		if err != nil {
			return err
		}
		b, err := wallet.Sign(d.claim(w, expires), key)
		if err != nil {
			return err
		}
		return d.save(b)
	case *phantom:
		expires, err := expiry(*days)
		if err != nil {
			return err
		}
		return bindInBrowser(d, expires)
	}
	c, err := wallet.ParseMessage(*message)
	if err != nil {
		return err
	}
	if !c.Device.Equal(d.key.Public()) {
		return fmt.Errorf("the message names device %s; this machine's key is %s", auth.EncodeKey(c.Device), qnet.PublicKey(d.key))
	}
	if !c.Server.Equal(d.server) {
		return fmt.Errorf("the message names server %s; this config's server_key is %s", auth.EncodeKey(c.Server), auth.EncodeKey(d.server))
	}
	if !time.Now().Before(c.Expires) {
		return fmt.Errorf("the message expired at %s; run mnemonic wallet message again", c.Expires.Format(time.RFC3339))
	}
	sig, err := wallet.ParseSignature(strings.TrimSpace(*signature))
	if err != nil {
		return err
	}
	b, err := wallet.Bind(c, sig)
	if err != nil {
		return err
	}
	return d.save(b)
}

func walletMessage(args []string) error {
	fs := flag.NewFlagSet("wallet message", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	address := fs.String("wallet", "", "the wallet's base58 address")
	days := fs.Int("days", defaultDays, "days until the binding expires")
	if err := fs.Parse(args); err != nil {
		return err
	}
	w, err := wallet.ParseAddress(*address)
	if err != nil {
		return fmt.Errorf("--wallet: %w", err)
	}
	expires, err := expiry(*days)
	if err != nil {
		return err
	}
	d, err := loadDevice(*configPath)
	if err != nil {
		return err
	}
	fmt.Println(string(d.claim(w, expires).Message()))
	return nil
}

func walletShow(args []string) error {
	fs := flag.NewFlagSet("wallet show", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := loadDevice(*configPath)
	if err != nil {
		return err
	}
	cred, err := qnet.LoadCredential(d.keyFile)
	if err != nil {
		return err
	}
	if cred.Binding == nil {
		fmt.Println("this machine has no wallet binding")
		return nil
	}
	c, err := wallet.Decode(cred.Binding, d.key.Public().(ed25519.PublicKey), d.server)
	if err != nil {
		return err
	}
	state := "until"
	if !time.Now().Before(c.Expires) {
		state = "expired"
	}
	fmt.Printf("wallet %s bound to this machine, %s %s\n", c.Wallet, state, c.Expires.Local().Format("Jan 2, 2006 3:04 PM MST"))
	return nil
}

//go:embed phantom.html
var phantomPage []byte

func bindInBrowser(d device, expires time.Time) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	token := make([]byte, 16)
	rand.Read(token)
	base := "/" + hex.EncodeToString(token) + "/"
	done := make(chan wallet.Binding, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		w.Write(phantomPage)
	})
	mux.HandleFunc("GET "+base+"message", func(w http.ResponseWriter, r *http.Request) {
		a, err := wallet.ParseAddress(r.URL.Query().Get("wallet"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(d.claim(a, expires).Message())
	})
	mux.HandleFunc("POST "+base+"bind", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Wallet, Signature string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a, err := wallet.ParseAddress(in.Wallet)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sig, err := wallet.ParseSignature(in.Signature)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, err := wallet.Bind(d.claim(a, expires), sig)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := qnet.SaveBinding(d.keyFile, b.Encode()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "wallet %s bound to this machine; you can close this tab", a)
		select {
		case done <- b:
		default:
		}
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	defer hs.Close()
	fmt.Fprintf(os.Stderr, "open http://%s%s in the browser that has Phantom, connect, and approve the message; nothing else is signed\n", ln.Addr(), base)
	select {
	case b := <-done:
		printBound(b.Claim())
		return nil
	case <-time.After(browserWait):
		return fmt.Errorf("no wallet signed within %s; run mnemonic wallet bind --phantom again", browserWait)
	}
}

const browserWait = 10 * time.Minute
