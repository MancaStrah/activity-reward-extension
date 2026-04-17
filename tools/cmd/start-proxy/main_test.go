package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindProxyConfigHonoursTheValidatedPath pins the reason PROXY_CONFIG exists.
//
// start-services.sh resolves the config for the selected chain, puts its
// [attestation] section through the profile gate, and exports the path. If this
// process rediscovered a path of its own instead, the file it opened could differ
// from the file that was checked — which is how the gate came to cover the Docker
// deployment path only.
func TestFindProxyConfigHonoursTheValidatedPath(t *testing.T) {
	want := filepath.Join(t.TempDir(), "checked-by-the-script.toml")
	t.Setenv("PROXY_CONFIG", want)
	// Set deliberately to values that would otherwise select something else, so the
	// test fails if the env var is ever demoted below the chain lookup.
	t.Setenv("CHAIN", "coston2")
	t.Setenv("LOCAL_MODE", "true")

	if got := findProxyConfig(); got != want {
		t.Fatalf("PROXY_CONFIG must win outright: got %q, want %q", got, want)
	}
}

// TestFindProxyConfigIsChainAware pins the standalone lookup — a bare
// `go run ./cmd/start-proxy` with no script driving it.
//
// The lookup used to return extension_proxy.toml whatever the chain was, so a
// testnet run loaded the local-devnet config together with whatever [attestation]
// posture that file happened to carry.
func TestFindProxyConfigIsChainAware(t *testing.T) {
	for _, c := range []struct {
		chain string
		want  string
	}{
		{"local", "extension_proxy.toml"},
		{"coston", "extension_proxy.coston.toml"},
		{"coston2", "extension_proxy.coston2.toml"},
	} {
		t.Run(c.chain, func(t *testing.T) {
			t.Setenv("PROXY_CONFIG", "")
			t.Setenv("CHAIN", c.chain)
			// The path may be absolute (the file is present in this checkout) or
			// relative (it is gitignored and absent); only the name is fixed.
			if got := filepath.Base(findProxyConfig()); got != c.want {
				t.Fatalf("chain %q: got %q, want %q", c.chain, got, c.want)
			}
		})
	}
}

// TestProxyConfigChainMirrorsTheScript pins the fallback when CHAIN is unset,
// including the two spellings that look like bugs and are not: start-services.sh
// defaults LOCAL_MODE to "true" when empty, and compares it case-sensitively. This
// function has to pick the file that script would have picked, so mirroring it beats
// normalising it — a more tolerant comparison here would make the two disagree on
// LOCAL_MODE=True, which is the class of drift the whole arrangement closes.
func TestProxyConfigChainMirrorsTheScript(t *testing.T) {
	for _, c := range []struct {
		name      string
		localMode string
		want      string
	}{
		{"unset defaults to the local devnet", "", "local"},
		{"true selects the local devnet", "true", "local"},
		{"false falls back to the legacy chain", "false", "coston2"},
		{"True is not true, matching the script's test", "True", "coston2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CHAIN", "")
			t.Setenv("LOCAL_MODE", c.localMode)
			got, ok := proxyConfigChain()
			if !ok {
				t.Fatalf("LOCAL_MODE=%q: the fallback must always be a usable chain name, got %q", c.localMode, got)
			}
			if got != c.want {
				t.Fatalf("LOCAL_MODE=%q: got chain %q, want %q", c.localMode, got, c.want)
			}
		})
	}
}

