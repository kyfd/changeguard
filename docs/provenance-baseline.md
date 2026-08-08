# Source provenance baseline

## Status

Historical Git metadata for ChangeGuard was unavailable when this repository baseline was established on 2026-08-08. Workspace and production-host inspection found source snapshots through 2026-08-04, but the active production release `/opt/changeguard/releases/20260807-panorama-v3-20260807-110449` contains only binaries and environment files. Rebuilding the available source with the observed production toolchain did not reproduce the active binary SHA-256.

The first commit containing this document is therefore a **new trustworthy baseline**, not a recovered commit and not proof of the active production binary's source. No tag, manifest, report, or later release may back-attribute this baseline to an older production artifact.

## Release invariant

Future core candidates must be built by `deploy/production/build-core-git-release.sh`. The wrapper requires:

- a clean Git worktree with no untracked source files;
- an annotated release tag that resolves exactly to `HEAD`;
- a verification evidence JSON file whose version, annotated tag, commit, source digest, and Linux target match the candidate exactly;
- an explicit, pre-populated absolute `CHANGEGUARD_GOMODCACHE`; release builds require the complete module graph, verify its checksums, clear inherited proxy variables, and force `GOPROXY=off` so assembly cannot fetch new dependencies;
- a stable source digest over `cmd`, `internal`, `go.mod`, and `go.sum`;
- embedded version, commit, commit time, source digest, and Go version;
- an external release directory containing the binary, verified Git bundle, source archive, dependency list, binary build metadata, verification evidence, build log, and SHA-256 manifest. An interrupted build is retained with a `.incomplete` marker for inspection and is never reported as a release.

Candidate health, startup logs, and Prometheus provenance must agree before any traffic is sent. A production-data-copy regression and reversible no-traffic staging run are required before gray release. The active production soft link must remain unchanged until those gates pass.
