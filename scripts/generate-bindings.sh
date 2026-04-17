#!/usr/bin/env bash
# generate-bindings.sh — Compile Solidity contracts and generate Go bindings.
#
# Prerequisites: forge (Foundry), jq
#
# Usage: ./scripts/generate-bindings.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# --- Contract name and Go package ---
CONTRACT_NAME="StravaInstructionSender"
GO_PKG="strava"
BINDINGS_DIR="$PROJECT_DIR/tools/pkg/contracts/$GO_PKG"

cd "$PROJECT_DIR"

# Resolve forge: PATH first, then the default foundryup install location.
if command -v forge >/dev/null 2>&1; then
    FORGE=forge
elif [[ -x "$HOME/.foundry/bin/forge" ]]; then
    FORGE="$HOME/.foundry/bin/forge"
else
    echo "ERROR: forge not found on PATH or in ~/.foundry/bin."
    echo "Install Foundry (https://getfoundry.sh) or add it to PATH:"
    echo '  export PATH="$HOME/.foundry/bin:$PATH"'
    exit 1
fi

echo "=== Step 1: Compile Solidity contracts ==="
"$FORGE" build

# Verify the contract name in the source matches what we expect
if ! grep -q "contract ${CONTRACT_NAME}" "$PROJECT_DIR/contracts/InstructionSender.sol" 2>/dev/null; then
    echo ""
    echo "ERROR: Contract name '${CONTRACT_NAME}' not found in contracts/InstructionSender.sol."
    echo "Make sure the contract name in InstructionSender.sol matches CONTRACT_NAME in this script."
    exit 1
fi

echo "=== Step 2: Extract ABI and BIN ==="
FORGE_OUT="$PROJECT_DIR/out/InstructionSender.sol/${CONTRACT_NAME}.json"
if [[ ! -f "$FORGE_OUT" ]]; then
    echo "ERROR: forge output not found at $FORGE_OUT"
    echo "Check that CONTRACT_NAME matches your Solidity contract name."
    exit 1
fi

mkdir -p "$BINDINGS_DIR"

# Extract ABI (JSON array)
jq '.abi' "$FORGE_OUT" > "$BINDINGS_DIR/${CONTRACT_NAME}.abi"

# Extract bytecode (hex string, strip 0x prefix)
jq -r '.bytecode.object' "$FORGE_OUT" | sed 's/^0x//' > "$BINDINGS_DIR/${CONTRACT_NAME}.bin"

echo "  ABI → $BINDINGS_DIR/${CONTRACT_NAME}.abi"
echo "  BIN → $BINDINGS_DIR/${CONTRACT_NAME}.bin"

echo "=== Step 3: Generate Go bindings ==="
cd "$PROJECT_DIR/tools"
go generate ./pkg/contracts/$GO_PKG/

echo "=== Done ==="
echo "Generated: $BINDINGS_DIR/autogen.go"