// TestProxyConfigNameAgreesWithTheShellMapping is the paired cross-language pin.
//
// The host-mode filename is defined twice — proxy_config_name() in
// scripts/lib/profile.sh, which is what the deployment scripts and the profile
// matrix use, and findProxyConfig() here, which is what a standalone run uses. The
// duplication is deliberate (tools/ carries no dependency on scripts/), so like the
// grant layout and the sign-payload vector it is pinned by a test that asserts both
// sides produce the same answer rather than by a comment asking for it.
func TestProxyConfigNameAgreesWithTheShellMapping(t *testing.T) {
	profileSh := filepath.Join(projectRoot(), "scripts", "lib", "profile.sh")
	if _, err := os.Stat(profileSh); err != nil {
		t.Skipf("scripts/lib/profile.sh not reachable from %s: %v", projectRoot(), err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}

	for _, chain := range []string{"local", "coston", "coston2"} {
		t.Run(chain, func(t *testing.T) {
			out, err := exec.Command(bash, "-c",
				`set -euo pipefail; source "$1"; proxy_config_name host "$2"`,
				"bash", profileSh, chain).Output()
			if err != nil {
				t.Fatalf("proxy_config_name host %s failed: %v", chain, err)
			}
			shell := string(out)

			t.Setenv("PROXY_CONFIG", "")
			t.Setenv("CHAIN", chain)
			if got := filepath.Base(findProxyConfig()); got != shell {
				t.Fatalf("chain %q: Go says %q, scripts/lib/profile.sh says %q — the two mappings have drifted, "+
					"so the file the script validates is not the file this binary opens",
					chain, got, shell)
			}
		})
	}
}

// TestProxyConfigChainRefusesAnythingButAChainName pins the fail-closed direction on
// a CHAIN that is not a chain name. Falling back to the local-devnet config would be
// the chain-blind behaviour this lookup was changed to stop — it would pick a
// different file, with its own [attestation] posture — so an unusable CHAIN has to be
// refused rather than substituted.
func TestProxyConfigChainRefusesAnythingButAChainName(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd", // escapes config/proxy entirely
		"..",               // the parent directory
		"coston2/../local", // normalises to a different file
		"coston 2",         // a space
		"Coston2",          // uppercase: the shell mapping never produces this
		"coston2.docker",   // would cross into the container config
		"-coston2",         // leading separator
		"coston2\n",        // trailing newline from a mis-set env var
	} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("CHAIN", bad)
			if _, ok := proxyConfigChain(); ok {
				t.Fatalf("CHAIN=%q must be refused, not interpolated into a config filename", bad)
			}
		})
	}

	// And the three real ones are still accepted, so the rule above rejects by shape
	// rather than by rejecting everything.
	for _, good := range []string{"local", "coston", "coston2"} {
		t.Run("accepts/"+good, func(t *testing.T) {
			t.Setenv("CHAIN", good)
			got, ok := proxyConfigChain()
			if !ok || got != good {
				t.Fatalf("CHAIN=%q must be accepted verbatim, got (%q, %v)", good, got, ok)
			}
		})
	}
}

// TestFindProxyConfigDiesOnAnUnusableChain pins the refusal itself, not just the
// predicate behind it. findProxyConfig ends the process for an unusable CHAIN, which a
// same-process test cannot observe, so this re-execs the test binary as a child and
// asserts the child died — the standard way to cover a fatal path, and worth the extra
// lines here because the alternative behaviour (fall back to the local-devnet config)
// is exactly the defect this lookup was changed to close.
func TestFindProxyConfigDiesOnAnUnusableChain(t *testing.T) {
	const childMarker = "PROXY_CONFIG_FATAL_CHILD"

	if os.Getenv(childMarker) == "1" {
		findProxyConfig()
		// Only reachable if the refusal was skipped; the parent reads exit 0 as failure.
		os.Exit(0)
	}

	// Built explicitly rather than appended to os.Environ(): a duplicate key in an
	// exec environment is resolved by the OS, not by append order, so the values
	// under test are set by filtering the inherited ones out first.
	env := []string{childMarker + "=1"}
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "CHAIN="),
			strings.HasPrefix(kv, "PROXY_CONFIG="),
			strings.HasPrefix(kv, childMarker+"="):
		default:
			env = append(env, kv)
		}
	}
	env = append(env, "CHAIN=../../etc/passwd")

	cmd := exec.Command(os.Args[0], "-test.run=^TestFindProxyConfigDiesOnAnUnusableChain$")
	cmd.Env = env
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("findProxyConfig returned for CHAIN=../../etc/passwd instead of refusing:\n%s", out)
	}
	if !strings.Contains(string(out), "not a plain chain name") {
		t.Fatalf("the child died, but not with the refusal this test is about:\n%s", out)
	}
}
