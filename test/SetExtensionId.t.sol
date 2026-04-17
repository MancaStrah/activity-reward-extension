// SPDX-License-Identifier: MIT
pragma solidity ^0.8.27;

import {StravaInstructionSender} from "../contracts/InstructionSender.sol";
import {ITeeExtensionRegistry} from "../contracts/interfaces/ITeeExtensionRegistry.sol";
import {ITeeMachineRegistry} from "../contracts/interfaces/ITeeMachineRegistry.sol";

/// @notice Regression tests for the hardening of setExtensionId(). The repo
/// ships no forge-std, so reverts are asserted with try/catch and non-owner calls
/// are made through a helper contract (which becomes msg.sender). Run: `forge test`.
contract SetExtensionIdTest {
    uint256 constant FIRST_PUBLIC = 0x10000; // 65536

    // Deploys a fresh sender owned by THIS test contract, plus a mock registry.
    function _deploy() internal returns (StravaInstructionSender sender, MockExtensionRegistry reg) {
        reg = new MockExtensionRegistry();
        MockMachineRegistry mach = new MockMachineRegistry();
        sender = new StravaInstructionSender(ITeeExtensionRegistry(address(reg)), ITeeMachineRegistry(address(mach)));
    }

    function test_OwnerBindsToRegisteredId() public {
        (StravaInstructionSender sender, MockExtensionRegistry reg) = _deploy();
        uint256 id = FIRST_PUBLIC + 617; // e.g. 66153
        reg.setSender(id, address(sender));

        sender.setExtensionId(id);
        require(sender.extensionId() == id, "extension id not bound");
    }

    function test_RejectsIdNotBoundToThisContract() public {
        (StravaInstructionSender sender, MockExtensionRegistry reg) = _deploy();
        uint256 id = FIRST_PUBLIC + 1;
        reg.setSender(id, address(0xBEEF)); // registry maps id to someone else

        try sender.setExtensionId(id) {
            revert("expected revert");
        } catch Error(string memory reason) {
            require(_eq(reason, "Registry does not bind this id to this contract."), reason);
        }
    }

    function test_RejectsBelowFirstPublicId() public {
        (StravaInstructionSender sender, MockExtensionRegistry reg) = _deploy();
        reg.setSender(1, address(sender)); // even if bound, a reserved id is refused
        try sender.setExtensionId(1) {
            revert("expected revert");
        } catch Error(string memory reason) {
            require(_eq(reason, "Not a public extension id."), reason);
        }
    }

    function test_RejectsSecondSet() public {
        (StravaInstructionSender sender, MockExtensionRegistry reg) = _deploy();
        uint256 id = FIRST_PUBLIC + 5;
        reg.setSender(id, address(sender));
        sender.setExtensionId(id);

        try sender.setExtensionId(id) {
            revert("expected revert");
        } catch Error(string memory reason) {
            require(_eq(reason, "Extension ID already set."), reason);
        }
    }

    function test_RejectsNonOwner() public {
        (StravaInstructionSender sender, MockExtensionRegistry reg) = _deploy();
        uint256 id = FIRST_PUBLIC + 9;
        reg.setSender(id, address(sender));

        Caller notOwner = new Caller();
        try notOwner.callSet(sender, id) {
            revert("expected revert");
        } catch Error(string memory reason) {
            require(_eq(reason, "Only owner."), reason);
        }

        // And the owner can still bind afterward — proving the non-owner attempt
        // left the contract unbound rather than partially mutated.
        sender.setExtensionId(id);
        require(sender.extensionId() == id, "owner bind after non-owner attempt failed");
    }

    /// The core attack scenario: an attacker pre-registers THIS contract's address
    /// under their own (lower) extension id before the legitimate registration. The
    /// old permissionless setter scanned and bound to the first (attacker's) match.
    /// The setter lets the owner bind to the id THEY registered, so the
    /// duplicate lower-id registration cannot capture the binding.
    function test_PreRegistrationAttackDefeated() public {
        (StravaInstructionSender sender, MockExtensionRegistry reg) = _deploy();
        uint256 attackerId = FIRST_PUBLIC + 1; // registered first, lower id
        uint256 legitId = FIRST_PUBLIC + 500; // owner's real registration
        reg.setSender(attackerId, address(sender));
        reg.setSender(legitId, address(sender));

        sender.setExtensionId(legitId);
        require(sender.extensionId() == legitId, "must bind to the owner-chosen id, not the attacker's");
    }

    function _eq(string memory a, string memory b) private pure returns (bool) {
        return keccak256(bytes(a)) == keccak256(bytes(b));
    }
}

/// Makes an external call to setExtensionId as its own address, so msg.sender is
/// this helper (a non-owner) rather than the test contract.
contract Caller {
    function callSet(StravaInstructionSender s, uint256 id) external {
        s.setExtensionId(id);
    }
}

/// Minimal ITeeExtensionRegistry: a settable id -> sender map. Only the members
/// setExtensionId() touches are meaningful.
contract MockExtensionRegistry is ITeeExtensionRegistry {
    mapping(uint256 => address) private senders;
    uint256 private next;

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
        return bytes32(0);
    }
}

/// Only needs deployed code — the constructor checks code.length, and the
/// setExtensionId path never calls into the machine registry.
contract MockMachineRegistry {
    function ping() external pure returns (bool) {
        return true;
    }
}
