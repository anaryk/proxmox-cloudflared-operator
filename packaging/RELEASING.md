# Releasing pco

A release is a tag. The `release` workflow builds the packages and the
appliance templates from it, signs `checksums.txt`, checks the result and
leaves a draft, which it publishes once the maintainer approved. No release is
published without that approval, the monthly ones included.

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
   - Settings, Environments: create `release`. Under "Deployment branches and
     tags" choose "Selected branches and tags" and add the tag rule `v*`.
   - In that environment add the secret `PCO_RELEASE_GPG_KEY`, the armored
     private key (`gpg --armor --export-secret-keys <fingerprint>`). Delete a
     repository secret of the same name, if there is one.
   - Create the environment `publish`, with the maintainer as its required
     reviewer and the same tag rule `v*`. Its approval is what publishes a
     release. A required reviewer on `release` as well makes every release,
     the monthly ones included, wait for two approvals: one before it is
     signed, one before it is published.
   - Settings, Rules, Rulesets: create a tag ruleset for `v*` that restricts
     creation to the maintainers and blocks updates, deletions and force pushes.
     For the monthly rebuild, add the GitHub Actions app to its bypass list,
     for creation only; without it the job `cut` of `template` cannot push its
     tag (see "The monthly rebuild").
   - Settings, Actions, General: under "Workflow permissions" allow GitHub
     Actions to create pull requests, which the workflow `cloudflared` opens.
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

3. The job `ui` builds the web interface first (see below). The job `build`
   builds the packages and from them the appliance templates for amd64 and
   arm64, and boots the amd64 one (see below). Neither holds a secret.
4. The job `sign`, in the environment `release`, refuses to start building
   when `scripts/install.sh` at the tag still has the placeholder, has a block
   the installer cannot decode, or does not carry the key of the secret. Then
   it builds the packages again, adds the templates, their SBOMs,
   `cloudflared-versions.json` and `pco-appliance_<version>.pin.conf` to
   `checksums.txt`, signs it and leaves a draft release. It checks the file
   names, the checksums, the control files of the packages, that each package
   carries the web interface, that each template carries the package of this
   release byte for byte, and the signature (with `gpgv` and `sqv`, against
   the keys in `scripts/install.sh`).
5. The job `publish` waits for the approval of the environment `publish`.
   Look at the draft, then approve: the job publishes it. Only the highest
   final version is marked as the latest release, so a pre-release, or a fix
   for an older line, is not what the installer picks.

A tag is never moved. If a run fails for a reason outside the repository, run
the job again. Otherwise delete the draft release and release the next version.

The workflow also runs on a tag by hand, with the Debian snapshot to build the
templates from (empty: the one in `packaging/appliance/pin.conf` at the tag):

    gh workflow run release.yml --ref v0.1.1 -f snapshot=20261102T000000Z

The body of an annotated tag's message, after its first line, heads the notes
of the release.

## The appliance templates

`packaging/appliance/README.md` says what a template holds and how it is
built. In a release the job `build` makes them, without a secret and outside
the environment `release`, so that mmdebstrap, Debian's packages and qemu never
run where the key is. The job `sign` downloads them, builds the packages again
from the same tag and refuses the release unless the sha256 of each of its
packages is the one `pco-appliance_<version>_<arch>.deb.sha256` says the
template was built from (`check-artifacts.sh --require-template`). The two
builds are the same as long as the packages are reproducible; the `package`
job of `ci` builds them twice on every push to find out.

## The monthly rebuild

Debian publishes security updates all the time, and a template carries the
packages of its snapshot. The workflow `template` runs on the first Monday of
each month, and by hand:

1. The job `rebuild` downloads the packages and the SBOMs of the latest
   release, checks them against its signed `checksums.txt`, builds both
   templates from those packages and the current Debian snapshot, boots the
   amd64 one without and with a network, and compares the packages with those
   of the release's SBOMs. When nothing changed it stops and says so in the
   summary of the run.
2. When something changed, the job `cut` tags the next patch version on the
   commit of that release, `v0.1.1` after `v0.1.0`, with a message that lists
   the Debian packages that changed, and starts the release workflow on the
   tag with the snapshot it built from. The release then waits as a draft at
   `publish`, its notes saying that pco itself is unchanged.
3. When a build or a boot fails, or the tag cannot be pushed, the job `report`
   opens an issue, or comments on the open one, and nothing is released.

A draft from an earlier month that was never published keeps the next tag:
the rebuild fails until it is published or its tag and draft are deleted.
Without the bypass of the tag ruleset the job `cut` fails at `git push`; tag
and start the release by hand then, with the snapshot the summary of the run
names:

    git tag -a v0.1.1 -m "pco 0.1.1" v0.1.0^{commit}
    git push origin v0.1.1
    gh workflow run release.yml --ref v0.1.1 -f snapshot=<snapshot>

## cloudflared versions

The appliance carries cloudflared from `packaging/cloudflared-versions.json`,
the versions that were vetted, not from Cloudflare's apt repository, which
offers only the newest one. Each entry names the packages of both
architectures and their sha256; `deny` lists versions that must not be used,
with the reason; `updated` is the day the list last changed. The newest version
it allows is the one the templates carry. The file ships in every release,
listed in the signed `checksums.txt`; `packaging/cloudflared-versions_test.sh`,
part of `make test-scripts`, checks its rules.

The workflow `cloudflared` looks at the latest release of cloudflared every
Wednesday. When the list does not name it, allowed or denied, it runs
`packaging/cloudflared-update.sh <version>`, which downloads both packages and
writes their entry, checks the sha256 values against the digests GitHub lists,
and opens the pull request `build: vet cloudflared <version>` from the branch
`cloudflared/<version>`. An open pull request for the same version is left as
it is. Merging it is the vetting: the pull request says what to read and test
first. To refuse a version, move its entry to `deny` with the reason. CI does
not run on a pull request the workflow's token opened; close and reopen it to
run CI.

## The web interface

The packages carry the web interface, which Vite builds from `web/` into
`internal/web/ui/dist` and the build tag `webui` embeds. npm, and every package
it installs, never runs where the release key is:

- The job `ui` checks out the tag, sets up Node with `actions/setup-node`, runs
  `make ui-test ui-budget` and uploads `internal/web/ui/dist` as the artifact
  `ui-dist`. It may only read the repository, and has no environment and no
  secret. The job of the same name in `ci` does the same for every push.
- The job `sign` needs `ui` and `build` and downloads `ui-dist` right after
  the checkout, and the templates right after that, before the signature
  tools are installed and before the key is imported. Nothing of npm runs in
  it, and no template is built there; goreleaser's `before` hook only checks
  that `internal/web/ui/dist/index.html` is there.
- `packaging/check-artifacts.sh --require-ui` reads `usr/bin/pco` out of each
  package and fails unless `go version -m` lists `-tags=nomsgpack,webui`.
  Without the tag the binary serves a page that says it has no web interface.
  The `package` job of `ci` and `make snapshot` run the same check, which needs
  `dpkg-deb` and `go` (on macOS, `brew install dpkg`).

`packaging/release-workflow_test.sh`, part of `make test-scripts`, fails when the
release workflow loses this order, when `ui` or `build` gets a secret, an
environment or more than read access, or when any job but `publish`
publishes. The workflow `ui-audit` runs
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
