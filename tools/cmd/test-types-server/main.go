// Command test-types-server exercises a running types-server's /decode endpoint.
//
// The ABI layouts are declared locally: tools/ asserts on the wire format and
// stays independent of the extension implementation (see docs/extension-contract.md).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flare-foundation/go-flare-common/pkg/logger"
)

type decodeRequest struct {
	OPType    string `json:"opType"`
	OPCommand string `json:"opCommand"`
	Kind      string `json:"kind"`
	Data      string `json:"data"`
}

// Flat ABI layout matching the contract's abi.encode(...) call in getDistanceProof():
// abi.encode(bytes32 challenge, address caller, address verifyingContract,
//
//	uint256 chainId, bytes encryptedToken)
var distanceMsgArgs abi.Arguments

func init() {
	bytes32Ty, _ := abi.NewType("bytes32", "", nil)
	addressTy, _ := abi.NewType("address", "", nil)
	uint256Ty, _ := abi.NewType("uint256", "", nil)
	bytesTy, _ := abi.NewType("bytes", "", nil)

	distanceMsgArgs = abi.Arguments{
		{Name: "challenge", Type: bytes32Ty},
		{Name: "caller", Type: addressTy},
		{Name: "verifyingContract", Type: addressTy},
		{Name: "chainId", Type: uint256Ty},
		{Name: "encryptedToken", Type: bytesTy},
	}
}

func main() {
	tf := flag.String("t", "http://localhost:8100", "types server URL")
	flag.Parse()

	baseURL := *tf

	passed := 0
	failed := 0

	run := func(name string, fn func() error) {
		logger.Infof("TEST: %s", name)
		if err := fn(); err != nil {
			logger.Errorf("  FAIL: %s", err)
			failed++
		} else {
			logger.Infof("  PASS")
			passed++
		}
	}

	// --- Success cases ---

	run("DISTANCE message (ABI-encoded)", func() error {
		var challenge [32]byte
		challenge[0] = 0x01
		caller := common.HexToAddress("0x00000000000000000000000000000000000000B2")
		contract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
		encoded, err := distanceMsgArgs.Pack(challenge, caller, contract, big.NewInt(114), []byte("test-token"))
		if err != nil {
			return fmt.Errorf("ABI encode: %w", err)
		}
		data := hexutil.Encode(encoded)
		_, err = postDecode(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "DISTANCE", Kind: "message", Data: data,
		})
		return err
	})

	run("DISTANCE result (JSON)", func() error {
		payload, _ := json.Marshal(map[string]any{"distanceKm": 15.3, "eligible": false, "message": "4.7 km more."})
		data := hexutil.Encode(payload)
		resp, err := postDecode(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "DISTANCE", Kind: "result", Data: data,
		})
		if err != nil {
			return err
		}
		return requireField(resp, "message", "4.7 km more.")
	})

	// --- Error cases ---

	run("removed REWARD command → 404", func() error {
		return expectStatus(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "REWARD", Kind: "result", Data: "0x7b7d",
		}, http.StatusNotFound)
	})

	run("unknown OPType → 404", func() error {
		return expectStatus(baseURL, decodeRequest{
			OPType: "UNKNOWN", OPCommand: "", Kind: "message", Data: "0x7b7d",
		}, http.StatusNotFound)
	})

	run("invalid kind → 400", func() error {
		return expectStatus(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "DISTANCE", Kind: "invalid", Data: "0x7b7d",
		}, http.StatusBadRequest)
	})

	run("invalid hex → 400", func() error {
		return expectStatus(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "DISTANCE", Kind: "message", Data: "not-hex",
		}, http.StatusBadRequest)
	})

	run("valid hex, bad JSON payload → 422", func() error {
		return expectStatus(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "DISTANCE", Kind: "result", Data: "0xdeadbeef",
		}, http.StatusUnprocessableEntity)
	})

	run("truncated ABI message → 422", func() error {
		return expectStatus(baseURL, decodeRequest{
			OPType: "STRAVA", OPCommand: "DISTANCE", Kind: "message", Data: "0x0102",
		}, http.StatusUnprocessableEntity)
	})

	// --- Summary ---
	logger.Infof("")
	logger.Infof("Results: %d passed, %d failed", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// postDecode sends a POST /decode request and returns the "decoded" field from the response.
func postDecode(baseURL string, req decodeRequest) (map[string]any, error) {
	body, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+"/decode", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /decode: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Decoded map[string]any `json:"decoded"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return result.Decoded, nil
}

// expectStatus sends a POST /decode and asserts the HTTP status code.
func expectStatus(baseURL string, req decodeRequest, wantStatus int) error {
	body, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+"/decode", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /decode: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck

	if resp.StatusCode != wantStatus {
		return fmt.Errorf("expected status %d, got %d", wantStatus, resp.StatusCode)
	}
	return nil
}

// requireField asserts a string field in the decoded response.
func requireField(decoded map[string]any, key, want string) error {
	got, ok := decoded[key]
	if !ok {
		return fmt.Errorf("missing field %q", key)
	}
	gotStr, ok := got.(string)
	if !ok {
		return fmt.Errorf("field %q: expected string, got %T", key, got)
	}
	if gotStr != want {
		return fmt.Errorf("field %q: expected %q, got %q", key, want, gotStr)
	}
	return nil
}
