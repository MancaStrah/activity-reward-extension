// SPDX-License-Identifier: MIT
pragma solidity ^0.8.27;

// TODO: Replace local interfaces with imports from flare-smart-contracts-v2 once published as a package.
import {ITeeExtensionRegistry} from "./interfaces/ITeeExtensionRegistry.sol";
import {ITeeMachineRegistry} from "./interfaces/ITeeMachineRegistry.sol";

/// @title StravaInstructionSender
/// @notice Strava fitness reward extension — on-chain entry point for sending instructions to the TEE.
/// Rewards users with 1 native token for covering at least DISTANCE_THRESHOLD_X1000/1000
/// km per month on Strava (kept in step with config.RewardThresholdKm in the TEE).
contract StravaInstructionSender {
    // --- Operation constants (must match go/internal/config/config.go) ---
    // forge-lint: disable-next-line(unsafe-typecast)
    bytes32 public constant OP_TYPE_STRAVA = bytes32("STRAVA");
    /// @notice The extension's single operation: request a signed proof of the
    /// caller's monthly Strava distance. The proof is what claimReward consumes.
    // forge-lint: disable-next-line(unsafe-typecast)
    bytes32 public constant OP_COMMAND_DISTANCE = bytes32("DISTANCE");

    // --- Reward parameters ---
    uint256 public constant REWARD_AMOUNT = 1 ether;
    /// @notice Age limit for a proof. The usable claim window is
    /// FRESHNESS_SECONDS - CLOCK_DRIFT_TOLERANCE (300 s), because the same constant
    /// bounds both directions; raising the forward tolerance without raising this
    /// would silently shorten the time a user has to claim.
    uint256 public constant FRESHNESS_SECONDS = 360;
    uint256 public constant DISTANCE_THRESHOLD_X1000 = 2000;
    /// @notice How far ahead of the including block a proof's timestamp may sit.
    /// The enclave clock and the chain clock drift independently, and a proof more
    /// than this far in the future is rejected outright — so too tight a value makes
    /// every claim fail whenever chain timestamps lag wall-clock.
    uint256 public constant CLOCK_DRIFT_TOLERANCE = 60;

    /// @notice Domain tag mixed into the TEE-signed distance-proof payload. Must
    /// match domainDistanceProof in go/internal/extension. It scopes a signature to
    /// "a Strava monthly-distance proof", so a signature over any other message the
    /// TEE might ever produce cannot be replayed here.
    bytes32 public constant DOMAIN_DISTANCE_PROOF = keccak256("STRAVA_DISTANCE_PROOF_V1");

    // --- Immutables (boilerplate) ---
    /// @notice Reference to the TEE extension registry (FlareTeeManager diamond).
    ITeeExtensionRegistry public immutable TEE_EXTENSION_REGISTRY;
    /// @notice Reference to the TEE machine registry (FlareTeeManager diamond).
    ITeeMachineRegistry public immutable TEE_MACHINE_REGISTRY;

    /// @notice First public extension ID. The registry reserves IDs below this
    /// for system/reserved extensions; public extensions are assigned from here up.
    uint256 private constant FIRST_PUBLIC_EXTENSION_ID = 0x10000; // 65536

    // --- State ---
    uint256 private _extensionId;
    uint256 private _nonce;
    address public owner;

    /// @notice The single outstanding proof request for one caller.
    /// @dev Keyed by caller rather than by instructionId, which bounds this state to
    /// at most ONE record per address however many proofs that address requests: a new
    /// request overwrites the previous one, claiming deletes it, and
    /// cancelPendingProof() clears it outright. Total storage still grows with the
    /// number of distinct addresses that request a proof and never claim or cancel —
    /// each one self-funded — but it cannot grow without bound for any single user.
    struct PendingProof {
        bytes32 instructionId;
        bytes32 challenge;
        // teeId is the specific TEE the caller routed the instruction to. The
        // distance proof is only accepted if signed by THIS machine (see
        // claimReward), so a proof from any other production TEE — including one
        // from a different extension — is rejected.
        address teeId;
    }

    /// @notice A TEE-signed statement of the caller's monthly Strava distance.
    /// Every identity the contract checks — caller, teeId, athleteHash — is covered
    /// by the signature, along with the challenge that ties it to one live request.
    struct DistanceProof {
        uint256 timestamp;
        bytes32 challenge;
        address caller;
        address teeId;
        bool eligible;
        uint256 distanceX1000;
        uint256 monthStart;
        bytes32 athleteHash;
        bytes signature;
    }

    mapping(address => PendingProof) public pendingProofs;
    mapping(address => uint256) public lastPaidMonth;
    mapping(bytes32 => uint256) public lastPaidAthleteHash;

    // --- Events ---
    event RewardRequested(address indexed user, bytes32 indexed instructionId, bytes32 challenge);
    event PendingProofCancelled(address indexed user, bytes32 indexed instructionId);
    event Withdrawal(address indexed to, uint256 amount);
    event RewardClaimed(
        address indexed user,
        bytes32 indexed instructionId,
        uint256 distanceX1000,
        uint256 monthStart,
        bytes32 athleteHash
    );
    event RewardRefused(
        address indexed user,
        bytes32 indexed instructionId,
        uint256 distanceX1000,
        uint256 monthStart,
        bytes32 athleteHash
    );

    /// @notice Initializes the contract with registry addresses.
    /// @param _teeExtensionRegistry Address of the TEE extension registry (FlareTeeManager diamond).
    /// @param _teeMachineRegistry Address of the TEE machine registry (FlareTeeManager diamond).
    constructor(ITeeExtensionRegistry _teeExtensionRegistry, ITeeMachineRegistry _teeMachineRegistry) {
        require(address(_teeExtensionRegistry) != address(0), "TeeExtensionRegistry cannot be zero address");
        require(address(_teeMachineRegistry) != address(0), "TeeMachineRegistry cannot be zero address");
        require(address(_teeExtensionRegistry).code.length > 0, "TeeExtensionRegistry has no code");
        require(address(_teeMachineRegistry).code.length > 0, "TeeMachineRegistry has no code");
        TEE_EXTENSION_REGISTRY = _teeExtensionRegistry;
        TEE_MACHINE_REGISTRY = _teeMachineRegistry;
        owner = msg.sender;
    }

    // --- Receive CFLR for reward pool ---
    receive() external payable {}

    // --- Admin ---

    /// @notice Withdraws funds from the reward pool. Only callable by the owner.
    function withdraw(uint256 _amount) external {
        require(msg.sender == owner, "Only owner.");
        require(_amount <= address(this).balance, "Insufficient balance.");
        (bool success,) = payable(owner).call{value: _amount}("");
        require(success, "Withdraw failed.");
        emit Withdrawal(owner, _amount);
    }

    // --- Send instructions ---

    /// @notice Requests a TEE-signed proof of the caller's monthly Strava distance.
    /// @dev The pending record is keyed by msg.sender, so getDistanceProof and
    /// claimReward MUST be called from the same address. Do not route these through a
    /// shared intermediary (multicall router, shared relayer/paymaster): every user
    /// behind it would share one record and overwrite each other.
    /// The returned proof is what claimReward consumes; a caller who only wants to
    /// read their distance can fetch the result and simply never claim.
    /// @param _teeId Address of the TEE machine to route the instruction to (token must be encrypted with this TEE's public key).
    /// @param _encryptedToken ECIES-encrypted Strava token grant (see the TEE's parseAndVerifyGrant).
    function getDistanceProof(address _teeId, bytes calldata _encryptedToken) external payable {
        require(
            TEE_MACHINE_REGISTRY.getTeeMachineStatus(_teeId) == ITeeMachineRegistry.TeeStatus.PRODUCTION,
            "TEE not in production."
        );
        // Reject a machine from another extension HERE, not only at claim time.
        // PRODUCTION alone spans every TEE on the network, including one an attacker
        // registered under their own extension running their own image; a proof from
        // such a machine can never be claimed, so accepting the request would just
        // take the caller's instruction fee for nothing.
        require(TEE_MACHINE_REGISTRY.getExtensionId(_teeId) == _getExtensionId(), "TEE not in this extension.");

        // A fresh, unpredictable challenge proves the proof was produced in response
        // to THIS request, without relying on the TEE's self-reported timestamp.
        _nonce++;
        bytes32 challenge = keccak256(
            abi.encodePacked(blockhash(block.number - 1), msg.sender, block.timestamp, block.prevrandao, _nonce)
        );

        address[] memory teeIds = new address[](1);
        teeIds[0] = _teeId;
        address[] memory cosigners = new address[](0);

        ITeeExtensionRegistry.TeeInstructionParams memory params = ITeeExtensionRegistry.TeeInstructionParams({
            opType: OP_TYPE_STRAVA,
            opCommand: OP_COMMAND_DISTANCE,
            // The TEE binds the decrypted grant to (caller, this contract, this
            // chain); it also echoes chainId/contract into the signed payload.
            message: abi.encode(challenge, msg.sender, address(this), block.chainid, _encryptedToken),
            cosigners: cosigners,
            cosignersThreshold: 0,
            claimBackAddress: msg.sender
        });

        bytes32 instructionId = TEE_EXTENSION_REGISTRY.sendInstructions{value: msg.value}(teeIds, params);
        // A zero id would be indistinguishable from "no pending request", leaving an
        // entry that claimReward rejects and cancelPendingProof refuses to clear.
        require(instructionId != bytes32(0), "Registry returned a zero instruction id.");
        // Overwrites any previous request from this caller, so repeated distance
        // checks never pile up storage. Recording teeId is what lets claimReward
        // require that the proof came from the very machine this caller chose.
        pendingProofs[msg.sender] = PendingProof({instructionId: instructionId, challenge: challenge, teeId: _teeId});
        emit RewardRequested(msg.sender, instructionId, challenge);
    }

    /// @notice Clears the caller's outstanding proof request, refunding its storage.
    /// Requesting a new proof or claiming one already clears it; this is for a caller
    /// who requested a proof and then decided not to claim it.
    function cancelPendingProof() external {
        bytes32 instructionId = pendingProofs[msg.sender].instructionId;
        require(instructionId != bytes32(0), "No pending proof.");
        delete pendingProofs[msg.sender];
        emit PendingProofCancelled(msg.sender, instructionId);
    }

    // --- Claim reward ---

    /// @notice Claims a reward using a TEE-signed distance proof.
    /// If the proof indicates the user is not eligible, emits RewardRefused instead of transferring funds.
    /// @dev Single-use: the pending entry is deleted on both the paid and refused
    /// paths, and a successful payout additionally marks the caller and the athlete
    /// as paid for the month, so the same proof can never yield a second payout.
    function claimReward(bytes32 _instructionId, DistanceProof calldata _proof) external {
        // Keyed by msg.sender, so only the caller who made the request can claim it —
        // there is no way to reach another user's pending entry.
        PendingProof storage pending = pendingProofs[msg.sender];

        // --- Common validation (applies to both eligible and ineligible proofs) ---
        require(pending.instructionId != bytes32(0), "No pending proof.");
        require(pending.instructionId == _instructionId, "Instruction mismatch.");
        require(_proof.caller == msg.sender, "Caller mismatch.");
        require(_proof.challenge == pending.challenge, "Challenge mismatch.");
        require(_proof.monthStart == _currentMonthStart(), "Proof not for current month.");
        require(_proof.timestamp <= block.timestamp + CLOCK_DRIFT_TOLERANCE, "Timestamp in future.");
        require(block.timestamp + CLOCK_DRIFT_TOLERANCE - _proof.timestamp < FRESHNESS_SECONDS, "Result too old.");

        // --- TEE signature verification (applies to both eligible and ineligible proofs) ---
        address signer = _recoverProofSigner(_instructionId, _proof);
        require(signer != address(0), "Invalid signature.");

        // The recovered signer IS the signing TEE's address. Requiring it to equal
        // BOTH the teeId carried in the signed proof and the teeId recorded when the
        // request was sent means the proof can only come from the very machine this
        // caller routed to — not merely from some other production TEE.
        require(signer == _proof.teeId, "Proof teeId does not match signer.");
        require(signer == pending.teeId, "Signer is not the requested TEE.");

        // NOTE: getTeeMachineStatus / getExtensionId revert with TeeNotFound() when
        // the signer was never registered, so a forged proof is rejected with that
        // custom error rather than the require strings below. Either way it is
        // rejected.
        require(
            TEE_MACHINE_REGISTRY.getTeeMachineStatus(signer) == ITeeMachineRegistry.TeeStatus.PRODUCTION,
            "Signer is not a valid TEE."
        );
        // PRIMARY anti-forgery check, not defence in depth: `signer == pending.teeId`
        // only proves the proof came from the machine this caller chose, and a caller
        // may choose ANY production machine on the network — including one an attacker
        // registered under their own extension with their own image. Extension
        // membership is what actually constrains the signer to a machine whose code
        // this extension's governance allow-listed.
        require(TEE_MACHINE_REGISTRY.getExtensionId(signer) == _getExtensionId(), "TEE not in this extension.");

        // --- Handle ineligible proof: clean up and emit RewardRefused ---
        if (!_proof.eligible) {
            delete pendingProofs[msg.sender];
            emit RewardRefused(
                _proof.caller, _instructionId, _proof.distanceX1000, _proof.monthStart, _proof.athleteHash
            );
            return;
        }

        // --- Handle eligible proof: additional checks + transfer ---
        require(canClaimAddress(_proof.caller), "Address already paid this month.");
        require(canClaimAthlete(_proof.athleteHash), "Strava account already paid this month.");
        require(_proof.distanceX1000 >= DISTANCE_THRESHOLD_X1000, "Distance below threshold.");
        require(address(this).balance >= REWARD_AMOUNT, "Insufficient reward pool balance.");

        // Effects (before transfer — checks-effects-interactions)
        uint256 monthStart = _currentMonthStart();
        delete pendingProofs[msg.sender];
        lastPaidMonth[_proof.caller] = monthStart;
        lastPaidAthleteHash[_proof.athleteHash] = monthStart;

        (bool success,) = payable(_proof.caller).call{value: REWARD_AMOUNT}("");
        require(success, "Transfer failed.");

        emit RewardClaimed(_proof.caller, _instructionId, _proof.distanceX1000, monthStart, _proof.athleteHash);
    }

    // --- Proof verification ---

    /// @notice Checks that a distance proof is authentic AND still fresh: signed by
    /// the very PRODUCTION TEE this caller routed to, over this
    /// chain/contract/instruction and the challenge that was actually issued, issued
    /// within FRESHNESS_SECONDS, and covering the current month.
    /// @dev Freshness is included so a `true` result cannot be produced by an old or
    /// previous-month proof — that would let a consumer display long-stale distances
    /// as "verified".
    ///
    /// Authenticity is now the SAME question claimReward asks: all three fields of
    /// the pending record — instructionId, challenge and teeId — must match. It used
    /// to compare only the instruction id, which made this the weaker of two
    /// verifiers over one proof and let a signature over a never-issued challenge,
    /// from a different machine of this extension, return true here while claimReward
    /// rejected it.
    ///
    /// What this still does NOT check is payout ELIGIBILITY: the caller's and
    /// athlete's monthly quota, the distance threshold and the pool balance remain
    /// claimReward's business. So `true` means "genuinely from the TEE this caller
    /// asked, answering this request, and current" — not "will pay out".
    /// @return True if the proof is authentic, fresh, for the current month, and not
    /// yet consumed by claimReward or cleared by cancelPendingProof.
    function verifyDistanceProof(bytes32 _instructionId, DistanceProof calldata _proof) external view returns (bool) {
        return _isAuthenticFreshProof(_instructionId, _proof);
    }

    /// @dev Shared by verifyDistanceProof and verifyDistanceProofFor. Internal so the
    /// stricter variant does not have to make an external self-call to reuse it.
    function _isAuthenticFreshProof(bytes32 _instructionId, DistanceProof calldata _proof)
        internal
        view
        returns (bool)
    {
        address signer = _recoverProofSigner(_instructionId, _proof);
        if (signer == address(0) || signer != _proof.teeId) {
            return false;
        }
        // The pending record is the on-chain memory of what was actually requested,
        // and ALL THREE of its fields have to match — the same three claimReward
        // requires. Read once; a storage pointer costs one lookup instead of three.
        PendingProof storage pending = pendingProofs[_proof.caller];

        // There must BE a request, stated separately exactly as claimReward states it
        // (`require(pending.instructionId != bytes32(0), "No pending proof.")`).
        //
        // The bug this names was real: for an address with no record every field
        // reads as zero, so a caller passing _instructionId = 0 satisfied an
        // instructionId-only comparison with 0 == 0, and both verifiers vouched for a
        // maximum-distance proof naming an address that had never touched this
        // contract. There is a regression test for it.
        //
        // This line is REDUNDANT and deliberately kept. Mutation-checked: deleting it
        // alone breaks no test, because the `signer != pending.teeId` comparison below
        // already rejects that input — pending.teeId is the zero address for an absent
        // record and a recovered signer never is. So no test can bind this line in
        // isolation, and none pretends to. It is here because the invariant it states
        // is the reason the check below is load-bearing, and a future edit that
        // relaxes that comparison would otherwise silently reopen a hole nobody
        // connected to it. If you remove this line, the mutation to re-run is
        // "delete the teeId comparison and check the no-pending-request test fails".
        if (pending.instructionId == bytes32(0)) {
            return false;
        }

        // Single-use, checked against on-chain state rather than the proof itself.
        // Proofs are public — they sit in claimReward calldata and on the proxy's
        // unauthenticated result endpoint — so without this a proof stays verifiable
        // for the rest of its freshness window AFTER claimReward consumed it, and
        // anyone replaying the calldata could keep satisfying an integrator that
        // grants something per verification. claimReward deletes the record, and
        // cancelPendingProof clears it, so both close this at the same instant.
        if (pending.instructionId != _instructionId) {
            return false;
        }
        // The challenge is what proves the proof answers THIS request rather than
        // merely naming its instruction id. Checking the id alone left both public
        // verifiers accepting a signature over a challenge that was never issued:
        // a TEE that signs arbitrary tuples could mint an authentic-looking,
        // maximum-distance proof for any address with a request outstanding, and
        // both would return true. claimReward always rejected it ("Challenge
        // mismatch."), so this was never a path to the pool — but these two
        // functions exist for integrators who grant something of their own on a
        // `true`, and they were the weaker half of the same question.
        if (_proof.challenge != pending.challenge) {
            return false;
        }
        // And it has to come from the very machine this caller routed to, not
        // merely from some other production TEE of this extension. Membership is
        // checked below; that is a different claim from "the one that was asked".
        // Same reasoning as claimReward's `signer == pending.teeId`, and since
        // signer == _proof.teeId is already established above, comparing either
        // one to pending.teeId settles all three.
        if (signer != pending.teeId) {
            return false;
        }
        // Ordered so the subtraction below cannot underflow.
        if (_proof.timestamp > block.timestamp + CLOCK_DRIFT_TOLERANCE) {
            return false;
        }
        if (block.timestamp + CLOCK_DRIFT_TOLERANCE - _proof.timestamp >= FRESHNESS_SECONDS) {
            return false;
        }
        if (_proof.monthStart != _currentMonthStart()) {
            return false;
        }
        // Read the extension id BEFORE the try blocks. _getExtensionId() reverts when
        // it is unset, and Solidity's catch only wraps the external call — a revert
        // inside a success body propagates, which would make this predicate throw
        // instead of answering false for every proof until setExtensionId() has run.
        uint256 myExtensionId = _extensionId;
        if (myExtensionId == 0) {
            return false;
        }
        // try/catch: the registry reverts TeeNotFound() for an unregistered signer,
        // and a predicate must answer false rather than revert.
        try TEE_MACHINE_REGISTRY.getTeeMachineStatus(signer) returns (ITeeMachineRegistry.TeeStatus status) {
            if (status != ITeeMachineRegistry.TeeStatus.PRODUCTION) return false;
        } catch {
            return false;
        }
        try TEE_MACHINE_REGISTRY.getExtensionId(signer) returns (uint256 signerExtensionId) {
            if (signerExtensionId != myExtensionId) return false;
        } catch {
            return false;
        }
        return true;
    }

    /// @notice The check most integrators actually want: the proof is authentic,
    /// fresh and unconsumed (as `verifyDistanceProof`), belongs to `_expectedCaller`,
    /// and attests at least `_minDistanceX1000` metres-per-thousand. Any bar may be
    /// asked for, not only this contract's reward threshold.
    /// @dev `verifyDistanceProof` alone is NOT a fitness check and NOT caller-bound:
    /// it ignores `eligible` and `distanceX1000`, and proofs are public (they appear
    /// in calldata and on the proxy's unauthenticated result endpoint), so anyone
    /// could satisfy it using a stranger's proof. Gate on this function instead.
    /// @param _expectedCaller The address the proof must have been issued to.
    /// @param _minDistanceX1000 Minimum attested distance (km × 1000).
    /// @return True only if the proof is authentic, fresh, issued to _expectedCaller, and meets the distance bar.
    function verifyDistanceProofFor(
        bytes32 _instructionId,
        DistanceProof calldata _proof,
        address _expectedCaller,
        uint256 _minDistanceX1000
    ) external view returns (bool) {
        if (_proof.caller != _expectedCaller) return false;
        // Deliberately not gated on `_proof.eligible`. That flag is the TEE's verdict
        // against this contract's own 2 km reward bar, so testing it here would floor
        // every `_minDistanceX1000` below DISTANCE_THRESHOLD_X1000 to that bar and
        // silently answer false for a bar the attested distance plainly meets — an
        // integrator asking "at least 1 km?" would get no for a 1.5 km proof. The
        // attested distance is the authority; pass DISTANCE_THRESHOLD_X1000 to ask
        // the reward question.
        if (_proof.distanceX1000 < _minDistanceX1000) return false;
        return _isAuthenticFreshProof(_instructionId, _proof);
    }

    /// @notice Rebuilds the signed payload and recovers the signing TEE's address.
    /// @dev The payload is domain-separated and bound to this chain, this contract,
    /// and this specific instruction, so a signature cannot be replayed across
    /// operations, chains, contracts, or instructions. It must agree byte-for-byte
    /// with abiEncodeDistanceProofPayload in go/internal/extension — the paired
    /// vector tests (SignPayloadVector.t.sol and the Go equivalent) enforce that.
    /// Returns address(0) when the signature is malformed or unrecoverable.
    function _recoverProofSigner(bytes32 _instructionId, DistanceProof calldata _proof)
        internal
        view
        returns (address)
    {
        bytes memory payload = abi.encode(
            DOMAIN_DISTANCE_PROOF,
            block.chainid,
            address(this),
            _instructionId,
            _proof.timestamp,
            _proof.challenge,
            _proof.caller,
            _proof.teeId,
            _proof.eligible,
            _proof.distanceX1000,
            _proof.monthStart,
            _proof.athleteHash
        );
        bytes32 msgHash = keccak256(payload);
        bytes32 ethHash = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", msgHash));

        if (_proof.signature.length != 65) {
            return address(0);
        }
        bytes memory sig = _proof.signature;
        bytes32 r;
        bytes32 s;
        uint8 v;
        assembly {
            r := mload(add(sig, 32))
            s := mload(add(sig, 64))
            v := byte(0, mload(add(sig, 96)))
        }
        if (v < 27) v += 27;
        if (v != 27 && v != 28) {
            return address(0);
        }
        // Reject non-canonical (high-s) signatures. Not exploitable today, because
        // replay is prevented by deleting the pending record plus the lastPaid*
        // guards rather than by anything keyed on signature bytes — but four distinct
        // encodings recovering the same signer is a footgun the moment any future code
        // keys off a signature, so refuse the malleable half outright.
        if (uint256(s) > 0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0) {
            return address(0);
        }
        return ecrecover(ethHash, v, r, s);
    }

    // --- Helpers ---

    /// @notice The extension id this contract is registered under.
    /// @dev Public so a client can learn the expected extension id from the contract
    /// it is about to send an instruction to, instead of trusting the proxy's
    /// self-reported /info value. Reverts until setExtensionId() has run.
    function extensionId() external view returns (uint256) {
        return _getExtensionId();
    }

    /// @notice Returns the start of the current month as a Unix timestamp (for cross-language validation).
    function currentMonthStart() external view returns (uint256) {
        return _currentMonthStart();
    }

    function _currentMonthStart() internal view returns (uint256) {
        // Approximate: find the 1st of the current month at 00:00 UTC.
        // We use a loop-free approach based on date arithmetic.
        uint256 timestamp = block.timestamp;
        uint256 daysSinceEpoch = timestamp / 86400;

        // Convert days since epoch to year/month/day using the civil calendar algorithm.
        // Based on Howard Hinnant's algorithm (http://howardhinnant.github.io/date_algorithms.html)
        uint256 z = daysSinceEpoch + 719468;
        uint256 era = z / 146097;
        uint256 doe = z - era * 146097;
        uint256 yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
        uint256 doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
        uint256 mp = (5 * doy + 2) / 153;
        uint256 m = mp < 10 ? mp + 3 : mp - 9;
        uint256 y = yoe + era * 400 + (m <= 2 ? 1 : 0);

        // Compute days from epoch to the 1st of month m in year y.
        // First, convert back: day 1 of month m, year y.
        uint256 y2 = m <= 2 ? y - 1 : y;
        uint256 era2 = y2 / 400;
        uint256 yoe2 = y2 - era2 * 400;
        uint256 m2 = m > 2 ? m - 3 : m + 9;
        uint256 doy2 = (153 * m2 + 2) / 5; // day 0 of month
        uint256 doe2 = yoe2 * 365 + yoe2 / 4 - yoe2 / 100 + doy2;
        uint256 firstDayOfMonth = era2 * 146097 + doe2 - 719468;

        return firstDayOfMonth * 86400;
    }

    /// @notice Binds this contract to its on-chain extension id. Owner-only and
    /// single-use.
    /// @dev The id is supplied by the caller and cross-checked, never discovered. A
    /// setter that instead scanned the registry for the first id mapped to this
    /// address would be capturable: this contract's CREATE address is deterministic,
    /// so an attacker can pre-register it under their own extension at a lower id
    /// before deployment, and a scan would bind permanently to that registration —
    /// letting a TEE the attacker governs issue proofs this contract accepts.
    ///
    /// Three requirements close that off: the owner passes the id THEY registered, the
    /// registry must map exactly that id to this contract, and the binding is
    /// single-use. A pre-registration under any other id is then irrelevant, because
    /// claimReward and getDistanceProof both gate on
    /// getExtensionId(signer) == this id and reject machines from every other extension.
    /// @param expectedId The public extension id registered for this contract.
    function setExtensionId(uint256 expectedId) external {
        require(msg.sender == owner, "Only owner.");
        require(_extensionId == 0, "Extension ID already set.");
        require(expectedId >= FIRST_PUBLIC_EXTENSION_ID, "Not a public extension id.");
        require(
            TEE_EXTENSION_REGISTRY.getTeeExtensionInstructionsSender(expectedId) == address(this),
            "Registry does not bind this id to this contract."
        );
        _extensionId = expectedId;
    }

    /// @notice Checks whether msg.sender has already been paid this month.
    function canClaimAddress() public view returns (bool) {
        return lastPaidMonth[msg.sender] < _currentMonthStart();
    }

    /// @notice Checks whether a given address has already been paid this month.
    function canClaimAddress(address _addr) public view returns (bool) {
        return lastPaidMonth[_addr] < _currentMonthStart();
    }

    /// @notice Checks whether a Strava account has already been paid this month.
    /// @param _athleteHash The hashed athlete ID (from the distance proof).
    function canClaimAthlete(bytes32 _athleteHash) public view returns (bool) {
        return lastPaidAthleteHash[_athleteHash] < _currentMonthStart();
    }

    /// @notice Checks whether a reward can still be claimed this month for msg.sender and the given athlete.
    /// @param _athleteHash The hashed athlete ID (from the distance proof).
    function canClaim(bytes32 _athleteHash) external view returns (bool) {
        return canClaimAddress() && canClaimAthlete(_athleteHash);
    }

    /// @notice Checks whether a reward can still be claimed this month for a given address and athlete.
    /// @param _addr The wallet address.
    /// @param _athleteHash The hashed athlete ID (from the distance proof).
    function canClaim(address _addr, bytes32 _athleteHash) external view returns (bool) {
        return canClaimAddress(_addr) && canClaimAthlete(_athleteHash);
    }

    /// @notice Returns a random active TEE machine for this extension, along with its proxy URL.
    /// @dev The proxy URL exposes a /info endpoint from which the caller can retrieve the TEE's
    ///      public key, needed to ECIES-encrypt the token grant before calling getDistanceProof.
    /// @return teeId The TEE machine's Ethereum address.
    /// @return proxyUrl The TEE proxy URL (call GET <proxyUrl>/info to get the public key).
    function getTee() external view returns (address teeId, string memory proxyUrl) {
        address[] memory teeIds = TEE_MACHINE_REGISTRY.getRandomTeeIds(_getExtensionId(), 1);
        require(teeIds.length > 0, "No active TEE machine for this extension.");
        teeId = teeIds[0];
        ITeeMachineRegistry.TeeMachine memory machine = TEE_MACHINE_REGISTRY.getTeeMachine(teeId);
        proxyUrl = machine.url;
    }

    /// @notice Returns the cached extension ID, reverting if not yet set.
    /// @return The extension ID assigned to this contract.
    function _getExtensionId() internal view returns (uint256) {
        require(_extensionId != 0, "Extension ID is not set.");
        return _extensionId;
    }
}
