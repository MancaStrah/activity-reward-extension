// SPDX-License-Identifier: MIT
pragma solidity ^0.8.27;

/// @title SignPayloadVectorTest
/// @notice Cross-language vector: proves the Solidity reconstruction of the
/// TEE-signed distance-proof payload (as in _recoverProofSigner) matches the Go
/// extension (abiEncodeDistanceProofPayload) for a fixed input vector. The same
/// expected hash is asserted by TestSignPayloadCrossLanguageVector in
/// go/internal/extension. If the two ever diverge, on-chain claims would fail —
/// this test catches that at build time instead.
contract SignPayloadVectorTest {
    bytes32 constant DOMAIN_DISTANCE_PROOF = keccak256("STRAVA_DISTANCE_PROOF_V1");

    function test_DistanceProofPayloadHashMatchesGo() public pure {
        bytes32 instructionId = bytes32(uint256(0xdeadbeef));
        uint256 chainId = 114;
        address verifyingContract = address(uint160(0xCC));
        uint256 timestamp = 1700000000;
        bytes32 challenge = bytes32(uint256(1));
        address caller = address(uint160(0xC3));
        address teeId = address(uint160(0xEE));
        bool eligible = true;
        uint256 distanceX1000 = 5100;
        uint256 monthStart = 1698796800;
        bytes32 athleteHash = bytes32(uint256(0xee) << 248);

        bytes memory payload = abi.encode(
            DOMAIN_DISTANCE_PROOF,
            chainId,
            verifyingContract,
            instructionId,
            timestamp,
            challenge,
            caller,
            teeId,
            eligible,
            distanceX1000,
            monthStart,
            athleteHash
        );

        bytes32 got = keccak256(payload);
        bytes32 want = 0x92d724a4a2dac9e7c86026e881f6363515b6ad83e74d1590e2e732d8bbeeef13;
        require(got == want, "distance-proof payload hash mismatch between Solidity and Go");
    }
}
