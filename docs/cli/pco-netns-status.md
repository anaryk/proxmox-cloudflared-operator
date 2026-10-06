# pco netns status

Show the legs, the service addresses and the sticky entries of the node's cache of the
gateway, which the daemon writes after every change, and compare the namespace pco, its
links, addresses, routes and rules, the transit link, the routes of the service prefix on
the host, both tables with their elements and the sticky entries with it. It changes nothing,
and exits with 1 when anything differs; systemctl restart pco-netns brings back what is
missing.

## Usage

```text
pco netns status [flags]
```

## Examples

```text
# On the node
pco netns status

# Bring back what it names as missing
systemctl restart pco-netns
```

## Flags

```text
-h, --help   help for status
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco netns](pco-netns.md): Show the gateway namespace the attached networks are reached through
- [Command reference](index.md): every command of pco, by group
