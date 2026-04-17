// Command get-result fetches an instruction's result from the extension proxy
// and prints the decoded ActionResult.
//
// Everything in that response is attacker-influenced if the proxy is
// compromised or impersonated, so this command treats it as untrusted input:
// structured fields must parse exactly or the proof is refused, and free text is
// escaped before it reaches the terminal.
//
// It deliberately does not print a ready-to-paste `cast send` claim command.
// Building a shell command out of proxy-supplied strings put those strings one
// quote away from execution on the operator's machine, and a pasted command also
// skips the on-chain pre-verification that the real claim path performs. Use
// ./scripts/claim-reward.sh (or cmd/claim-reward) instead: it validates the
// proof, confirms the contract accepts it via a static call, and submits the
// transaction itself.
//
// Usage:
//
//	go run ./cmd/get-result -id 0x<instructionId> [-p <proxy-url>]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"activity-reward-extension/tools/pkg/fccutils"

	"github.com/ethereum/go-ethereum/common"
)

const PROXY_URL = "http://localhost:6664" // default extension proxy url

// maxSignatureLen bounds the signature before it is shown. A 65-byte ECDSA
// signature is what the contract accepts; the cap only stops an unbounded field.
const maxSignatureLen = 1024

// proof mirrors the JSON the extension returns. The fields arrive as strings and
// are parsed strictly below — the struct is the wire shape, not the trusted one.
type proof struct {
	Timestamp     int64   `json:"timestamp"`
	Challenge     string  `json:"challenge"`
	Caller        string  `json:"caller"`
	TeeID         string  `json:"teeId"`
	Eligible      bool    `json:"eligible"`
	DistanceKm    float64 `json:"distanceKm"`
	DistanceX1000 int64   `json:"distanceX1000"`
	MonthStart    int64   `json:"monthStart"`
	AthleteHash   string  `json:"athleteHash"`
	Signature     string  `json:"signature"`
	Message       string  `json:"message"`
}

// validated is the same proof after every field has been checked. Printing from
// these re-encoded values means the output is constrained by construction rather
// than by escaping whatever the proxy sent.
type validated struct {
	Challenge   common.Hash
	Caller      common.Address
	TeeID       common.Address
	AthleteHash common.Hash
	Signature   []byte
}

func main() {
	pf := flag.String("p", PROXY_URL, "proxy url")
	id := flag.String("id", "", "instruction id")
	flag.Parse()

	// Same transport bar as the other tools that read from the proxy: over plain
	// HTTP a network attacker supplies this response outright.
	if err := fccutils.RequireSecureProxyURL(*pf); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}

	instructionID, err := fccutils.StrictHash("-id", *id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}

	resp, err := fccutils.ActionResult(*pf, instructionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}

	fmt.Printf("status: %d  log: %s\n", resp.Result.Status, fccutils.SanitizeForTerminal(resp.Result.Log))
	if len(resp.Result.Data) == 0 {
		return
	}
	fmt.Printf("data: %s\n", fccutils.SanitizeForTerminal(string(resp.Result.Data)))

	var p proof
	if err := json.Unmarshal(resp.Result.Data, &p); err != nil || p.Signature == "" {
		// Not a distance proof — the sanitized data dump above is all there is.
		return
	}

	v, err := validateProof(&p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nWARNING: this result looks like a distance proof but does not "+
			"validate: %s\n  Treat it as untrusted and do not attempt to claim with it.\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("message:       %s\n", fccutils.SanitizeForTerminal(p.Message))
	fmt.Printf("eligible:      %t\n", p.Eligible)
	fmt.Printf("distance:      %d m (monthly total)\n", p.DistanceX1000)
	fmt.Printf("timestamp:     %d\n", p.Timestamp)
	fmt.Printf("monthStart:    %d\n", p.MonthStart)
	fmt.Printf("caller:        %s\n", v.Caller.Hex())
	fmt.Printf("teeId:         %s\n", v.TeeID.Hex())
	fmt.Printf("challenge:     %s\n", v.Challenge.Hex())
	fmt.Printf("athleteHash:   %s\n", v.AthleteHash.Hex())
	fmt.Printf("signature:     %d bytes\n", len(v.Signature))
	fmt.Println()
	fmt.Println("To claim, run the claim path — it re-verifies this proof on-chain before spending gas:")
	fmt.Printf("  ./scripts/claim-reward.sh          # or: cd tools && go run ./cmd/claim-reward -id %s\n", instructionID.Hex())
	fmt.Println("Claim from the same wallet that requested the proof, within the freshness window.")
}

// validateProof parses every untrusted field exactly, and screens the one field it
// cannot parse — the unsigned distanceKm — for agreement with the signed
// distanceX1000. A proof that fails here is not merely unusable: it did not come
// from a well-behaved extension, so the caller should stop rather than display it
// as if it were genuine.
func validateProof(p *proof) (*validated, error) {
	challenge, err := fccutils.StrictHash("challenge", p.Challenge)
	if err != nil {
		return nil, err
	}
	athleteHash, err := fccutils.StrictHash("athleteHash", p.AthleteHash)
	if err != nil {
		return nil, err
	}
	caller, err := fccutils.StrictAddress("caller", p.Caller)
	if err != nil {
		return nil, err
	}
	teeID, err := fccutils.StrictAddress("teeId", p.TeeID)
	if err != nil {
		return nil, err
	}
	signature, err := fccutils.StrictBytes("signature", p.Signature, maxSignatureLen)
	if err != nil {
		return nil, err
	}
	if p.Timestamp < 0 || p.MonthStart < 0 || p.DistanceX1000 < 0 {
		return nil, fmt.Errorf("negative timestamp, monthStart or distance")
	}
	if err := fccutils.CheckDistanceAgreement(p.DistanceKm, p.DistanceX1000); err != nil {
		return nil, err
	}
	return &validated{
		Challenge:   challenge,
		Caller:      caller,
		TeeID:       teeID,
		AthleteHash: athleteHash,
		Signature:   signature,
	}, nil
}
