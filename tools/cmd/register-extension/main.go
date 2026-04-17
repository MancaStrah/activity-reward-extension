package main

import (
	"flag"
	"fmt"

	"activity-reward-extension/tools/pkg/configs"
	"activity-reward-extension/tools/pkg/fccutils"
	"activity-reward-extension/tools/pkg/support"
	"activity-reward-extension/tools/pkg/validate"

	"github.com/ethereum/go-ethereum/common"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
)

func main() {
	af := flag.String("a", configs.AddressesFile, "file with deployed addresses")
	cf := flag.String("c", configs.ChainNodeURL, "chain node url")
	instructionSenderF := flag.String("instructionSender", "", "InstructionSender contract address (required)")
	governanceHashF := flag.String("governanceHash", "", "governance hash (optional)")
	flag.Parse()

	if *instructionSenderF == "" {
		logger.Fatal("--instructionSender flag is required")
	}

	testSupport, err := support.DefaultSupport(*af, *cf)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// Both values are written on-chain, and the lenient go-ethereum helpers turn
	// malformed input into a well-formed-looking hash or address by zero-padding
	// and truncating — so a mistyped governance hash would be registered as a
	// different, valid-looking commitment rather than refused. Parse exactly.
	// The governance hash stays optional: unset means the zero hash, which is the
	// registry's own "no governance commitment" value, and only a value that was
	// actually supplied has to parse.
	var governanceHash common.Hash
	if *governanceHashF != "" {
		governanceHash, err = fccutils.StrictHash("--governanceHash", *governanceHashF)
		if err != nil {
			fccutils.FatalWithCause(err)
		}
	}
	instructionSenderAddress, err := fccutils.StrictAddress("--instructionSender", *instructionSenderF)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// Pre-flight: verify instruction sender has code on-chain
	if err := validate.AddressHasCode(testSupport.ChainClient, instructionSenderAddress, "InstructionSender"); err != nil {
		fccutils.FatalWithCause(err)
	}

	logger.Infof("Registering extension with InstructionSender %s...", instructionSenderAddress.Hex())
	extensionID, err := fccutils.SetupExtension(testSupport, governanceHash, instructionSenderAddress, common.Address{})
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	if extensionID == nil || extensionID.Sign() <= 0 {
		logger.Warnf("WARNING: extension ID is %v — verify this is expected", extensionID)
	}

	extensionIDHex := fmt.Sprintf("0x%064x", extensionID)
	logger.Infof("Extension registered with ID: %s", extensionIDHex)

	// Machine-readable output on stdout
	fmt.Println(extensionIDHex)
}
