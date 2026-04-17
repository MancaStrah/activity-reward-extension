package types

import "activity-reward-extension/pkg/decoder"

// RegisterDecoders registers all type decoders for this extension. The extension
// has a single operation (STRAVA/DISTANCE), so there is one message decoder and one
// result decoder.
func RegisterDecoders(r *decoder.Registry) {
	// DISTANCE result (JSON)
	r.Register(
		decoder.RegistryKey{OPType: "STRAVA", OPCommand: "DISTANCE", Kind: decoder.KindResult},
		decoder.NewJSONDecoder[DistanceResponse](),
	)
	// DISTANCE message — flat ABI encoding, matching the contract's
	// abi.encode(challenge, msg.sender, address(this), block.chainid, _encryptedToken).
	r.Register(
		decoder.RegistryKey{OPType: "STRAVA", OPCommand: "DISTANCE", Kind: decoder.KindMessage},
		decoder.NewFlatABIDecoder[DistanceMessage](DistanceMessageArgs),
	)
}
