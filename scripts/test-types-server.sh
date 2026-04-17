#!/usr/bin/env bash
# test-types-server.sh — Run decode tests against a running types-server.
#
# Usage: ./scripts/test-types-server.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

TYPES_SERVER_URL="${TYPES_SERVER_URL:-http://localhost:8100}"

cd "$PROJECT_DIR/tools"
go run ./cmd/test-types-server -t "$TYPES_SERVER_URL"
