package types

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"activity-reward-extension/pkg/decoder"
)

// TestMessageDecoderRoundTrip packs a DISTANCE message exactly as the contract
// does and decodes it through the registered decoder — the same path the
// types-server serves. Guards the ABI-name ↔ struct-field mapping (e.g.
// "chainId" needs an explicit abi tag on ChainID).
func TestMessageDecoderRoundTrip(t *testing.T) {
	var challenge [32]byte
	challenge[0] = 0x01
	caller := common.HexToAddress("0x00000000000000000000000000000000000000B2")
	contract := common.HexToAddress("0x00000000000000000000000000000000000000CC")
	encoded, err := DistanceMessageArgs.Pack(challenge, caller, contract, big.NewInt(114), []byte("ciphertext"))
	if err != nil {
		t.Fatalf("pack: %v", err)
	}

	reg := decoder.NewRegistry()
	RegisterDecoders(reg)
	dec, err := reg.Lookup("STRAVA", "DISTANCE", decoder.KindMessage)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	out, err := dec.Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg, ok := out.(DistanceMessage)
	if !ok {
		t.Fatalf("decoded type: got %T, want DistanceMessage", out)
	}
	if msg.Challenge != challenge {
		t.Errorf("challenge: got %x", msg.Challenge)
	}
	if msg.Caller != caller {
		t.Errorf("caller: got %s", msg.Caller)
	}
	if msg.VerifyingContract != contract {
		t.Errorf("verifyingContract: got %s", msg.VerifyingContract)
	}
	if msg.ChainID == nil || msg.ChainID.Int64() != 114 {
		t.Errorf("chainId: got %v, want 114", msg.ChainID)
	}
	if string(msg.EncryptedToken) != "ciphertext" {
		t.Errorf("encryptedToken: got %q", msg.EncryptedToken)
	}
}
