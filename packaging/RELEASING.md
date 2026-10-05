# Releasing pco

A release is a tag. The `release` workflow builds the packages from it, signs
`checksums.txt`, checks the result and only then publishes it.

## Once, before the first release

1. Make the release key: one OpenPGP signing key without a passphrase, which
   the workflow imports with `gpg --batch`. Keep a copy of the private key
   outside GitHub.
2. `scripts/install_test.sh` finds the key block in `scripts/install.sh` by its
   `read -r -d '' PCO_RELEASE_KEY_B64` line and the `EOF` that closes it, so it
   works with any key in the block. It pins the fingerprint of the key, in
   `RELEASE_FPRS`, and checks that the keyring in the script holds that key and
   no other primary key.
3. Paste the public key into `scripts/install.sh`, in place of what the block
   holds:

       gpg --export <fingerprint> | base64 | tr -d '\n' | fold -w 64

   Check that the script reads it back, and commit it. It prints the
   fingerprint of every key in the block:

       packaging/release-key.sh scripts/install.sh /tmp/release.gpg

   Change `RELEASE_FPRS` in `scripts/install_test.sh` to match, and name the
   fingerprint where `README.md` and `docs/quickstart.md` do, so that an admin can
   check the key of the script against it. The key in the repository now is
   `3D326CB52862A2E91C9919EFA98A1ED57B31F91B`.

4. On GitHub, in this order:
   - Settings, Environments: create `release`. Add a required reviewer. Under
     "Deployment branches and tags" choose "Selected branches and tags" and
     add the tag rule `v*`.
   - In that environment add the secret `PCO_RELEASE_GPG_KEY`, the armored
     private key (`gpg --armor --export-secret-keys <fingerprint>`). Delete a
     repository secret of the same name, if there is one.
   - Settings, Rules, Rulesets: create a tag ruleset for `v*` that restricts
     creation to the maintainers and blocks updates, deletions and force pushes.
   - Settings, General, Releases: enable release immutability.

## A release

1. Merge what goes into it and wait for `ci` to pass on `main`.
2. Tag the commit and push the tag. A final release is `vX.Y.Z`, a
   pre-release `vX.Y.Z-rc.N`; the workflow refuses any other name. Push one
   tag at a time and wait for its run to end: the `release` concurrency group
   keeps one running and one pending run, and a newer pending run replaces an
   older one.

       git tag -a v0.1.0 -m "pco 0.1.0"
       git push origin v0.1.0

3. The job `ui` builds the web interface first (see below). Approve the run of
   the `release` environment when the workflow waits for it.
4. The workflow refuses to start building when `scripts/install.sh` at the tag
   still has the placeholder, has a block the installer cannot decode, or does
   not carry the key of the secret. Then it builds a draft release, checks the
   file names, the checksums, the control files of the packages, that each
   package carries the web interface, and the signature (with `gpgv` and
   `sqv`, against the keys in `scripts/install.sh`), and publishes the draft.
   Only the highest final version is marked as the latest release, so a
   pre-release, or a fix for an older line, is not what the installer picks.

A tag is never moved. If a run fails for a reason outside the repository, run
the job again. Otherwise delete the draft release and release the next version.

## The web interface

The packages carry the web interface, which Vite builds from `web/` into
`internal/web/ui/dist` and the build tag `webui` embeds. npm, and every package
it installs, never runs where the release key is:

- The job `ui` checks out the tag, sets up Node with `actions/setup-node`, runs
  `make ui-test ui-budget` and uploads `internal/web/ui/dist` as the artifact
  `ui-dist`. It may only read the repository, and has no environment and no
  secret. The job of the same name in `ci` does the same for every push.
- The job `release` needs `ui` and downloads `ui-dist` right after the
  checkout, before the signature tools are installed and before the key is
  imported. Nothing of npm runs in it; goreleaser's `before` hook only checks
  that `internal/web/ui/dist/index.html` is there.
- `packaging/check-artifacts.sh --require-ui` reads `usr/bin/pco` out of each
  package and fails unless `go version -m` lists `-tags=nomsgpack,webui`.
  Without the tag the binary serves a page that says it has no web interface.
  The `package` job of `ci` and `make snapshot` run the same check, which needs
  `dpkg-deb` and `go` (on macOS, `brew install dpkg`).

`packaging/release-workflow_test.sh`, part of `make test-scripts`, fails when the
release workflow loses this order. The workflow `ui-audit` runs
`npm audit --omit=dev` every week and when `web/package-lock.json` changes; it
is not a required check, so an advisory or a registry that does not answer
blocks no pull request.

## Node and npm

Node and npm are pinned exactly, so a newer toolchain cannot change the build
without a commit: Node 22.23.3 in `web/.nvmrc`, and npm 10.9.9, the npm that
release of Node ships, in `packageManager` and `engines` of `web/package.json`.
`web/.npmrc` sets `engine-strict`, so `npm ci` refuses any other version.
`actions/setup-node` reads `web/.nvmrc` and brings the npm that goes with it.

To raise them:

1. Pick the release of Node from <https://nodejs.org/dist/index.json>; its
   `npm` field is the version of npm it ships.
2. Put the Node version into `web/.nvmrc` and `engines.node`, the npm version
   into `engines.npm` and `packageManager`, and both into this section.
3. With that Node installed (`nvm install` in `web/` reads `.nvmrc`), run
   `make ui-test ui-budget` and commit the files, with `web/package-lock.json`
   if npm changed it.

## Replacing the key

The installers of old releases carry the old key only, so the new key goes in
beside it first:

1. Put both public keys into the block of `scripts/install.sh`
   (`gpg --export <old> <new> | base64 ...`), list both fingerprints in
   `RELEASE_FPRS` of `scripts/install_test.sh`, and release, still signed with
   the old key.
2. Replace the secret with the new private key and release again. The workflow
   accepts a secret that is any of the keys in `install.sh`.
3. Later, when the old installers are gone, drop the old key from `install.sh`
   and from `RELEASE_FPRS`, and change the fingerprint in `README.md` and
   `docs/quickstart.md` to the new one.
