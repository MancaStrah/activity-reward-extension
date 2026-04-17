// Command set-extension-id binds a freshly deployed StravaInstructionSender to its
// on-chain extension id (owner-only, one-shot). Running it from pre-build.sh right
// after registration makes deploy -> register -> bind a single operator-driven
// sequence with no public gap in which a pre-registration attack could act.
//
// The id is pinned with -extension-id (the value register-extension produced). If
// omitted it is resolved from the registry, which refuses to guess when the address
// is registered under more than one extension — the duplicate-registration signal.
package main

import (
	"flag"
	"fmt"
	"math/big"
	"os"

	"activity-reward-extension/tools/pkg/configs"
	"activity-reward-extension/tools/pkg/support"
	instrutils "activity-reward-extension/tools/pkg/utils"

	"github.com/ethereum/go-ethereum/common"
)

func main() {
	af := flag.String("a", configs.AddressesFile, "deployed addresses file")
	cf := flag.String("c", configs.ChainNodeURL, "chain rpc url")
	senderF := flag.String("instructionSender", "", "StravaInstructionSender address")
	idF := flag.String("extension-id", "", "extension id to bind (decimal or 0x); default resolves it from the registry")
	flag.Parse()

	if !common.IsHexAddress(*senderF) {
		fmt.Fprintln(os.Stderr, "invalid or missing -instructionSender")
		os.Exit(1)
	}
	addr := common.HexToAddress(*senderF)

	s, err := support.DefaultSupport(*af, *cf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain support: %v\n", err)
		os.Exit(1)
	}

	var expectedID *big.Int
	if *idF != "" {
		expectedID = new(big.Int)
		if _, ok := expectedID.SetString(*idF, 0); !ok {
			fmt.Fprintf(os.Stderr, "invalid -extension-id %q: expected a decimal or 0x id\n", *idF)
			os.Exit(1)
		}
	} else {
		expectedID, err = instrutils.ResolveExtensionId(s, addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolving extension id (pass -extension-id to pin it): %v\n", err)
			os.Exit(1)
		}
	}

	if err := instrutils.SetExtensionId(s, addr, expectedID); err != nil {
		fmt.Fprintf(os.Stderr, "setExtensionId failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Bound %s to extension %s\n", addr.Hex(), expectedID.String())
}
