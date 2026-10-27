package main

import (
	"flag"
	"fmt"
	"time"
)

func init() { commands["wallet"] = walletCmd }

const defaultBindingDays = 90

// walletCmd binds this machine's key to a Solana wallet, so the machine reads
// the sessions shared with that wallet. Every form reads the client config
// for the key file and the server key the binding names.
//
//	mnemonic wallet bind --keypair ~/.config/solana/id.json [--days 90]
//	mnemonic wallet message --wallet <address> [--days 90]
//	mnemonic wallet bind --message '<the line message printed>' --signature <base58 or base64>
//	mnemonic wallet show
func walletCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mnemonic wallet bind|message|show")
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

// walletBind stores a binding beside the key file with qnet.SaveBinding.
// With --keypair it signs the claim itself (wallet.Sign over the raw
// message). With --message and --signature it parses the pasted line
// (wallet.ParseMessage), refuses it unless its device and server keys are
// this machine's and this config's, and checks the signature in both forms
// (wallet.Bind) before storing, so a bad paste fails here, not at connect.
// It prints the wallet address and the expiry in local time.
func walletBind(args []string) error {
	fs := flag.NewFlagSet("wallet bind", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	keypair := fs.String("keypair", "", "Solana CLI keypair file (id.json) to sign with")
	days := fs.Int("days", defaultBindingDays, "days until the binding expires (with --keypair)")
	message := fs.String("message", "", "the line `mnemonic wallet message` printed, as the wallet signed it")
	signature := fs.String("signature", "", "the wallet's signature over --message, base58 or base64")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, _, _, _, _ = configPath, keypair, days, message, signature
	// TODO: exactly one of --keypair or (--message and --signature).
	panic("not implemented")
}

// walletMessage prints the one line a wallet app signs: the claim that
// --wallet lets this machine's key read on this config's server until now
// plus --days. Paste it into Phantom's sign-message, or run
// `solana sign-offchain-message '<line>'`, then pass the line and the
// signature to `mnemonic wallet bind`.
func walletMessage(args []string) error {
	fs := flag.NewFlagSet("wallet message", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	address := fs.String("wallet", "", "the wallet's base58 address")
	days := fs.Int("days", defaultBindingDays, "days until the binding expires")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, _, _ = configPath, address, time.Duration(*days)*24*time.Hour
	panic("not implemented")
}

// walletShow prints the stored binding's wallet and expiry (wallet.Decode),
// or that this machine has none.
func walletShow(args []string) error {
	panic("not implemented")
}
