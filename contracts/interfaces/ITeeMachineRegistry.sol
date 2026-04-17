// SPDX-License-Identifier: MIT
pragma solidity >=0.7.6 <0.9;

// TODO: Replace this minimal interface with the full import once flare-smart-contracts-v2
// is published as a package:
//   import { IMachineManager } from "flare-smart-contracts-v2/contracts/userInterfaces/tee/IMachineManager.sol";
//
// Calls are served by the MachineManager facet of the FlareTeeManager diamond.
interface ITeeMachineRegistry {
    /// Mirrors IMachineManager.TeeStatus of the deployed FlareTeeManager.
    /// Ordinals matter: getTeeMachineStatus returns the raw enum value, so this
    /// declaration must list the states in the registry's own order — PRODUCTION is 2.
    enum TeeStatus {
        NONE,
        INITIALIZED,
        PRODUCTION,
        SUSPENDED,
        PAUSED,
        BANNED
    }

    struct TeeMachine {
        address teeId;
        address teeProxyId;
        string url;
    }

    function getRandomTeeIds(uint256 _extensionId, uint256 _count) external view returns (address[] memory);

    /**
     * Get the extension a TEE machine belongs to.
     * @param _teeId The TEE machine id.
     * @return The extension id the machine is registered under. Reverts with
     *         TeeNotFound() for addresses that were never registered.
     */
    function getExtensionId(address _teeId) external view returns (uint256);

    /**
     * Get the status of a TEE machine.
     * @param _teeId The TEE machine id.
     * @return The status of the TEE machine. Reverts with TeeNotFound() for
     *         addresses that were never registered.
     */
    function getTeeMachineStatus(address _teeId) external view returns (TeeStatus);

    /**
     * Get TEE machine data (teeId, teeProxyId, url).
     * @param _teeId The TEE machine id.
     * @return The TEE machine data.
     */
    function getTeeMachine(address _teeId) external view returns (TeeMachine memory);
}
