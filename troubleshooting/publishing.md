# Publishing runbook

This is the exact process used to publish `observe-claude` so end users can
`brew install` (macOS) or `scoop install` (Windows), plus every problem we hit
and how it was fixed.

## The big picture

There is **one binary**, `observe-claude` (subcommands `init` / `serve` /
`hook`). "Publishing" means: [GoReleaser](https://goreleaser.com) builds that
binary for every OS/arch on a git tag, then:

```
 git tag v0.1.1
        │
        ▼
   GoReleaser
        ├── uploads binaries + checksums → GitHub Release (this repo)
        ├── writes a Homebrew cask        → kritnambutt/homebrew-tap
        └── writes a Scoop manifest        → kritnambutt/scoop-bucket
                                                     │
                        brew install / scoop install ┘  (end users)
```

Config lives in [`../.goreleaser.yaml`](../.goreleaser.yaml). CI lives in
[`../.github/workflows/release.yml`](../.github/workflows/release.yml).

## One-time setup (already done, here for reference)

```sh
# 1. GoReleaser itself (macOS)
brew install goreleaser

# 2. The two repos GoReleaser pushes the cask / manifest into.
#    Names matter — GoReleaser targets them by name.
gh repo create kritnambutt/homebrew-tap  --public --add-readme --description "Homebrew tap for observe-claude"
gh repo create kritnambutt/scoop-bucket  --public --add-readme --description "Scoop bucket for observe-claude"
```

For releasing **via GitHub Actions** (instead of locally) you also need a
Personal Access Token with `repo` scope stored as the Actions secret
`GORELEASER_TOKEN` — the default `GITHUB_TOKEN` can't push to the tap/bucket
repos. See `../RELEASING.md`.

## Cut a release (the commands that actually worked)

Always validate first:

```sh
goreleaser check                          # config valid for your GoReleaser version?
goreleaser build --snapshot --clean       # do all platforms compile? (fast, publishes nothing)
# optional full dry run incl. cask/manifest generation, still publishes nothing:
goreleaser release --snapshot --clean
```

Then the real release (run from the repo root, clean git tree):

```sh
git tag -a v0.1.1 -m "observe-claude v0.1.1"
GITHUB_TOKEN="$(gh auth token)" goreleaser release --clean
rm -rf dist                               # build output; also gitignored
```

> We released **locally** rather than through CI, so we never had to store a
> token as a repo secret. `gh auth token` supplies a token that already has
> `repo` scope. Doing it locally also means the CI workflow (which fires on a
> pushed tag) doesn't run half-configured.

### Verify the release landed

```sh
gh release view v0.1.1 -R kritnambutt/observe-claude --json tagName,assets -q '.tagName, (.assets[].name)'
gh api repos/kritnambutt/homebrew-tap/contents/Casks/observe-claude.rb -q '.path'
gh api repos/kritnambutt/scoop-bucket/contents/observe-claude.json    -q '.path'
```

### Test the install like a real user

```sh
brew uninstall observe-claude 2>/dev/null; brew untap kritnambutt/tap 2>/dev/null
brew install kritnambutt/tap/observe-claude
observe-claude version        # should print the released version and run cleanly
```

---

## Problems we hit, and the fixes

### 1. `zsh: command not found: goreleaser`
**Cause:** GoReleaser wasn't installed.
**Fix:** `brew install goreleaser` (or `go install github.com/goreleaser/goreleaser/v2@latest`).

### 2. `brew install …` → `Repository not found: homebrew-tap`
**Cause:** the `homebrew-tap` repo didn't exist yet, and no release had been
published to populate it. `brew install owner/tap/x` first clones
`owner/homebrew-tap`.
**Fix:** create the tap + bucket repos, then cut a release (both above). Nothing
to install until a release exists.

### 3. `goreleaser check` → deprecated `archives.format_overrides.format`
**Cause:** GoReleaser v2 renamed the field to a list.
**Fix:** in `.goreleaser.yaml`, use `formats: [zip]` instead of `format: zip`.

### 4. macOS: `observe-claude` installs but prints nothing / is killed on first run
**Cause:** the binary is **unsigned**, so macOS Gatekeeper quarantines it
(`com.apple.quarantine` extended attribute) and silently blocks it.
**Immediate fix:** `xattr -dr com.apple.quarantine "$(which observe-claude)"`.
**Permanent fix (shipped):** a cask post-install hook that strips the attribute
on install, in `.goreleaser.yaml`:

```yaml
homebrew_casks:
  - # …
    hooks:
      post:
        install: |
          if OS.mac?
            system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{staged_path}/observe-claude"]
          end
```

Check whether a binary is quarantined:
`xattr -p com.apple.quarantine "$(which observe-claude)"` (no output = clean).
The real long-term fix is code signing (Apple Developer ID).

### 5. Release fails: `compare/v0.1.0...v0.1.1: 404 Not Found` (changelog)
**Cause:** `changelog.use: github` calls GitHub's compare API, which needs the
new tag already pushed to GitHub. We release locally without pushing the tag
first, so GitHub 404s.
**Fix:** use the local git history for the changelog instead:

```yaml
changelog:
  use: git
```

### 6. (CI only) release job fails pushing to the tap/bucket
**Cause:** the default Actions `GITHUB_TOKEN` has no write access to *other*
repos (`homebrew-tap`, `scoop-bucket`).
**Fix:** create a PAT with `repo` scope, store it as the `GORELEASER_TOKEN`
Actions secret; the workflow passes it as `GITHUB_TOKEN`.

### Windows note
Scoop couldn't be tested from macOS, but the manifest is published and validated
by GoReleaser. The unsigned `.exe` may trip Windows SmartScreen on first run
("More info" → "Run anyway"); code signing is the eventual fix, same as macOS.

---

## Handy references

```sh
observe-claude version                    # what version am I running?
which observe-claude                       # where did brew link it?
brew info kritnambutt/tap/observe-claude   # cask details / installed version
gh release list -R kritnambutt/observe-claude
goreleaser --version
```
