# Releases and installers

## Supported binaries

GitHub Actions builds precompiled, CGO-free binaries:

| OS | CPU | Release asset |
| --- | --- | --- |
| Linux | AMD64 | `agentmux_linux_amd64.tar.gz` |
| Linux | ARM64 | `agentmux_linux_arm64.tar.gz` |
| macOS | AMD64 | `agentmux_darwin_amd64.tar.gz` |
| macOS | ARM64 | `agentmux_darwin_arm64.tar.gz` |
| Windows | AMD64 | `agentmux_windows_amd64.zip` |
| Windows | ARM64 | `agentmux_windows_arm64.zip` |

Each release includes `SHA256SUMS` with the SHA-256 of every archive.

## Installation

The install scripts require **Bash** on Unix or **PowerShell** on Windows.
They never invoke Go, Git, npm, Node, Python, mise or a local build.
They select the OS and architecture, download one archive and its checksum
manifest from the latest GitHub Release, verify the archive, extract the
executable, and install it into `~/.local/bin/`. They refuse to overwrite an
existing executable and perform a binary version smoke test.

Set `AGENTMUX_VERSION` to an existing published tag (for example,
`AGENTMUX_VERSION=v0.2.2`) to select a specific release.
Set `AGENTMUX_BIN_DIR` for a custom user-owned destination.
`AGENTMUX_RELEASE_BASE_URL` is used by local installer test fixtures and
normally should remain unset.

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
   both successful installation and refusal to overwrite an existing binary.
   The checksum and `agentmux version` are verified as part of installation.

The release workflow also accepts a manual trigger with an existing tag.
Publishing requires GitHub Actions `contents: write` permission only for
the publish job. Never publish artifacts built from uncommitted local changes.
