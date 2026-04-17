package main

import (
	"activity-reward-extension/tools/pkg/configs"
	"activity-reward-extension/tools/pkg/fccutils"
	"activity-reward-extension/tools/pkg/support"
	"crypto/ecdsa"
	"flag"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
	"github.com/pkg/errors"
	"os"
	"strings"
)

func main() {
	af := flag.String("a", configs.AddressesFile, "file with deployed addresses")
	cf := flag.String("c", configs.ChainNodeURL, "chain node url")
	pf := flag.String("p", configs.ExtensionProxyURL, "proxy url")
	versionF := flag.String("version", "v0.1.0", "version")
	expectedCodeHashF := flag.String("expected-codehash", "",
		"REQUIRED on confidential-space: the measured image code hash from YOUR build (0x-hex); the proxy /info value must match it")
	expectedPlatformF := flag.String("expected-platform", "",
		"REQUIRED on confidential-space: the measured platform hash from YOUR build (0x-hex); the proxy /info value must match it")
	flag.Parse()

	// This tool learns the codeHash from the proxy's own /info — circular
	// trust. Simulated profiles may proceed (the value is a dev constant
	// there); a real deployment must supply an independent expectation from
	// its own build. See docs/production-allowlisting.md.
	teeProfile := os.Getenv("TEE_PROFILE")
	if err := fccutils.AllowlistGate(teeProfile, os.Getenv("LOCAL_MODE"), *expectedCodeHashF); err != nil {
		fccutils.FatalWithCause(err)
	}
	if teeProfile == "confidential-space" {
		// The platform is half of the (codeHash, platform) pair this tool
		// allow-lists, and it comes from the same proxy /info as the codeHash.
		// Without an independent expectation the platform side of the pair is
		// whatever the proxy claims, so require it too.
		if *expectedPlatformF == "" {
			fccutils.FatalWithCause(errors.New(
				"TEE_PROFILE=confidential-space: refusing to allow-list a platform read from the proxy's own /info — " +
					"pass -expected-platform with the measured platform from YOUR build; see docs/production-allowlisting.md"))
		}
		// Defense in depth: the /info cross-check below only helps if the
		// transport cannot be rewritten in transit.
		if err := fccutils.RequireSecureProxyURL(*pf); err != nil {
			fccutils.FatalWithCause(err)
		}
	}

	testSupport, err := support.DefaultSupport(*af, *cf)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// get teeID from proxy
	teeInfo, err := fccutils.TeeInfo(*pf)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	// Cross-check /info against the operator's expectation before anything is
	// written on-chain: the proxy never gets to promote its own value.
	//
	// Both expectations are parsed STRICTLY. They are the whole point of this
	// tool — the operator's independent measurement, the only thing standing
	// between a proxy-reported codeHash and an on-chain allow-list entry — and
	// common.HexToHash discards its decode error, so a value this tool cannot
	// read (a "sha256:" prefix copied from the proxy config template, a stray
	// newline, one mistyped digit) would silently become the zero hash rather
	// than an error. Comparing against zero is fail-closed in the ordinary case,
	// but it is not the check the operator asked for: it fails open against a
	// proxy that also reports zero, and the mismatch message would quote the
	// mangled value instead of what was typed. Refuse to guess.
	if *expectedCodeHashF != "" {
		expected, err := fccutils.StrictHash("-expected-codehash", *expectedCodeHashF)
		if err != nil {
			fccutils.FatalWithCause(errors.Errorf(
				"%s — pass the measured image digest as 32 bytes of hex; refusing to allow-list against a value that cannot be read", err))
		}
		if expected != teeInfo.MachineData.CodeHash {
			fccutils.FatalWithCause(errors.Errorf(
				"code hash mismatch: proxy /info reports %s but -expected-codehash is %s — refusing to allow-list. "+
					"Rebuild/verify your image and retry with the measured hash",
				teeInfo.MachineData.CodeHash.Hex(), expected.Hex()))
		}
		logger.Infof("Code hash matches operator expectation %s", expected.Hex())
	}
	if *expectedPlatformF != "" {
		expected, err := fccutils.StrictHash("-expected-platform", *expectedPlatformF)
		if err != nil {
			fccutils.FatalWithCause(errors.Errorf(
				"%s — pass the measured platform as 32 bytes of hex; refusing to allow-list against a value that cannot be read", err))
		}
		if expected != teeInfo.MachineData.Platform {
			fccutils.FatalWithCause(errors.Errorf(
				"platform mismatch: proxy /info reports %s but -expected-platform is %s — refusing to allow-list",
				teeInfo.MachineData.Platform.Hex(), expected.Hex()))
		}
	}

	var privKey *ecdsa.PrivateKey
	privKeyString := os.Getenv("EXTENSION_OWNER_KEY")
	if privKeyString != "" {
		if strings.HasPrefix(privKeyString, "0x") || strings.HasPrefix(privKeyString, "0X") {
			privKeyString = privKeyString[2:]
		}
		privKey, err = crypto.HexToECDSA(privKeyString)
		if err != nil {
			fccutils.FatalWithCause(err)
		}
	} else {
		privKey = testSupport.Prv
	}

	keySource := "EXTENSION_OWNER_KEY"
	if privKeyString == "" {
		keySource = "DEPLOYMENT_PRIVATE_KEY (default)"
	}
	logger.Infof("Using key: %s (deployer: %s)", keySource, crypto.PubkeyToAddress(privKey.PublicKey).Hex())

	teeID, _, err := fccutils.TeeProxyId(teeInfo)
	if err != nil {
		fccutils.FatalWithCause(err)
	}

	logger.Infof("Code hash:    %s (source: proxy /info)", teeInfo.MachineData.CodeHash.Hex())
	logger.Infof("Platform:     %s (source: proxy /info)", teeInfo.MachineData.Platform.Hex())
	logger.Infof("Extension ID: %s", teeInfo.MachineData.ExtensionID.Big().String())
	logger.Infof("TEE ID:       %s", teeID.Hex())
	logger.Infof("Version:      %s", *versionF)
	if *expectedCodeHashF == "" {
		logger.Warnf("NOTE: Code hash is from proxy /info response — not independently verified against attestation (simulated profile)")
	}

	// Idempotency: skip if this codeHash+platform combo is already registered and active.
	// Avoids sending a tx that would revert with VersionAlreadyExists() on re-runs.
	supported, err := testSupport.TeeExtensionRegistry.IsCodeHashPlatformSupported(
		nil,
		teeInfo.MachineData.ExtensionID.Big(),
		teeInfo.MachineData.CodeHash,
		teeInfo.MachineData.Platform,
	)
	if err != nil {
		fccutils.FatalWithCause(err)
	}
	if supported {
		logger.Infof("version already registered for this code hash + platform, skipping")
		return
	}

	err = fccutils.AddTeeVersion(testSupport, privKey, teeInfo.MachineData.ExtensionID.Big(), teeInfo.MachineData.CodeHash, teeInfo.MachineData.Platform, *versionF)
	if err != nil {
		if strings.Contains(err.Error(), "VersionAlreadyExists") {
			logger.Infof("version already registered, skipping")
		} else {
			fccutils.FatalWithCause(err)
		}
	}
}
