# Releasing pco

A release is a tag. The `release` workflow builds the packages from it, signs
`checksums.txt`, checks the result and only then publishes it.

## Once, before the first release

1. Make the release key: one OpenPGP signing key without a passphrase, which
   the workflow imports with `gpg --batch`. Keep a copy of the private key
   outside GitHub.
2. Paste the public key into `scripts/install.sh`, in place of the placeholder:

       gpg --export <fingerprint> | base64 | tr -d '\n' | fold -w 64

   Check that the script reads it back, and commit it:

       packaging/release-key.sh scripts/install.sh /tmp/release.gpg

3. On GitHub, in this order:
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
   pre-release `vX.Y.Z-rc.N`; the workflow refuses any other name.

       git tag -a v0.1.0 -m "pco 0.1.0"
       git push origin v0.1.0

3. Approve the run of the `release` environment when the workflow waits for it.
4. The workflow refuses to start building when the key in `scripts/install.sh`
   at the tag is the placeholder or not the key of the secret. Then it builds a
   draft release, checks the file names, the checksums, the control files of the
   packages and the signature (with `gpgv` and `sqv`, against the key in
   `scripts/install.sh`), and publishes the draft. A pre-release is not marked
   as the latest release, so the installer does not pick it.

A tag is never moved. If a run fails for a reason outside the repository, run
the job again. Otherwise delete the draft release and release the next version.

To rotate the key, change the secret and the key in `scripts/install.sh` in
one commit, before the next tag. The workflow stops before building when the two
differ.
