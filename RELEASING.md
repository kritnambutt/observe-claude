# Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) from a git tag: it
cross-compiles the `observe-claude` binary for macOS / Windows / Linux
(amd64 + arm64), uploads archives to GitHub Releases, and updates a Homebrew
cask and a Scoop manifest so end users can `brew install` / `scoop install`.

## One-time setup

1. **Create two empty public repos** under the `kritnambutt` account (names
   matter — GoReleaser pushes to them):
   - `homebrew-tap` → the Homebrew cask lands here (`brew install kritnambutt/tap/observe-claude`).
   - `scoop-bucket` → the Scoop manifest lands here.

2. **Create a token GoReleaser can push with.** The default Actions
   `GITHUB_TOKEN` can't write to *other* repos (the tap/bucket), so make a
   Personal Access Token:
   - Classic PAT with the `repo` scope, **or** a fine-grained token granting
     `contents: read/write` on `observe-claude`, `homebrew-tap`, and
     `scoop-bucket`.
   - Add it to this repo as an Actions secret named **`GORELEASER_TOKEN`**
     (Settings → Secrets and variables → Actions).

## Cut a release

```sh
# validate the config against your installed GoReleaser first
goreleaser check

# optional: full dry run, builds everything locally, publishes nothing
goreleaser release --snapshot --clean

# real release
git tag v0.1.0
git push origin v0.1.0        # → triggers .github/workflows/release.yml
```

The workflow runs `goreleaser release --clean`. When it finishes you'll have a
GitHub Release with binaries + checksums, an updated cask in `homebrew-tap`,
and an updated manifest in `scoop-bucket`.

## Notes

- **Version stamping.** The tag flows into the binary via
  `-ldflags -X …/internal/cli.Version=<tag>`, so `observe-claude version`
  prints the released version.
- **Code signing.** Binaries are unsigned, so first launch on macOS may hit
  Gatekeeper (right-click → Open, or `xattr -dr com.apple.quarantine`) and
  Windows SmartScreen may warn. Signing is a later addition (Apple Developer
  ID / a Windows code-signing cert) and slots into `.goreleaser.yaml`.
- **winget** (the Windows Package Manager) is intentionally not automated here:
  it requires PRs into `microsoft/winget-pkgs` with manual review. Scoop is the
  self-hosted, instant Windows channel. GoReleaser has a `winget:` block that
  can be added later if you want `winget install` too.
