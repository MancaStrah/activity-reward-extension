package fccutils

import (
	"context"
	"fmt"
	"math/big"
	"net/url"
	"os"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/go-flare-common/pkg/contracts/tee/machinemanager"
)

// TeeStatusProduction is the ordinal of IMachineManager.TeeStatus.PRODUCTION
// (enum { NONE, INITIALIZED, PRODUCTION, SUSPENDED, PAUSED, BANNED }). Only a
// machine in this state has completed the on-chain attestation flow.
const TeeStatusProduction uint8 = 2

// RequireSecureProxyURL rejects proxy URLs whose transport a network attacker
// could intercept. The proxy /info response carries the TEE public key that a
// secret is about to be encrypted to; if that key can be swapped in transit, the
// secret is encrypted to the attacker instead. HTTPS (or a loopback host, where
// there is no network to intercept) is therefore required. Callers may bypass this
// for local development via an explicit opt-out.
func RequireSecureProxyURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing proxy url %q: %w", rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	// Deliberately generic: callers use this before fetching a TEE public key,
	// /info, or an action result, and over plain HTTP to a remote host all three
	// are whatever the network says they are.
	return fmt.Errorf(
		"refusing to talk to the proxy over an insecure transport (%q): a man-in-the-middle "+
			"controls this response, so use https:// or a loopback address",
		rawURL,
	)
}

// RequireSecureChainURL rejects an RPC URL a network attacker could tamper with.
// The client trusts RPC reads (chain id, TEE registry status/extension) to decide
// whether a key belongs to an attested PRODUCTION TEE before a secret is encrypted
// to it; over plain HTTP a MITM could forge those reads. HTTPS (or a loopback host)
// is required. Set ALLOW_INSECURE_RPC=true to bypass for a non-loopback dev RPC.
func RequireSecureChainURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing chain rpc url %q: %w", rawURL, err)
	}
	if u.Scheme == "https" || u.Scheme == "wss" {
		return nil
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	if os.Getenv("ALLOW_INSECURE_RPC") == "true" {
		return nil
	}
	return fmt.Errorf(
		"refusing to trust chain reads over an insecure transport (%q): use https:// so a man-in-the-middle "+
			"cannot forge the on-chain TEE checks, or set ALLOW_INSECURE_RPC=true for local dev",
		rawURL,
	)
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// VerifyTeeProduction confirms that teeAddr is a registered, attested TEE in
// PRODUCTION status on-chain before any secret is encrypted to its key.
//
// This is the client-side anchor for token confidentiality. teeAddr is derived
// from the public key returned by the proxy /info endpoint
// (crypto.PubkeyToAddress), so the address commits to that exact key, and a machine
// reaches PRODUCTION only after the on-chain registration flow verified its Google
// Confidential Space attestation. Two checks are made, and BOTH matter:
//
//   - the derived address is PRODUCTION, i.e. some genuine attested enclave;
//   - it belongs to expectedExtensionID.
//
// The second is not optional. PRODUCTION spans every TEE machine on the network, so
// without it a hostile or compromised proxy could return the public key of a machine
// the ATTACKER registered under their own extension, running code they wrote, and the
// caller would encrypt their Strava token straight to it. Pass the extension id read
// from the contract you are about to send the instruction to (its extensionId()
// getter) — not the one the proxy reports about itself.
//
// Be precise about what this does NOT establish: it does not verify the attestation
// quote or the code hash, so it inherits whatever the extension's governance
// allow-listed via allow-tee-version. It says "a machine this extension's governance
// accepted", not "a machine running the code I expect".
func VerifyTeeProduction(mm *machinemanager.MachineManager, teeAddr common.Address, expectedExtensionID *big.Int) error {
	if expectedExtensionID == nil || expectedExtensionID.Sign() <= 0 {
		return fmt.Errorf("expectedExtensionID must be set — refusing to verify a TEE against an unknown extension")
	}

	opts := &bind.CallOpts{Context: context.Background()}

	status, err := mm.GetTeeMachineStatus(opts, teeAddr)
	if err != nil {
		// The registry reverts with TeeNotFound() for an address that was never
		// registered — surfaced here as an error, i.e. fail closed.
		return fmt.Errorf(
			"querying on-chain status of TEE %s (unregistered key, wrong registry address, or unreachable RPC?): %w",
			teeAddr.Hex(), err,
		)
	}
	if !IsProductionStatus(status) {
		return fmt.Errorf(
			"refusing to encrypt to TEE %s: on-chain status is %d, expected PRODUCTION (%d) — the /info public key "+
				"does not belong to a registered, attested production TEE",
			teeAddr.Hex(), status, TeeStatusProduction,
		)
	}

	teeExtension, err := mm.GetExtensionId(opts, teeAddr)
	if err != nil {
		return fmt.Errorf("querying the extension of TEE %s: %w", teeAddr.Hex(), err)
	}
	if teeExtension.Cmp(expectedExtensionID) != 0 {
		return fmt.Errorf(
			"refusing to encrypt to TEE %s: it belongs to extension %s, not %s — the proxy may be serving the key of "+
				"a machine registered under someone else's extension",
			teeAddr.Hex(), teeExtension.String(), expectedExtensionID.String(),
		)
	}
	return nil
}

// IsProductionStatus reports whether a raw getTeeMachineStatus() value is PRODUCTION.
func IsProductionStatus(status uint8) bool {
	return status == TeeStatusProduction
}
