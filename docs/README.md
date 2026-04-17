# Docs index

Standard layout shared by every FCC extension example. Validate with
`./scripts/check-docs.sh`.

The [repo README](../README.md) is the front door: what the extension pays for,
how to get a Strava token, the full env var and port tables, and the contract
reference. Everything here is the task-shaped detail behind it.

## Required — every extension has these

| Doc                                                      | Answers                                       |
| -------------------------------------------------------- | --------------------------------------------- |
| [getting-started.md](getting-started.md)                 | run it locally, end to end                    |
| [deployment-steps.md](deployment-steps.md)               | deploy to Coston/Coston2 and operate it       |
| [testing.md](testing.md)                                 | test suites and what they cover               |
| [testing-against-coston2.md](testing-against-coston2.md) | test against a deployed extension             |
| [architecture.md](architecture.md)                       | how this extension works                      |
| [ngrok.md](ngrok.md)                                     | expose a local proxy for testnet registration |
| [cloudflared.md](cloudflared.md)                         | the manual tunnel alternative to ngrok        |

`deployment-steps.md` must cover the platform-wide traps, because they are not
obvious and every extension hits them:

- the TEE key is in memory only — every relaunch mints a new identity, and the old
  machine stays **active** and keeps receiving instructions
- one-shot bindings (here `setExtensionId`) must be written **last**
- the Confidential Space launch policy aborts on the first env var outside
  `tee.launch_policy.allow_env_override`
- deploy by **digest**, not tag — the code hash is registered on-chain
- `SIMULATED_TEE=false` on real hardware

## Shared — kept byte-identical across FCC extensions

These two describe the platform (the tee-node/tee-proxy container contract and the
instruction-sender pattern) rather than this extension, so they are maintained as one
shared set across FCC extension repos and copied in verbatim. Correct a platform fact
in the shared set and re-copy, rather than editing this copy into a fork of it; the
extension-specific detail belongs in the docs below instead:

[extension-guide.md](extension-guide.md) ·
[instruction-sender.md](instruction-sender.md)

Two docs in the shared set do not apply and are deliberately absent:
`languages.md`, because this repo ships the Go implementation only, and
`manual-setup.md`, because there is no manual-setup path. `check-docs.sh` omits
both from its required list for that reason.

`types-server.md` **is** missing: this repo has a types-server (port 8100), but
it is documented in the repo README's [Types Server](../README.md#types-server)
section instead of a doc of its own, so `check-docs.sh` warns rather than fails.

## Extension-specific

| Doc | Answers |
| --- | --- |
| [extension-contract.md](extension-contract.md) | the normative container contract an implementation must satisfy |
| [production-allowlisting.md](production-allowlisting.md) | authorize a `codeHash` on-chain without circular trust |
| [security/DEPENDENCY-EXCEPTIONS.md](security/DEPENDENCY-EXCEPTIONS.md) | accepted `govulncheck` advisories on the proxy build, and why |

Also at the repo root:

| Doc | Answers |
| --- | --- |
| [REPRODUCIBILITY.md](../REPRODUCIBILITY.md) | what the image build guarantees for the registered `codeHash` |

## Style

Written for testers, not authors. Short, plain, skimmable: tables over prose,
runnable commands over description, and a symptom→cause table for failures. If a
section does not change what someone types or decides, cut it.
