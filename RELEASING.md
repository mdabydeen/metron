# Releasing

metron is released by pushing a tag. Everything else is automated.

## Before the first release

Three things need doing once, and the release will be incomplete without them:

1. **Create `mdabydeen/homebrew-tap`** (a public repository, empty is fine) and
   add a `HOMEBREW_TAP_TOKEN` secret to this repository — a fine-grained PAT
   with `contents: write` on the tap. Without it the release still publishes;
   only the cask update is skipped, and the workflow says so rather than
   failing.
2. **Enable Discussions**, or remove that link from
   `.github/ISSUE_TEMPLATE/config.yml`. It currently points at a page that does
   not exist yet.
3. **Read `CHANGELOG.md` top to bottom.** It is the release notes.

## Cutting a release

```bash
make check          # vet, hermetic tests, race, 100% coverage
make docker-test    # the integration tier, against real rg/ctags/git
make lint           # golangci-lint at the pinned version
make release-check  # goreleaser validates .goreleaser.yaml

git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The `release` workflow runs the integration tier first, then publishes: six
archives (darwin and linux, amd64 and arm64), a checksum file, and a Homebrew
cask.

## Afterwards

- `brew install mdabydeen/tap/metron` should work within a minute or two.
- `go install github.com/mdabydeen/metron/cmd/metron@v0.1.0` should work
  immediately. Note that it reports version `dev`: Go's module installer does
  not run the Makefile's ldflags, so a bug report from a `go install` user will
  not carry a version. The issue template asks for one anyway, so expect to have
  to ask.
- Open a new `## [Unreleased]` section at the top of `CHANGELOG.md`.

## What is not automated, deliberately

Nothing signs the binaries. They are unsigned, which is why the Homebrew cask
strips the Gatekeeper quarantine attribute on install — without that, the first
run of a downloaded binary fails with "cannot be opened". If metron ever gets a
signing identity, that hook is the thing to remove.
