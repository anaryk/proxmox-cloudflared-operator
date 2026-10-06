# pco upgrade

Upgrade pco, cloudflared or both (all, the default) in place, inside the appliance.

pco comes from its latest release, or the one `--version` names. The signature of the
release's checksums.txt is checked with the release key the package ships, and the
package with its line there. cloudflared comes from the manifest of vetted versions the
release lists in that checksums.txt: the newest version it allows, or the one `--version`
names, never one it denies, checked against the sha256 of the manifest. The connectors
restart on the new cloudflared one after the other, and each has 60 s to be ready again.
With all, pco is upgraded first and cloudflared follows the manifest of the release pco
came from.

The package of the version that is replaced is kept in `/var/lib/pco/upgrades/previous`,
one per package, and `--rollback` installs it again. A rollback of pco is refused while the
store holds an object the kept pco cannot read, and one of cloudflared to a version the
manifest denies. pco and cloudflared stay held, so that apt and unattended-upgrades leave
them alone.

pco cannot take a snapshot of its own container. Before an upgrade, take one on the node,
named `pco-pre-upgrade-<YYYYMMDD>`; a rollback to it is followed by pco appliance recover.

`--check` says what is installed and what is available, and which versions of cloudflared
are denied; its exit status is 1 when an upgrade is available. pco upgrade runs as root
inside the appliance; a host upgrades pco and cloudflared with apt.

## Usage

```text
pco upgrade [pco|cloudflared|all] [flags]
```

## Examples

```text
# Say what is installed and what the latest release offers; exit status 1 when there is an upgrade
pco upgrade --check

# Upgrade pco and then cloudflared without the question, as a script does
pco upgrade --yes

# Install the release 1.4.0 of pco, and leave cloudflared as it is
pco upgrade pco --version 1.4.0

# Install cloudflared 2026.9.3, if the manifest allows it
pco upgrade cloudflared --version 2026.9.3

# Go back to the cloudflared the last upgrade replaced
pco upgrade cloudflared --rollback
```

## Flags

```text
    --check            only say what is installed and what is available; exit status 1 when an upgrade is available
-h, --help             help for upgrade
    --rollback         install the package the last upgrade replaced
    --version string   the version of pco or of cloudflared to install, instead of the newest
-y, --yes              do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
