# Accepted dependency advisories

Status of `govulncheck` findings against the **hardened tee-proxy build**
(`proxy/Dockerfile`: tee-proxy `v0.0.18` @ `2e84129`, Go 1.25.13, security
backports baked into the image). Raw output: [`govulncheck-tee-proxy.txt`](govulncheck-tee-proxy.txt).

**Symbol-reachable advisories: 0** (the unhardened upstream build had 35).

The two remaining findings are *not reachable* from the proxy entrypoint —
`govulncheck` classifies them as "imported / required but never called". They
are accepted with the following assessment and should be re-checked whenever
the tee-proxy pin or the Go toolchain changes:

| Advisory | Component | Why it is accepted |
|---|---|---|
| GO-2026-5942 | Go stdlib `net` (SVCB/HTTPS DNS RR parse panic) | Fixed only in go1.26.6; no go1.25.x backport exists as of this writing. The vulnerable symbol (`dnsmessage` SVCB parsing) is not called by `cmd/proxy` — the proxy performs no SVCB/HTTPS-RR lookups. Revisit when moving to a Go 1.26 toolchain. |
| GO-2026-5932 | `golang.org/x/crypto/openpgp` (unmaintained by design, no fixed version) | Pulled in transitively; the openpgp package is never imported by any code path in the proxy binary. There is no fixed release to bump to (`Fixed in: N/A`); removal must happen upstream. |

## Extension modules (go/, tools/)

Both extension modules pin `toolchain go1.25.13` and their direct dependency
versions are at or above every fixed version listed above. Run in CI:

```
govulncheck ./...        # in go/ and tools/
```

Any new symbol-reachable finding is a build blocker; a new non-reachable
finding gets a row in the table above or a dependency bump, whichever is
cheaper and safer.
