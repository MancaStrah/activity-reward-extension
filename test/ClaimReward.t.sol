// SPDX-License-Identifier: MIT
pragma solidity ^0.8.27;

import {StravaInstructionSender} from "../contracts/InstructionSender.sol";
import {ITeeExtensionRegistry} from "../contracts/interfaces/ITeeExtensionRegistry.sol";
import {ITeeMachineRegistry} from "../contracts/interfaces/ITeeMachineRegistry.sol";

/// Minimal slice of the Foundry cheatcode interface (the repo ships no
/// forge-std). Bound to the canonical HEVM address below.
interface Vm {
    function sign(uint256 privateKey, bytes32 digest) external pure returns (uint8 v, bytes32 r, bytes32 s);
    function warp(uint256 newTimestamp) external;
    function addr(uint256 privateKey) external pure returns (address);
}

/// @notice Behavioral matrix for the claim flow: successful payout, replay,
/// wrong signer, foreign TEE, stale/future
/// proof, wrong month, below-threshold, ineligible result, monthly quotas
/// (address + athlete), owner withdrawal with a pending claim, insufficient
/// pool, reentrancy, verify* tampering, and cancel. Run: `forge test`.
contract ClaimRewardTest {
    Vm constant VM = Vm(0x7109709ECfa91a80626fF3989D68f67F5b1DD12D);

    uint256 constant EXT_ID = 0x10000 + 42;
    uint256 constant OTHER_EXT_ID = 0x10000 + 43;
    uint256 constant TEE_PK = 0xA11CE;
    uint256 constant TEE2_PK = 0xB0B;
    // Aug 2025, mid-month — far from a month boundary so freshness windows
    // never straddle a rollover mid-test.
    uint256 constant NOW = 1755000000;

    ITeeMachineRegistry.TeeStatus constant PRODUCTION = ITeeMachineRegistry.TeeStatus.PRODUCTION;

    bytes32 constant DOMAIN_DISTANCE_PROOF = keccak256("STRAVA_DISTANCE_PROOF_V1");
    bytes32 constant ATHLETE_A = keccak256("athlete-A");
    bytes32 constant ATHLETE_B = keccak256("athlete-B");

    // Rewards and withdrawals land here when the test contract acts as owner/caller.
    receive() external payable {}

    // --- Harness -------------------------------------------------------------

    function _deploy()
        internal
        returns (StravaInstructionSender sender, BehExtensionRegistry ext, BehMachineRegistry mach, address tee)
    {
        VM.warp(NOW);
        ext = new BehExtensionRegistry();
        mach = new BehMachineRegistry();
        sender = new StravaInstructionSender(ITeeExtensionRegistry(address(ext)), ITeeMachineRegistry(address(mach)));
        ext.setSender(EXT_ID, address(sender));
        sender.setExtensionId(EXT_ID);
        tee = VM.addr(TEE_PK);
        mach.set(tee, PRODUCTION, EXT_ID);
    }

    function _fund(StravaInstructionSender sender, uint256 amount) internal {
        (bool ok,) = address(sender).call{value: amount}("");
        require(ok, "funding the pool failed");
    }

    function _proof(
        address caller,
        address tee,
        bytes32 challenge,
        bool eligible,
        uint256 distanceX1000,
        bytes32 athlete,
        StravaInstructionSender sender
    ) internal view returns (StravaInstructionSender.DistanceProof memory p) {
        p.timestamp = block.timestamp;
        p.challenge = challenge;
        p.caller = caller;
        p.teeId = tee;
        p.eligible = eligible;
        p.distanceX1000 = distanceX1000;
        p.monthStart = sender.currentMonthStart();
        p.athleteHash = athlete;
    }

    function _sign(
        uint256 pk,
        StravaInstructionSender sender,
        bytes32 instructionId,
        StravaInstructionSender.DistanceProof memory p
    ) internal view returns (bytes memory) {
        bytes memory payload = abi.encode(
            DOMAIN_DISTANCE_PROOF,
            block.chainid,
            address(sender),
            instructionId,
            p.timestamp,
            p.challenge,
            p.caller,
            p.teeId,
            p.eligible,
            p.distanceX1000,
            p.monthStart,
            p.athleteHash
        );
        bytes32 ethHash = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", keccak256(payload)));
        (uint8 v, bytes32 r, bytes32 s) = VM.sign(pk, ethHash);
        return abi.encodePacked(r, s, v);
    }

    /// Request + build + sign an eligible proof for `user` in one step.
    function _requestAndProve(
        BehUser user,
        address tee,
        StravaInstructionSender sender,
        bool eligible,
        uint256 distanceX1000,
        bytes32 athlete
    ) internal returns (bytes32 iid, StravaInstructionSender.DistanceProof memory p) {
        bytes32 challenge;
        (iid, challenge) = user.request(tee);
        p = _proof(address(user), tee, challenge, eligible, distanceX1000, athlete, sender);
        p.signature = _sign(TEE_PK, sender, iid, p);
    }

    function _expectClaimRevert(
        BehUser user,
        bytes32 iid,
        StravaInstructionSender.DistanceProof memory p,
        string memory want
    ) internal {
        try user.claim(iid, p) {
            revert(string.concat("expected revert: ", want));
        } catch Error(string memory reason) {
            require(_eq(reason, want), reason);
        }
    }

    function _eq(string memory a, string memory b) private pure returns (bool) {
        return keccak256(bytes(a)) == keccak256(bytes(b));
    }

    // --- Happy path ------------------------------------------------------------

    function test_SuccessfulClaimPays() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        user.claim(iid, p);

        require(address(user).balance == 1 ether, "reward not paid");
        require(address(sender).balance == 0, "pool not debited");
        (bytes32 pendingIid,,) = sender.pendingProofs(address(user));
        require(pendingIid == bytes32(0), "pending entry not consumed");
        require(!sender.canClaimAddress(address(user)), "address quota not marked");
        require(!sender.canClaimAthlete(ATHLETE_A), "athlete quota not marked");
    }

    function test_NextMonthAllowsClaimAgain() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 2 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        user.claim(iid, p);

        VM.warp(NOW + 32 days); // next calendar month
        require(sender.canClaimAddress(address(user)), "quota must reset next month");
        (iid, p) = _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        user.claim(iid, p);
        require(address(user).balance == 2 ether, "second-month reward not paid");
    }

    // --- Replay / signer / TEE identity ---------------------------------------

    function test_ReplaySameProofRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 2 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        user.claim(iid, p);
        _expectClaimRevert(user, iid, p, "No pending proof.");
    }

    function test_WrongSignerRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, bytes32 challenge) = user.request(tee);
        StravaInstructionSender.DistanceProof memory p =
            _proof(address(user), tee, challenge, true, 2500, ATHLETE_A, sender);
        p.signature = _sign(TEE2_PK, sender, iid, p); // signed by someone else
        _expectClaimRevert(user, iid, p, "Proof teeId does not match signer.");
    }

    function test_ProofFromDifferentProductionTeeRejected() public {
        (StravaInstructionSender sender,, BehMachineRegistry mach, address tee) = _deploy();
        _fund(sender, 1 ether);
        address tee2 = VM.addr(TEE2_PK);
        mach.set(tee2, PRODUCTION, EXT_ID); // also a production TEE of this extension
        BehUser user = new BehUser(sender);

        (bytes32 iid, bytes32 challenge) = user.request(tee); // routed to tee, not tee2
        StravaInstructionSender.DistanceProof memory p =
            _proof(address(user), tee2, challenge, true, 2500, ATHLETE_A, sender);
        p.signature = _sign(TEE2_PK, sender, iid, p); // consistent (signer == proof.teeId)...
        _expectClaimRevert(user, iid, p, "Signer is not the requested TEE."); // ...but not the requested machine
    }

    function test_NonProductionSignerRejected() public {
        (StravaInstructionSender sender,, BehMachineRegistry mach, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        mach.set(tee, ITeeMachineRegistry.TeeStatus.PAUSED, EXT_ID); // demoted after request
        _expectClaimRevert(user, iid, p, "Signer is not a valid TEE.");
    }

    function test_SignerFromOtherExtensionRejected() public {
        (StravaInstructionSender sender,, BehMachineRegistry mach, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        mach.set(tee, PRODUCTION, OTHER_EXT_ID); // re-homed to another extension
        _expectClaimRevert(user, iid, p, "TEE not in this extension.");
    }

    function test_RequestRejectsForeignOrUnregisteredTee() public {
        (StravaInstructionSender sender,, BehMachineRegistry mach,) = _deploy();
        BehUser user = new BehUser(sender);

        // Never registered: the (post-diamond) registry reverts TeeNotFound()
        // rather than returning a zero status, and that custom error propagates.
        try user.request(address(0xDEAD)) {
            revert("expected revert for unregistered TEE");
        } catch Error(string memory reason) {
            revert(reason); // would mean the mock returned instead of reverting
        } catch (bytes memory data) {
            require(bytes4(data) == BehMachineRegistry.TeeNotFound.selector, "expected TeeNotFound()");
        }

        // Production, but registered under a different extension.
        address foreign = VM.addr(0xF0E1);
        mach.set(foreign, PRODUCTION, OTHER_EXT_ID);
        try user.request(foreign) {
            revert("expected revert: TEE not in this extension.");
        } catch Error(string memory reason) {
            require(_eq(reason, "TEE not in this extension."), reason);
        }
    }

    // --- Freshness / month binding ---------------------------------------------

    function test_StaleProofRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, bytes32 challenge) = user.request(tee);
        StravaInstructionSender.DistanceProof memory p =
            _proof(address(user), tee, challenge, true, 2500, ATHLETE_A, sender);
        p.timestamp = block.timestamp - 300; // usable window is < 300 s
        p.signature = _sign(TEE_PK, sender, iid, p);
        _expectClaimRevert(user, iid, p, "Result too old.");
    }

    function test_FutureProofRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, bytes32 challenge) = user.request(tee);
        StravaInstructionSender.DistanceProof memory p =
            _proof(address(user), tee, challenge, true, 2500, ATHLETE_A, sender);
        p.timestamp = block.timestamp + 61; // beyond CLOCK_DRIFT_TOLERANCE
        p.signature = _sign(TEE_PK, sender, iid, p);
        _expectClaimRevert(user, iid, p, "Timestamp in future.");
    }

    function test_WrongMonthRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, bytes32 challenge) = user.request(tee);
        StravaInstructionSender.DistanceProof memory p =
            _proof(address(user), tee, challenge, true, 2500, ATHLETE_A, sender);
        p.monthStart = sender.currentMonthStart() - 31 days; // previous month's label
        p.signature = _sign(TEE_PK, sender, iid, p);
        _expectClaimRevert(user, iid, p, "Proof not for current month.");
    }

    // --- Eligibility ------------------------------------------------------------

    function test_BelowThresholdRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 1999, ATHLETE_A); // 1.999 km < 2 km
        _expectClaimRevert(user, iid, p, "Distance below threshold.");
    }

    function test_IneligibleProofRefusedAndConsumed() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, false, 5000, ATHLETE_A); // genuine but ineligible
        user.claim(iid, p); // must NOT revert: RewardRefused path

        require(address(user).balance == 0, "refused proof must not pay");
        require(address(sender).balance == 1 ether, "pool must be untouched");
        (bytes32 pendingIid,,) = sender.pendingProofs(address(user));
        require(pendingIid == bytes32(0), "refused proof must still consume the pending entry");
        require(sender.canClaimAddress(address(user)), "refusal must not burn the monthly quota");
        _expectClaimRevert(user, iid, p, "No pending proof."); // and it is single-use
    }

    // --- Monthly quotas ----------------------------------------------------------

    function test_SecondClaimSameMonthRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 2 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        user.claim(iid, p);

        // Fresh request, fresh proof, different athlete — the ADDRESS quota trips.
        (iid, p) = _requestAndProve(user, tee, sender, true, 2500, ATHLETE_B);
        _expectClaimRevert(user, iid, p, "Address already paid this month.");
    }

    function test_SameAthleteSecondWalletRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 2 ether);
        BehUser user1 = new BehUser(sender);
        BehUser user2 = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user1, tee, sender, true, 2500, ATHLETE_A);
        user1.claim(iid, p);

        // Same Strava athlete from a different wallet — the ATHLETE quota trips.
        (iid, p) = _requestAndProve(user2, tee, sender, true, 2500, ATHLETE_A);
        _expectClaimRevert(user2, iid, p, "Strava account already paid this month.");
    }

    // --- Pool solvency / governance (documents accepted design choice) ------

    function test_InsufficientPoolRejected() public {
        (StravaInstructionSender sender,,, address tee) = _deploy(); // pool: 0
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        _expectClaimRevert(user, iid, p, "Insufficient reward pool balance.");
    }

    /// Documents (rather than fixes) a deliberate limitation: the pool is
    /// discretionary. The owner may withdraw everything even while a claim is
    /// pending; the claim then fails on pool balance, not on anything the user
    /// did wrong. Accepted design choice for this prototype.
    function test_OwnerWithdrawalWithPendingClaimDefundsIt() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);

        uint256 before = address(this).balance;
        sender.withdraw(1 ether); // owner = this test contract
        require(address(this).balance == before + 1 ether, "withdrawal must reach the owner");

        _expectClaimRevert(user, iid, p, "Insufficient reward pool balance.");
    }

    function test_WithdrawRejectsNonOwner() public {
        (StravaInstructionSender sender,,,) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);
        try user.withdraw(1 ether) {
            revert("expected revert: Only owner.");
        } catch Error(string memory reason) {
            require(_eq(reason, "Only owner."), reason);
        }
    }

    // --- Reentrancy ----------------------------------------------------------------

    function test_ReentrantClaimGetsExactlyOnePayout() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 2 ether); // enough for two payouts, so only CEI stops the second
        BehReentrantUser attacker = new BehReentrantUser(sender);

        (bytes32 iid, bytes32 challenge) = attacker.request(tee);
        StravaInstructionSender.DistanceProof memory p =
            _proof(address(attacker), tee, challenge, true, 2500, ATHLETE_A, sender);
        p.signature = _sign(TEE_PK, sender, iid, p);

        attacker.claim(iid, p);

        require(attacker.reentered(), "receive() hook did not run - test harness broken");
        require(!attacker.reentrySucceeded(), "reentrant claimReward must fail");
        require(address(attacker).balance == 1 ether, "attacker must be paid exactly once");
        require(address(sender).balance == 1 ether, "pool must be debited exactly once");
    }

    // --- verify* views ----------------------------------------------------------------

    function test_VerifyDistanceProofRejectsTampering() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        require(sender.verifyDistanceProof(iid, p), "genuine proof must verify");
        require(sender.verifyDistanceProofFor(iid, p, address(user), 2000), "genuine proof must verify for its caller");
        require(
            !sender.verifyDistanceProofFor(iid, p, address(this), 2000), "proof must not verify for a different caller"
        );

        uint256 honest = p.distanceX1000;
        p.distanceX1000 = honest * 10; // tamper → digest changes → signer mismatch
        require(!sender.verifyDistanceProof(iid, p), "tampered proof must not verify");
        p.distanceX1000 = honest;

        VM.warp(block.timestamp + 400); // beyond FRESHNESS_SECONDS
        require(!sender.verifyDistanceProof(iid, p), "expired proof must not verify");
    }

    /// The public verifiers must ask the SAME authenticity question claimReward asks.
    /// They used to compare only pendingProofs[caller].instructionId, ignoring the
    /// stored challenge and the requested teeId — so a signature over a challenge
    /// that was never issued, produced by a DIFFERENT production machine of this same
    /// extension, returned true from both while claimReward rejected it. That made
    /// these two the weaker half of one question, and they are the half integrators
    /// are told to gate on.
    ///
    /// Not a path to this pool: the assertion at the end is what proves claimReward
    /// was always strict here. The exposure was any integrator granting something of
    /// its own on a `true`.
    function test_VerifyRejectsForgedChallengeFromAnotherTeeOfThisExtension() public {
        (StravaInstructionSender sender,, BehMachineRegistry mach, address tee) = _deploy();
        BehUser user = new BehUser(sender);

        // A second machine, equally PRODUCTION and equally a member of THIS
        // extension — so every membership check below passes on its own terms.
        address tee2 = VM.addr(TEE2_PK);
        mach.set(tee2, PRODUCTION, EXT_ID);

        // The caller routes to tee1 and the contract issues a challenge to match.
        (bytes32 iid, bytes32 issued) = user.request(tee);

        // tee2 signs a proof over a challenge that was never issued, naming itself,
        // and claiming a distance no ride produces.
        StravaInstructionSender.DistanceProof memory forged =
            _proof(address(user), tee2, keccak256("never-issued-challenge"), true, 999_999_000, ATHLETE_A, sender);
        forged.signature = _sign(TEE2_PK, sender, iid, forged);
        require(forged.challenge != issued, "test setup: the forged challenge must differ from the issued one");

        require(!sender.verifyDistanceProof(iid, forged), "forged challenge + another TEE must not verify");
        require(
            !sender.verifyDistanceProofFor(iid, forged, address(user), 2000),
            "forged challenge + another TEE must not verify for its caller"
        );

        // Each half on its own, so this case cannot pass because of the other.
        // (a) The requested TEE, but a challenge that was never issued.
        StravaInstructionSender.DistanceProof memory wrongChallenge =
            _proof(address(user), tee, keccak256("also-never-issued"), true, 2500, ATHLETE_A, sender);
        wrongChallenge.signature = _sign(TEE_PK, sender, iid, wrongChallenge);
        require(!sender.verifyDistanceProof(iid, wrongChallenge), "a never-issued challenge must not verify");

        // (b) The issued challenge, but a machine this caller never routed to.
        StravaInstructionSender.DistanceProof memory wrongTee =
            _proof(address(user), tee2, issued, true, 2500, ATHLETE_A, sender);
        wrongTee.signature = _sign(TEE2_PK, sender, iid, wrongTee);
        require(!sender.verifyDistanceProof(iid, wrongTee), "a TEE that was not requested must not verify");

        // The control: the genuine proof from the requested TEE over the issued
        // challenge still verifies. Without this the three above would pass just as
        // well against a verifier that refused everything.
        (bytes32 iid2, StravaInstructionSender.DistanceProof memory genuine) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        require(sender.verifyDistanceProof(iid2, genuine), "the genuine proof must still verify");
        require(
            sender.verifyDistanceProofFor(iid2, genuine, address(user), 2000),
            "the genuine proof must still verify for its caller"
        );

        // And claimReward's own strictness, which is why none of this was a fund
        // path: it names the challenge specifically.
        _expectClaimRevert(user, iid2, forged, "Challenge mismatch.");
    }

    /// Found while auditing the fix above, and strictly broader than the finding that
    /// prompted it: for an address with NO pending record every field of the struct
    /// reads as zero, so passing `_instructionId = 0` satisfied an instructionId-only
    /// comparison with 0 == 0. Both verifiers then vouched for a maximum-distance
    /// proof naming an address that had never touched this contract — no request, no
    /// challenge, no routing decision. Verified against the pre-fix contract: it
    /// returned true.
    function test_VerifyRejectsAProofForAnAddressWithNoPendingRequest() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();

        address stranger = address(0xBEEF);
        (bytes32 none,,) = sender.pendingProofs(stranger);
        require(none == bytes32(0), "setup: the stranger must have no pending record");

        // instructionId 0 and challenge 0 — the values that match an empty record.
        StravaInstructionSender.DistanceProof memory p =
            _proof(stranger, tee, bytes32(0), true, 999_999_000, ATHLETE_A, sender);
        p.signature = _sign(TEE_PK, sender, bytes32(0), p);

        require(
            !sender.verifyDistanceProof(bytes32(0), p), "a proof for an address with no pending request must not verify"
        );
        require(
            !sender.verifyDistanceProofFor(bytes32(0), p, stranger, 2000), "and must not verify for that address either"
        );
    }

    /// A proof is public once it is claimed — it sits in claimReward calldata and on
    /// the proxy's result endpoint. Both views must stop vouching for it the moment
    /// it is consumed, or an integrator granting something per verification can be
    /// driven repeatedly with replayed calldata for the rest of the freshness window.
    function test_VerifyStopsVouchingOnceProofIsConsumed() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        require(sender.verifyDistanceProof(iid, p), "genuine unconsumed proof must verify");
        require(sender.verifyDistanceProofFor(iid, p, address(user), 2000), "genuine unconsumed proof must verify for");

        user.claim(iid, p);

        require(!sender.verifyDistanceProof(iid, p), "consumed proof must not verify");
        require(!sender.verifyDistanceProofFor(iid, p, address(user), 2000), "consumed proof must not verify for");
    }

    /// Cancelling clears the pending record, so it must close the views at the same
    /// instant a claim would.
    function test_VerifyStopsVouchingAfterCancel() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        require(sender.verifyDistanceProof(iid, p), "proof must verify before cancel");
        user.cancel();
        require(!sender.verifyDistanceProof(iid, p), "cancelled proof must not verify");
    }

    /// verifyDistanceProofFor must answer the bar it was asked about. `eligible` is
    /// this contract's own 2 km reward verdict, so gating on it would floor every
    /// lower bar to 2 km and wrongly refuse a distance that plainly clears it.
    function test_VerifyForHonoursBarsBelowTheRewardThreshold() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        BehUser user = new BehUser(sender);

        // 1.5 km: genuine and signed, but under the reward bar, so eligible = false.
        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, false, 1500, ATHLETE_A);

        require(sender.verifyDistanceProofFor(iid, p, address(user), 1000), "1.5 km proof must satisfy a 1 km bar");
        require(sender.verifyDistanceProofFor(iid, p, address(user), 1500), "1.5 km proof must satisfy a 1.5 km bar");
        require(!sender.verifyDistanceProofFor(iid, p, address(user), 1501), "1.5 km proof must not satisfy 1.501 km");
        require(
            !sender.verifyDistanceProofFor(iid, p, address(user), 2000), "1.5 km proof must not satisfy the reward bar"
        );
    }

    // --- Cancel -------------------------------------------------------------------------

    function test_CancelPendingProofClearsClaim() public {
        (StravaInstructionSender sender,,, address tee) = _deploy();
        _fund(sender, 1 ether);
        BehUser user = new BehUser(sender);

        (bytes32 iid, StravaInstructionSender.DistanceProof memory p) =
            _requestAndProve(user, tee, sender, true, 2500, ATHLETE_A);
        user.cancel();
        _expectClaimRevert(user, iid, p, "No pending proof.");
        try user.cancel() {
            revert("expected revert: No pending proof.");
        } catch Error(string memory reason) {
            require(_eq(reason, "No pending proof."), reason);
        }
    }
}

