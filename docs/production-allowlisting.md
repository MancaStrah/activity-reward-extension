# Production allowlisting runbook

How to authorize a TEE image's `codeHash` on-chain for a **real Confidential
Space deployment** without circular trust.

## The problem this procedure exists for

`allow-tee-version` reads `extensionId`, `codeHash` and `platform` from the
extension proxy's `/info` endpoint and writes them into the on-chain allowlist
with the owner key. If the proxy's word is taken at face value, the component
being vetted supplies its own identity — an attacker controlling the proxy
path (or a mistakenly deployed but validly attested image) gets its own
`codeHash` authorized.

On the **simulated profiles** (`TEE_PROFILE=local`, `testnet-sim`) this is
accepted: attestation is simulated and the "codeHash" is a development
constant, not a trust anchor. The tool therefore runs unmodified there and
prints a warning.

On **`TEE_PROFILE=confidential-space`** the tool fails closed: it refuses to
run unless the operator supplies both expected values — `codeHash` and
`platform` — from their **own build**, and then it only *confirms* the proxy
agrees — a proxy-supplied value is never promoted on its own.

## Procedure

1. **Build the image reproducibly** and record its digest. The Confidential
   Space launcher measures the container image; the measured code hash is the
   image digest of the exact image you hand to the VM operator:

   ```bash
   export SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
   docker compose -f docker-compose.yaml build extension-tee
   docker buildx imagetools inspect <registry>/<image>:<tag>   # after push
   # → sha256:<measured-image-code-hash>
   ```

   Keep the digest with the release notes (see REPRODUCIBILITY.md — an
   independent party must be able to rebuild the same digest from the tagged
   source).

2. **Deploy the image** on the Confidential Space VM
   (docs/deployment-steps.md steps 5–7) and confirm `/info` over **HTTPS**
   reports that same hash:

   ```bash
   curl -s "$EXT_PROXY_URL/info" | jq '.machineData.codeHash'
   ```

   If the values differ, stop. Do not "fix" this by allowlisting what `/info`
   reports — that is exactly the circular trust this runbook removes.

3. **Pin the whole attestation posture** in the proxy config
   (`config/proxy/extension_proxy.<chain>.docker.toml`):

   ```toml
   [attestation]
   enable = true
   allow_magic_pass = false
   audience = "<attestation-token-audience>"
   expected_code_hashes    = ["sha256:<measured-image-code-hash>"]
   expected_platforms      = ["AMD_SEV_SNP_VM"]          # or ["INTEL_TDX_VM"]
   expected_debug_statuses = ["disabled-since-boot"]
   max_token_age = "5m"
   require_sec_boot = true
   ```

   All of them are required, because upstream tee-proxy reads an empty list, an
   empty string, `max_token_age = 0` or `require_sec_boot = false` as "skip that
   check" rather than as a misconfiguration — every value left unset silently
   drops one control while the section still looks configured.
   `start-services.sh` refuses the `confidential-space` profile until each is
   meaningfully set (`validate_proxy_attestation_config` in
   `scripts/lib/profile.sh`, which reads the file with the proxy's own parser via
   `tools/cmd/check-proxy-config`). Two keys are checked for meaning rather than
   presence, because a valid value can still remove the control: the debug status
   must be `disabled-since-boot` — `["enabled"]` would leave the check running and
   passing for a TEE the host can inspect — and `max_token_age` must be positive and
   no longer than an hour, since the token's own `exp` is always enforced and this
   setting can only narrow that window.
   `config/proxy/extension_proxy.<chain>.docker.toml.example` is the source of
   truth for the keys and documents what each empty value would disable.

4. **Allowlist with the explicit expectations** (the tool cross-checks and
   refuses on any mismatch):

   ```bash
   EXPECTED_CODE_HASH=0x<measured-image-code-hash> \
   EXPECTED_PLATFORM=0x<expected-platform> ./scripts/post-build.sh
   # or directly:
   cd tools && TEE_PROFILE=confidential-space go run ./cmd/allow-tee-version \
       -a ../config/coston2/deployed-addresses.json -c "$CHAIN_URL" -p "$EXT_PROXY_URL" \
       -version v0.1.0 -expected-codehash 0x<measured-image-code-hash> \
       -expected-platform 0x<expected-platform>
   ```

   Requirements enforced by the tool on this profile:
   - `-expected-codehash` **and** `-expected-platform` present (fail-closed
     without either — the platform is the other half of the
     `(codeHash, platform)` pair being allow-listed, and it comes from the same
     `/info` response),
   - `EXT_PROXY_URL` is HTTPS or loopback (`RequireSecureProxyURL`),
   - `/info` codeHash **and** platform exactly match the supplied values.

   The platform expectation is the on-chain platform value of the machine type
   you provisioned — `GCP_AMD_SEV` hex-encoded
   (`0x4743505f414d445f534556…`, `PlatformAMD` in
   `tools/pkg/fccutils/encoding.go`) for an AMD SEV VM. It is a different
   encoding from the `hwmodel` list in step 3, so do not copy one into the
   other.

5. **Record the trail** so the deployment can be reproduced later: source tag
   → image digest → allowlist transaction hash. Put the tx hash and digest in
   the release notes.

## Known residual gaps (documented, not closed)

- The tool does **not** verify the Confidential Space attestation JWT itself
  (issuer, audience, nonce, debug claims). Those claims are checked by the
  proxy, whose full `[attestation]` posture this profile requires (step 3),
  and the operator's own digest anchors the codeHash independently — together
  they cover the bootstrap path, but an end-to-end JWT verification in the tool
  remains future work: it can only be meaningfully tested against a live
  Confidential Space deployment.
- A single allowlisted TEE remains a single cryptographic point of failure —
  an accepted design limitation of this prototype.
