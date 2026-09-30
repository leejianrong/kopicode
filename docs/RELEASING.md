# Releasing kopicode

A release is a git tag `vX.Y.Z` on `main`. Pushing the tag is the one manual, deliberate
step: [`.github/workflows/release.yml`](../.github/workflows/release.yml) reacts to it,
cross-compiles every platform (`make xbuild`), stages `kopicode-<os>-<arch>` and
`kopibench-<os>-<arch>` assets plus a `SHA256SUMS` file, and publishes the GitHub
release. [`scripts/install.sh`](../scripts/install.sh) installs from the latest one.

## Versioning

Semantic versioning, while pre-1.0:

- **Minor (`0.Y.0`)** — new user-visible capability, or any change to a compatibility
  surface a consumer codes against: the `run --print` NDJSON schema and exit codes
  ([`run-print-protocol.md`](run-print-protocol.md)), the `kopicode serve` wire
  ([`kopicode-serve-protocol.md`](kopicode-serve-protocol.md)), the journal event schema,
  or a harness-config change that alters an arm's hash. Read the release notes before
  upgrading across a minor.
- **Patch (`0.Y.Z`)** — fixes that change none of the above.
- **`1.0.0`** waits until those wire surfaces are settled enough to promise not to
  break; until then a minor may break them, and the notes say so.

A benchmark result is tied to a binary through `internal/build`'s stamped identity, never
to the tag alone — a tag names a moment, the commit names the code.

## Cutting a release

1. `main` is green and contains what the release should. `git fetch && git switch main
   && git pull --ff-only`; the tree is clean.
2. `make ci` passes locally (the same gates the release re-runs).
3. Tag and push: `git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z`.
4. Watch the **Release** workflow. It creates the release with `--generate-notes`; edit
   the notes to lead with anything that breaks a compatibility surface above.
5. Verify from the published assets, not from your build tree:
   ```bash
   cd "$(mktemp -d)"
   gh release download vX.Y.Z --repo leejianrong/kopicode
   sha256sum -c --ignore-missing SHA256SUMS
   chmod +x kopicode-linux-amd64 && ./kopicode-linux-amd64 version   # must print vX.Y.Z, via ldflags
   ```
   `version` reporting a pseudo-version or `unknown` means the build lost its tag
   (`fetch-depth: 0` in the workflow) — fix before announcing.
6. Update anything that names the latest version by hand (there should be none; the
   installer resolves `latest`).

The checksums catch a corrupted or truncated download. They do not defend against a
compromised release, because they are served from the same place as the binaries.

## Not yet

Signed artifacts / provenance attestation, a Homebrew tap, and Windows packaging beyond the
raw `.exe` are deliberately not built. Add them when there is a user who needs them.