/// A wallet that requests and claims as its own address (pendingProofs is keyed
/// by msg.sender, so the test contract cannot impersonate it).
contract BehUser {
    StravaInstructionSender internal immutable TARGET;

    constructor(StravaInstructionSender target) {
        TARGET = target;
    }

    receive() external payable {}

    function request(address teeId) external returns (bytes32 instructionId, bytes32 challenge) {
        TARGET.getDistanceProof(teeId, hex"");
        (instructionId, challenge,) = TARGET.pendingProofs(address(this));
    }

    function claim(bytes32 instructionId, StravaInstructionSender.DistanceProof calldata proof) external {
        TARGET.claimReward(instructionId, proof);
    }

    function cancel() external {
        TARGET.cancelPendingProof();
    }

    function withdraw(uint256 amount) external {
        TARGET.withdraw(amount);
    }
}

/// A claimant whose receive() re-enters claimReward with the same proof. The
/// contract deletes the pending entry BEFORE the transfer (checks-effects-
/// interactions), so the re-entry must fail with "No pending proof.".
contract BehReentrantUser {
    StravaInstructionSender internal immutable TARGET;
    bytes32 internal storedIid;
    StravaInstructionSender.DistanceProof internal storedProof;
    bool public reentered;
    bool public reentrySucceeded;

    constructor(StravaInstructionSender target) {
        TARGET = target;
    }

    receive() external payable {
        if (!reentered) {
            reentered = true;
            StravaInstructionSender.DistanceProof memory p = storedProof;
            (bool ok,) = address(TARGET).call(abi.encodeCall(TARGET.claimReward, (storedIid, p)));
            reentrySucceeded = ok;
        }
    }

    function request(address teeId) external returns (bytes32 instructionId, bytes32 challenge) {
        TARGET.getDistanceProof(teeId, hex"");
        (instructionId, challenge,) = TARGET.pendingProofs(address(this));
    }

    function claim(bytes32 instructionId, StravaInstructionSender.DistanceProof calldata proof) external {
        storedIid = instructionId;
        storedProof = proof;
        TARGET.claimReward(instructionId, proof);
    }
}

