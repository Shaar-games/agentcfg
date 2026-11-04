# Releases and installers

## Supported binaries

GitHub Actions builds precompiled, CGO-free binaries:

| OS | CPU | Release asset |
| --- | --- | --- |
| Linux | AMD64 | `agentcfg_linux_amd64.tar.gz` |
| Linux | ARM64 | `agentcfg_linux_arm64.tar.gz` |
| macOS | AMD64 | `agentcfg_darwin_amd64.tar.gz` |
| macOS | ARM64 | `agentcfg_darwin_arm64.tar.gz` |
| Windows | AMD64 | `agentcfg_windows_amd64.zip` |
| Windows | ARM64 | `agentcfg_windows_arm64.zip` |

Each release includes `SHA256SUMS` with the SHA-256 of every archive.

## Installation

The install scripts require **Bash** on Unix or **PowerShell** on Windows.
They never invoke Go, Git, npm, Node, Python, mise or a local build.
They select the OS and architecture, download one archive and its checksum
manifest from the latest GitHub Release, verify the archive, extract the
executable, and install it into `~/.local/bin/`. Reinstalling **replaces**
the existing executable after checking the archive and staged binary. A
temporary backup is kept until the new binary passes its version smoke test.
On Windows, an old running executable may leave a backup that can be removed
after the process exits. The installer targets only `agentcfg` (`agentcfg.exe`
on Windows), without modifying MCP or harness configuration.

Set `AGENTCFG_VERSION` to an existing published release tag to select a
specific version.
Set `AGENTCFG_BIN_DIR` for a custom user-owned destination.
`AGENTCFG_RELEASE_BASE_URL` is used by local installer test fixtures and
normally should remain unset.

## Self-update

```sh
agentcfg update
```

The command queries the latest **stable GitHub Release targeting `main`**,
downloads the platform archive plus `SHA256SUMS`, checks the SHA-256 digest,
extracts only the expected executable, verifies that it reports the release
version, and replaces the executable currently running agentcfg. It does not
touch native harness configurations, MCP definitions or skills. No Go toolchain
is required.

Unix uses a same-filesystem atomic rename and a backup for rollback. Windows
renames the old executable before installing the replacement, so an active
agentcfg process can keep using its already-mapped image. If Windows cannot
delete that backup while it remains in use, its path is printed for cleanup
after the updater exits. Failure before replacement leaves the existing binary
untouched; a failed installed-binary smoke test triggers restoration.

**Checksum verification is not publisher authentication.** Since the checksum
comes from the same GitHub Release as the binary, a compromised release could
replace both. Signed provenance/attestations are potential future enhancements.

## Publishing a version

1. Confirm the CI matrix is green on the intended commit.
2. Create and push an annotated semantic version tag such as `v0.2.0`.
3. `.github/workflows/release.yml` verifies, builds all six targets, generates
   `SHA256SUMS`, and attaches the assets to a GitHub Release.
4. The release workflow runs a post-publication installer smoke matrix on
   Linux, macOS and Windows, downloading real release artifacts and checking
   both successful installation and successful replacement of an existing agentcfg binary.
   The checksum, `agentcfg version` and no-argument CLI help are verified as part of installation.

The release workflow also accepts a manual trigger with an existing tag.
To retry a release after a workflow-only fix, dispatch the current `main`
workflow with that existing tag. Every build and installer still checks out
the immutable tag; published assets are never overwritten.
Publishing requires GitHub Actions `contents: write` permission only for
the publish job. Never publish artifacts built from uncommitted local changes.

Once installed and available on `PATH`, the executable can generate
[shell completion](completion.md) for Bash, Zsh, Fish, Windows PowerShell
and PowerShell 7. The installation scripts do not edit shell profiles.
