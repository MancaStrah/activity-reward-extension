// TEMPORARY: This command starts the extension proxy as a Go process.
// It will be replaced by a Docker container once the Dockerfile is implemented.
// See EXTENSION-TEMPLATE-SPEC.md §5 for the Docker approach.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"activity-reward-extension/tools/pkg/fccutils"

	"github.com/flare-foundation/go-flare-common/pkg/logger"
	proxyConfig "github.com/flare-foundation/tee-proxy/pkg/config"
	initProxy "github.com/flare-foundation/tee-proxy/pkg/init"
	"github.com/joho/godotenv"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loadEnv()

	proxyConfigFile := findProxyConfig()
	// Logged because which config is in force decides the proxy's whole attestation
	// posture, and the resolution above has three possible sources.
	logger.Infof("Proxy config: %s", proxyConfigFile)

	initProxy.Init(ctx, proxyConfigFile)
	logger.Infof("Started extension proxy")

	err := logProxyAndTeeIds(proxyConfigFile)
	if err != nil {
		logger.Warnf("Failed to log proxy and tee IDs: %v", err)
	}

	<-ctx.Done()
	logger.Infof("Received shutdown signal, shutting down")
}

func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

func loadEnv() {
	// Try project-root .env first (works even when CWD is tools/).
	rootEnv := filepath.Join(projectRoot(), ".env")
	if err := godotenv.Load(rootEnv); err != nil {
		// Fallback to CWD .env.
		if err := godotenv.Load(); err != nil {
			fmt.Printf("Warning: Error loading .env file: %v\n", err)
		}
	}
}

// findProxyConfig resolves the config file this proxy will read.
//
// PROXY_CONFIG is the authority when it is set, and start-services.sh always sets
// it: that script resolves the path for the selected chain and puts its
// [attestation] section through the profile gate, so honouring the variable is what
// makes the file this process opens the same file that was checked. Rediscovering a
// path here would let the two disagree, which is how the gate came to cover the
// Docker path only.
//
// Without it — a standalone `go run ./cmd/start-proxy` — the chain decides, mirroring
// proxy_config_name() in scripts/lib/profile.sh. A chain-blind default loaded the
// local-devnet config on a testnet run, and with it whatever posture that file
// happened to carry.
func findProxyConfig() string {
	if envConfig := os.Getenv("PROXY_CONFIG"); envConfig != "" {
		return envConfig
	}

	chain, ok := proxyConfigChain()
	if !ok {
		// Refuse rather than fall back to the local-devnet config: falling back is
		// exactly the chain-blind behaviour this lookup was changed to stop, and it
		// would pick a file with its own [attestation] posture.
		logger.Fatalf("CHAIN=%q is not a plain chain name (letters, digits and - only), so it will not be "+
			"interpolated into a config filename. Fix CHAIN, or set PROXY_CONFIG to the path you mean.",
			os.Getenv("CHAIN"))
	}

	name := "extension_proxy.toml"
	if chain != "local" {
		name = fmt.Sprintf("extension_proxy.%s.toml", chain)
	}

	// Try project-root relative path first.
	candidate := filepath.Join(projectRoot(), "config", "proxy", name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Fallback to working directory relative.
	return filepath.Join(".", "config", "proxy", name)
}

// proxyConfigChain resolves the chain the way start-services.sh does, deliberately
// including its case-sensitive LOCAL_MODE test and its legacy coston2 fallback. The
// point of this function is to pick the file that script would have picked, so
// "mirror it exactly" beats "normalise it better" — a more tolerant comparison here
// would make the two disagree on LOCAL_MODE=True, which is precisely the class of
// drift this whole change closes.
// It reports ok=false when CHAIN is set to something that is not a plain chain name.
// The value is interpolated into a filename, so it is constrained rather than passed
// through: no privilege boundary is crossed here — whoever sets CHAIN can set
// PROXY_CONFIG to any path outright — but a value carrying a separator or a `..`
// would quietly resolve to a file somewhere else, and "quietly reads a different
// config" is the failure this lookup exists to stop. Deliberately a charset rule
// rather than a list of the three known chains: adding a chain should not mean
// editing this.
func proxyConfigChain() (chain string, ok bool) {
	if c := os.Getenv("CHAIN"); c != "" {
		return c, validChainName(c)
	}
	// start-services.sh: LOCAL_MODE="${LOCAL_MODE:-true}", then true → local,
	// anything else → the legacy coston2 default.
	if localMode := os.Getenv("LOCAL_MODE"); localMode == "" || localMode == "true" {
		return "local", true
	}
	return "coston2", true
}

// validChainName accepts the shape of a chain name and nothing else: it must start
// with a lowercase letter or digit and may continue with those plus '-'. That rules
// out path separators, '.', '..' and anything else that would change which directory
// the filename resolves in.
func validChainName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

func logProxyAndTeeIds(configFile string) error {
	config, err := proxyConfig.Read(configFile)
	if err != nil {
		return fmt.Errorf("failed to read proxy config: %w", err)
	}

	proxyURL := fmt.Sprintf("http://localhost:%s", config.Ports.External)

	teeID, proxyID, err := fccutils.GetTeeProxyID(proxyURL)
	if err != nil {
		return fmt.Errorf("failed to extract teeID and proxyID: %w", err)
	}

	logger.Infof("Proxy started - TeeID: %s, ProxyID: %s", teeID.Hex(), proxyID.Hex())
	return nil
}
