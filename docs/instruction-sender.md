# InstructionSender Contract

## What It Is

The InstructionSender is the **only on-chain address allowed to submit
instructions** to your extension's TEE machines. It acts as the gateway between
end users and the TEE — users call functions on your InstructionSender, which
routes those calls through the `TeeExtensionRegistry` (a facet of the
`FlareTeeManager` diamond).

This is enforced at the protocol level. When you register an extension, you
provide your InstructionSender's address. From that point on, the registry
rejects any `sendInstructions` call where `msg.sender` doesn't match that
address. No EOA, no other contract — only your InstructionSender can submit
instructions for your extension.

## How It Fits Into the System

```
User (EOA)
  │
  │  calls getDistanceProof(teeId, encryptedToken)
  ▼
StravaInstructionSender (this repo's contract)
  │
  │  1. Verifies the requested TEE is PRODUCTION *and* belongs to this extension
  │  2. Calls sendInstructions() on TeeExtensionRegistry (forwarding msg.value)
  ▼
TeeExtensionRegistry (FlareTeeManager diamond)
  │
  │  Checks: msg.sender == registered InstructionSender? ✓
  │  Emits TeeInstructionsSent event
  ▼
TEE machine picks up the instruction off-chain and executes it
```

## Requirements

Any InstructionSender contract must:

1. **Know its extension ID** — needed to constrain which TEE machines may
   serve it. This repo binds it with `setExtensionId(uint256 expectedId)`:
   owner-only, one-shot, the id must be a public id (≥ `0x10000`), and the
   registry must map exactly that id to this contract. The id is supplied and
   cross-checked, never discovered — a setter that scanned the registry for the
   first id mapped to this address would be capturable, because the contract's
   CREATE address is deterministic and an attacker can pre-register it under
   their own extension at a lower id before deployment. `test/SetExtensionId.t.sol`
   covers that scenario. `pre-build.sh` binds the id immediately after
   registration, so deploy → register → bind is one operator sequence with no
   public gap.

2. **Call `sendInstructions` on `TeeExtensionRegistry`** — the only way to
   submit instructions. The current interface takes the TEE list plus a
   `TeeInstructionParams` struct:

   ```solidity
   struct TeeInstructionParams {
       bytes32   opType;             // e.g. bytes32("STRAVA") — must match the extension's handler
       bytes32   opCommand;          // e.g. bytes32("DISTANCE")
       bytes     message;            // the payload the handler receives (JSON or ABI-encoded)
       address[] cosigners;          // multi-sig scenarios; usually empty
       uint64    cosignersThreshold; // usually 0
       address   claimBackAddress;   // refund address for the instruction fee
   }

   function sendInstructions(address[] calldata teeIds, TeeInstructionParams calldata params)
       external payable returns (bytes32 instructionId);
   ```

3. **Forward `msg.value`** — the registry charges a fee per instruction, so
   send functions should be `payable` and forward the full value.

4. **Be deployed before registration** — the extension is registered by
   passing the InstructionSender's address; the address must exist at
   registration time.

The registry doesn't inspect your contract's code or require specific function
signatures — as long as the registered address calls `sendInstructions` with
valid parameters, it works.

## How This Repo's Contract Does It

`contracts/InstructionSender.sol` (`StravaInstructionSender`) has a single
operation. The relevant parts of its send function:

```solidity
bytes32 public constant OP_TYPE_STRAVA      = bytes32("STRAVA");
bytes32 public constant OP_COMMAND_DISTANCE = bytes32("DISTANCE");

function getDistanceProof(address _teeId, bytes calldata _encryptedToken) external payable {
    // Only a PRODUCTION machine of THIS extension may serve the request —
    // checked here, not only at claim time, so a doomed request cannot
    // silently burn the caller's instruction fee.
    require(TEE_MACHINE_REGISTRY.getTeeMachineStatus(_teeId) == ITeeMachineRegistry.TeeStatus.PRODUCTION, ...);
    require(TEE_MACHINE_REGISTRY.getExtensionId(_teeId) == _getExtensionId(), ...);

    // Fresh unpredictable challenge — ties the eventual proof to THIS request.
    bytes32 challenge = keccak256(abi.encodePacked(blockhash(block.number - 1), msg.sender, block.timestamp, block.prevrandao, _nonce));

    ITeeExtensionRegistry.TeeInstructionParams memory params = ITeeExtensionRegistry.TeeInstructionParams({
        opType: OP_TYPE_STRAVA,
        opCommand: OP_COMMAND_DISTANCE,
        // Flat-ABI message; the Go handler unpacks it with DistanceMessageArgs.
        message: abi.encode(challenge, msg.sender, address(this), block.chainid, _encryptedToken),
        cosigners: new address[](0),
        cosignersThreshold: 0,
        claimBackAddress: msg.sender
    });

    bytes32 instructionId = TEE_EXTENSION_REGISTRY.sendInstructions{value: msg.value}(teeIds, params);
    ...
}
```

Design points worth copying:

- **Let the caller pick the TEE and record it.** The pending request stores
  `_teeId`, and `claimReward` later requires the proof's signer to equal it —
  a proof from any *other* production machine is rejected.
- **Bind the message to caller, contract and chain.** The challenge, caller
  address, contract address and chain id all ride in the message and are
  re-checked by the TEE against the decrypted grant, so a payload cannot be
  replayed for someone else, somewhere else.
- Each `OP_TYPE`/`OP_COMMAND` string must match the extension's constants in
  `go/internal/config/config.go` (compared as bytes32 via
  `teeutils.ToHash()`); a mismatch surfaces at runtime as HTTP 501.

After modifying the contract, run `./scripts/generate-bindings.sh` to
regenerate the Go bindings (the tools module imports them).

## Writing Your Own From Scratch

You don't have to start from this contract. Any InstructionSender satisfying
the requirements above works. Reasons to roll your own:

- **Custom access control** — whitelisted callers, token holders, DAO governance
- **On-chain validation** — message format checks, balances, rate limits
- **Multi-TEE routing** — `getRandomTeeIds(extensionId, n)` with n > 1
- **Cosigner workflows** — populate `cosigners` / `cosignersThreshold`
- **Batching** — multiple instructions per transaction

A minimal custom InstructionSender:

```solidity
contract MinimalInstructionSender {
    ITeeExtensionRegistry immutable registry;
    ITeeMachineRegistry immutable machines;
    uint256 immutable extensionId;

    constructor(ITeeExtensionRegistry r, ITeeMachineRegistry m, uint256 extId) {
        registry = r;
        machines = m;
        extensionId = extId;
    }

    function send(bytes32 opType, bytes32 opCommand, bytes calldata message) external payable {
        address[] memory tees = machines.getRandomTeeIds(extensionId, 1);
        registry.sendInstructions{value: msg.value}(
            tees,
            ITeeExtensionRegistry.TeeInstructionParams({
                opType: opType,
                opCommand: opCommand,
                message: message,
                cosigners: new address[](0),
                cosignersThreshold: 0,
                claimBackAddress: msg.sender
            })
        );
    }
}
```

If you take the constructor-parameter route for the extension id shown here,
you inherit its trade-off: the id is fixed at deploy time and the contract must
be deployed *after* registration. If you instead bind post-deployment, do it
so that it cannot be captured — owner-only, explicit expected id,
registry-verified — and never with a registry scan (see above).