/// Extension registry mock: settable id → sender map plus sendInstructions that
/// returns a unique non-zero instruction id (the contract rejects zero ids).
contract BehExtensionRegistry is ITeeExtensionRegistry {
    mapping(uint256 => address) private senders;
    uint256 private next;
    uint256 private instrNonce;

    function setSender(uint256 id, address sender) external {
        senders[id] = sender;
        if (id + 1 > next) next = id + 1;
    }

    function nextPublicExtensionId() external view returns (uint256) {
        return next;
    }

    function getTeeExtensionInstructionsSender(uint256 _extensionId) external view returns (address) {
        return senders[_extensionId];
    }

    function sendInstructions(address[] calldata, TeeInstructionParams calldata) external payable returns (bytes32) {
        instrNonce++;
        return keccak256(abi.encodePacked("instruction", instrNonce));
    }
}

/// Machine registry mock: per-TEE settable status and extension membership.
/// Like the real (post-diamond) registry, lookups for never-registered
/// addresses revert rather than return zero values.
contract BehMachineRegistry {
    struct Entry {
        ITeeMachineRegistry.TeeStatus status;
        uint256 extensionId;
        bool exists;
    }

    error TeeNotFound();

    mapping(address => Entry) private entries;

    function set(address teeId, ITeeMachineRegistry.TeeStatus status, uint256 extensionId) external {
        entries[teeId] = Entry(status, extensionId, true);
    }

    function getTeeMachineStatus(address teeId) external view returns (ITeeMachineRegistry.TeeStatus) {
        if (!entries[teeId].exists) revert TeeNotFound();
        return entries[teeId].status;
    }

    function getExtensionId(address teeId) external view returns (uint256) {
        if (!entries[teeId].exists) revert TeeNotFound();
        return entries[teeId].extensionId;
    }
}
