# Releasing pco

A release is an annotated tag, which only the maintainers create. The
`release` workflow builds the packages and the appliance templates from it,
waits for the maintainer's approval, signs `checksums.txt`, checks the result
and publishes it. No release is signed or published without that one
approval, the monthly ones included.

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
   fingerprint where `README.md`, `docs/quickstart.md` and `SECURITY.md` do, so
   that an admin can check the key of the script against it. The key in the repository now is
   `3D326CB52862A2E91C9919EFA98A1ED57B31F91B`.

4. On GitHub, in this order:
   - Settings, Environments: create `release`. Under "Deployment branches and
     tags" choose "Selected branches and tags" and add the tag rule `v*`. Add
     the maintainer as its required reviewer: this approval, before the key is
     used, is the only one a release waits for.
   - In that environment add the secret `PCO_RELEASE_GPG_KEY`, the armored
     private key (`gpg --armor --export-secret-keys <fingerprint>`). Delete a
     repository secret of the same name, if there is one.
   - Settings, Rules, Rulesets: create a tag ruleset for `v*` that lets only
     the admins create such tags, with nothing on its bypass list, and a
     second one that blocks updates and deletions of them. No workflow pushes
     a release tag: the monthly rebuild asks for one in an issue (see "The
     monthly rebuild").
   - Settings, Actions, General: under "Workflow permissions" allow GitHub
     Actions to create pull requests, which the workflow `cloudflared` opens.
   - Settings, General, Releases: enable release immutability.

## A release

1. Merge what goes into it and wait for `ci` to pass on `main`.
2. Tag the commit with an annotated tag and push it. A final release is
   `vX.Y.Z`, a pre-release `vX.Y.Z-rc.N`; the workflow refuses any other
   name, and a lightweight tag. Push one tag at a time and wait for its run to
   end: the `release` concurrency group keeps one running and one pending run,
   and a newer pending run replaces an older one.

       git tag -a v0.1.0 -m "pco 0.1.0"
       git push origin v0.1.0

3. The job `ui` builds the web interface first (see below). The job `build`
   reads the tag, builds the packages and from them the appliance templates
   for amd64 and arm64, and boots the amd64 one (see below). Neither holds a
   secret.
4. The job `sign`, in the environment `release`, waits for the maintainer's
   approval. Look at the run of `build`, then approve. The job refuses to
   start building when `scripts/install.sh` at the tag still has the
   placeholder, has a block the installer cannot decode, or does not carry
   the key of the secret. Then it builds the packages again, adds the
   templates, their SBOMs, `cloudflared-versions.json` and
   `pco-appliance_<version>.pin.conf` to `checksums.txt`, signs it and leaves
   a draft release. It checks the file names, the checksums, the control
   files of the packages, that each package carries the web interface, that
   each template carries the programs of the packages byte for byte, and the
   signature (with `gpgv` and `sqv`, against the keys in
   `scripts/install.sh`). When anything fails, it deletes the draft, so that
   no release that failed a check can be published by hand; the tag stays.
5. The job `publish` publishes the draft. Only the highest final version is
   marked as the latest release, so a pre-release, or a fix for an older
   line, is not what the installer picks.

A tag is never moved or deleted. If a run fails for a reason outside the
repository, run the failed jobs again. Otherwise release the next version.

The templates are built from the Debian snapshot that a line
`Snapshot: YYYYMMDDTHHMMSSZ` of the tag's message names, or from the one in
`packaging/appliance/pin.conf` at the tag when the message has no such line.
This is how the monthly rebuild asks for its tags:

    git tag -a v0.1.1 -m "pco 0.1.1" -m "Snapshot: 20261102T000000Z"

A run started by hand takes the snapshot from its input instead, to build the
release of a tag again from another snapshot. A pushed tag starts its own run,
so there is no need for one by hand next to it:

    gh workflow run release.yml --ref v0.1.1 -f snapshot=20261201T000000Z

The body of the tag's message, after its first line, heads the notes of the
release.

## The appliance templates

`packaging/appliance/README.md` says what a template holds and how it is
built. In a release the job `build` makes them, without a secret and outside
the environment `release`, so that mmdebstrap, Debian's packages and qemu never
run where the key is, and keeps the cloudflared packages it installed beside
them. The job `sign` downloads them, builds the packages again from the same
tag and refuses the release (`check-artifacts.sh --require-template`) unless
each template carries the `/usr/bin/pco` of the package built there and the
`/usr/bin/cloudflared` of the cloudflared package whose sha256
`packaging/cloudflared-versions.json` lists, byte for byte. The sha256 in
`pco-appliance_<version>_<arch>.deb.sha256` is looked at first. The two builds
of the packages are the same as long as they are reproducible; the `package`
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
2. When something changed, the job `cut` opens the issue
   `template: tag v0.1.1`, or comments on it while it is open. It names the
   commit of that release, the next patch version (`v0.1.1` after `v0.1.0`),
   the snapshot, the Debian packages that changed, and the commands that tag
   it:

       git fetch --tags origin
       git tag -a v0.1.1 -m "pco 0.1.1" -m "Snapshot: 20261102T000000Z" <commit>
       git push origin v0.1.1

   No workflow pushes a release tag, as only the maintainers may create one.
   The push starts the release workflow, which builds the templates from the
   snapshot the tag names and waits for the approval of `release`. The notes
   of the release begin with the line `Snapshot:`; the issue holds the text
   to add to them, which says that pco itself is unchanged and lists the
   packages.
3. When a build or a boot fails, or the issue cannot be written, the job
   `report` opens an issue, or comments on the open one, and nothing is
   released.

A tag of the next version without a published release, a release on its way
or one that failed, stops the rebuild: it fails until that release is
published. If it failed, release the version after it by hand, with the
snapshot of the issue.

## cloudflared versions

The appliance carries cloudflared from `packaging/cloudflared-versions.json`,
the versions that were vetted, not from Cloudflare's apt repository, which
offers only the newest one. Each entry names the packages of both
architectures and their sha256; `deny` lists versions that must not be used,
each with a reason that names something a reader can check: an issue
(`cloudflare/cloudflared#1737`), an advisory (a CVE or GHSA id) or an https
URL. `updated` is the day the list last changed. The newest version
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
first. To refuse a version, move its entry to `deny` with such a reason. CI does
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
release workflow loses this order, when `ui` or `build` gets a secret or more
than read access, when a job but `sign` runs in an environment, when `sign`
no longer deletes the draft of a failed run, or deletes it before the key is
gone, or when any job but `publish` publishes. It also runs the step of
`build` that reads the tag against annotated and lightweight tags, with and
without a line `Snapshot:`. The workflow `ui-audit` runs
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
   and from `RELEASE_FPRS`, and change the fingerprint in `README.md`,
   `docs/quickstart.md` and `SECURITY.md` to the new one.
