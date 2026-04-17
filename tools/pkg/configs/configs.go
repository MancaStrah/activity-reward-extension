package configs

import (
	"crypto/ecdsa"
	"encoding/json"
	"os"

	"github.com/ethereum/go-ethereum/crypto"
)

const (
	ExtensionProxyURL = "http://localhost:6664"
	ChainNodeURL      = "http://127.0.0.1:8545"
)

const (
	AddressesFile            = "../docker/sim_dump/deployed-addresses.json"
	ExtensionProxyConfigFile = "./configs/proxy/extension_proxy.toml"
)

const (
	ExtConfigurationPort = 5501 // port on tee for setting the configurations (proxyURL, initialOwner, extensionID)
	ExtProxyInternalPort = 6663 // internal port for tee to get actions from the queue from the proxy
	ExtensionServerPort  = 7701 // port on the tee that the extension server calls for signing, encrypting, etc.
	ExtensionPort        = 7702 // the port on the extension server that the tee calls to send non system actions
)

// devPrivateKeyHex is the well-known local-devnet funded key (Hardhat/Anvil). It
// has funds ONLY on a local devnet. It is deliberately NOT parsed at package
// init(): importing this package must never materialize key material or panic on a
// malformed literal (the previous init() did exactly that once the value was
// scrubbed for a public push). Parsing happens lazily in DevPrivateKey(), and
// callers must gate its use behind an explicit local-mode opt-in — see
// support.DefaultPrivateKey.
const devPrivateKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// DevPrivateKey parses and returns the local-devnet funded key on demand. It is a
// convenience for local testing only; testnet/production callers must supply a real
// key via DEPLOYMENT_PRIVATE_KEY and never reach for this.
func DevPrivateKey() (*ecdsa.PrivateKey, error) {
	return crypto.HexToECDSA(devPrivateKeyHex)
}

func ReadAddresses[T any](filePath string, dest *T) error {
	file, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	err = json.Unmarshal(file, dest)

	return err
}
