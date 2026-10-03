# Annotations

An annotation is the text in a guest's Notes that says which hostnames the guest
serves. This page is the whole grammar, as the parser reads it, with what it
rejects and why.

What gets parsed decides what is published, so doubt is an error: anything that is
not clearly a route is reported, and the entry it belongs to is dropped. The other
entries are not affected.

## Where routes are read

pco reads the Notes of every guest that carries the gate tag, `cf-tunnel` unless the
setting `gateTag` says otherwise. Templates are ignored, and so is a guest without
the tag, whatever its Notes say. pco never writes tags or Notes.

Routes are written in one of two forms.

A fenced block, whose tag is `cf-tunnel`:

~~~text
```cf-tunnel
app.example.com   -> :3000
api.example.com   -> :8080
```
~~~

or a single line that starts with `cf-tunnel:`:

~~~text
cf-tunnel: wiki.example.com -> :8080
~~~

Both can appear in the same Notes, any number of times, and the text around them is
ignored. Keys and the fence tag are case-insensitive.

The details of the two forms:

- A run of three or more backticks opens a block, anywhere in a line, so a
  description that was flattened onto one line still works. Its tag is the text up
  to the next blank. The block ends at the next run of backticks that is at least as
  long, or at the end of the Notes if there is none.
- Every fenced block is read, not only the `cf-tunnel` ones. A block with any other
  tag, or none, is skipped whole, and so is a block that starts with `~~~` at the
  beginning of a line. A route that is merely quoted in a code sample therefore never
  becomes a route.
- A `cf-tunnel:` line counts only outside every block, and it is one whole line. It
  cannot continue on the next line; use a fenced block for that. A code fence on such
  a line is an error.

## The grammar

    block   = entry*
    entry   = host { [","] host } "->" target { option }
    host    = a DNS name, optionally starting with "*."
    target  = [ ("http" | "https") "://" ] [ ipv4 ] ":" port [ "/" ]
    option  = "no-tls-verify"
            | "host-header=" value
            | "sni=" hostname
            | "via=" ( "net" N | ipv4 )

Spaces, tabs, line breaks and commas separate the words of an entry. The arrow `->`
must be a word of its own: `app.example.com->:80` is an error.

### Hostnames

A hostname is written as the public name: `app.example.com`. pco lower-cases it and
takes off one trailing dot. It must have at least two labels, each label made of
letters, digits and hyphens, not starting or ending with a hyphen and at most 63
characters, and at most 253 characters in all. The last label may not be all digits,
so an IPv4 address is never a hostname. Internationalised names are written in their
`xn--` form. Underscores are not accepted.

A leading `*.` makes a wildcard: `*.shop.example.com` with at least two labels after
the star. `*.com` is an error.

Several hostnames in front of one arrow share the target and the options:

~~~text
shop.example.com www.example.com *.shop.example.com -> :80
~~~

### Targets

The target says where the request goes on the guest.

- The scheme is `http` or `https`, in any case, and defaults to `http`.
- The address is an IPv4 address, or left out: `:3000` means the address of the guest
  that pco resolves itself (see [Identity](identity.md)). Loopback, link-local and
  multicast addresses, 0.0.0.0/8 and 240.0.0.0/4 never belong to a guest and are an
  error: `address is not routable to a guest`. IPv6 is not supported.
- The port is 1 to 65535, written without leading zeros.
- A path is an error, `paths are not supported`; a lone trailing `/` is accepted.

An address you name is not trusted. It has to be an address that the guest itself
answers for, which pco checks the same way as one it found. Naming an address only
chooses between the addresses the guest has.

### Options

Options follow the target. Each can be given once.

| Option | Meaning |
|---|---|
| `no-tls-verify` | Do not verify the certificate of an `https` target. Only valid with `https`. |
| `host-header=<value>` | Send this as the Host header. The value may hold letters, digits, `.`, `_`, `:` and `-`, at most 253 characters. |
| `sni=<hostname>` | The name to ask for in the TLS handshake. Only valid with `https`; a wildcard is not accepted. |
| `via=net<N>` | Use the addresses of that network card of the guest, `net0` to `net31`, written without leading zeros. |
| `via=<ipv4>` | Use this address of the guest. Cannot be combined with an address in the target. |

By default the Host header of the visitor's request is passed through unchanged. For
an `https` target the tunnel asks for the TLS name of the public hostname, unless
`sni=` or `no-tls-verify` says otherwise; for a wildcard route it asks for the name
in the Host header of the request.

`via=` matters only for a guest with more than one network card or address: it
narrows the candidates that pco considers. [Identity](identity.md) lists the order
in which they are tried.

### Comments

A `#` that starts a line or follows a blank begins a comment that runs to the end of
the line. A `#` inside a word is part of the word.

### Layout

An entry starts on a line, and the blanks that line starts with are its indent. The
following lines belong to the entry only if they start with that same indent and more.
A line indented the same or less starts a new entry. This is how an entry can be
spread over several lines:

~~~text
```cf-tunnel
shop.example.com
    www.example.com
    *.shop.example.com
  -> https://:443
  no-tls-verify
```
~~~

Rules about continuation lines:

- Before the target, they can hold more hostnames and the arrow.
- After the target, they can hold nothing but options. A hostname there is an error
  (`unknown option`), because it is more likely a mistake than a new entry.
- On the first line of an entry, a word that is a valid hostname after the options
  starts the next entry. That is what lets several entries stand on one line, as in
  `cf-tunnel: a.example.com -> :80 b.example.com -> :81`.
- Indents are compared as text. A tab is never taken for some number of spaces, and
  two indents that cannot be ordered, one with a tab and one with spaces, are an
  error: `inconsistent indentation: mix of tabs and spaces`.

If a Notes editor indents every line of the block by the same amount, nothing changes:
every line still starts a new entry.

## Examples

One port per hostname, which is the usual case for services that each listen on their
own port:

~~~text
```cf-tunnel
app.example.com   -> :3000
api.example.com   -> :8080
grafana.example.com -> :3001
```
~~~

Many hostnames to one reverse proxy on the guest. The proxy gets the Host header of
each request and chooses by it:

~~~text
```cf-tunnel
shop.example.com www.shop.example.com *.shop.example.com -> :80
```
~~~

An origin that speaks TLS, with a certificate that is not valid for the public name:

~~~text
```cf-tunnel
vault.example.com   -> https://:8443 no-tls-verify
admin.example.com   -> https://:443  sni=admin.internal.example.com
```
~~~

A guest with two network cards, a route on the second one, and one with an explicit
address:

~~~text
```cf-tunnel
files.example.com -> :8080 via=net1
nas.example.com   -> http://10.0.0.50:5000
```
~~~

### Wildcards and their order

A wildcard serves every name below it that reaches the tunnel and has no rule of its
own:

~~~text
```cf-tunnel
*.example.com   -> :80
www.example.com -> :8080
```
~~~

The order in the Notes does not matter. A tunnel uses the first rule that matches, so
pco sorts the rules it writes: exact names before wildcards, names with more labels
first, then alphabetically. `www.example.com` therefore goes to port 8080 and every
other name below `example.com` to port 80.

When a wildcard of one guest covers a hostname of another guest, the route of each gets
a warning, `<name> overlaps <name> owned by <owner>`, which `pco routes` shows in the
note column of a route that has no reason to show instead. A name with more than
one label below its zone gets
`more than one level below <zone>: needs an advanced certificate`: Cloudflare's
universal certificate covers one level only.

## Hostnames belong to one guest at a time

A hostname can be written once in the Notes of a guest. Every hostname that is read
counts, also in an entry that is dropped, so a mistake in the first mention does not
make the second one safe. Each later mention is an error and is dropped
(`hostname "x" is listed twice`).

Between guests, the first guest to claim a hostname keeps it for as long as it keeps
asking. A second guest that writes the same hostname, such as the clone of a published
guest, gets the state `conflict` and serves nothing. A claim moves only when its
holder stops asking for it; see [Operations](operations.md) for how long it is kept
and `pco claims` for handing it over on purpose.

To take a hostname down, remove it from the Notes altogether, or remove the tag.
Leaving the name behind in a comment or in prose keeps the claim: pco takes any word
of the Notes that is a valid hostname as a sign that the guest still asks for it. The
name then answers 503, with the state `held` and the note that the guest names it
without a route, until it is gone from the Notes. The same holds for an entry that is
broken: a mistake never unpublishes a hostname by silently dropping it, it blocks it
until the entry is fixed.

The settings can also allow or deny hostnames by pattern, `allowHosts` and
`denyHosts`; see [Operations](operations.md). A hostname that the policy rejects is
an issue, `hostname "x" is not allowed by policy`, and the entry for that name is
dropped.

## What is rejected, and why

Every message about a word comes with the position of that word. The first line of the
Notes is line 1 and the first column is column 1; columns count characters, not
bytes. `pco status` lists them under `Issues` as
`qemu/101 line 2, column 5: message`, ten at most, and `pco status --json` has all
of them in the field `issues`. Two messages are about the guest as a whole and have no
position, and `pco status` lists them as `qemu/101: message`: `tagged cf-tunnel but no
routes found in Notes` (the last row below), and `waiting for approval`, which says that
admission mode `approve` holds the routes of the guest back until an admin approves it;
see [Security](security.md).

| Message | What went wrong |
|---|---|
| `invalid hostname "wiki": needs at least two labels` | The word before the arrow is not a hostname. The reason follows the name: a label with an invalid character, a hyphen at the edge, a name that is too long. |
| `expected '->' after hostnames` | There is no arrow: after at least one hostname, a word with a colon or an equals sign stands where the arrow belongs, or the entry ends after the hostnames. The position is that word, or the last hostname. The arrow has to be a word of its own: `a.example.com->:80` as the first word of an entry is an `invalid hostname`, with the label `com->:80` named as containing `>`, and after another hostname (`b.example.com a.example.com->:80`) it is this message. |
| `expected a hostname before '->'` | The entry starts with an arrow. |
| `invalid target "8080": expected [http\|https://][ipv4]:port` | The target has no colon, a port out of range or with leading zeros, an address that is not IPv4, or an unknown scheme. It points at the target. |
| `invalid target ":80/wiki": paths are not supported` | The target has a path. |
| `invalid target "127.0.0.1:80": address is not routable to a guest` | The address cannot belong to a guest. |
| `unknown option "tls-verify"` | The word after the target is not an option, and does not start a new entry: on a continuation line, or because it is not a hostname. Options written wrongly end up here too, and so does an arrow stuck to a hostname after the target (`:81 a.example.com->:80`). |
| `option "no-tls-verify" given twice` | An option repeated. |
| `no-tls-verify only applies to https targets`, `sni= only applies to https targets` | The option needs `https://`. |
| `invalid value for host-header=`, `... sni=`, `... via=` | The value has characters that are not allowed, is empty, or is not a hostname, a NIC name or an address. |
| `via= cannot be combined with an address in the target` | An address is already named. |
| `hostname "x" is listed twice` | The name appears earlier in the same Notes, in any entry, valid or not. |
| `inconsistent indentation: mix of tabs and spaces` | A line is indented with a tab where the entry is indented with spaces, or the other way round. |
| `code fences are not allowed inside a cf-tunnel block` | The block holds a shorter run of backticks, which only quoted or nested markup has. The whole block is dropped. |
| `the closing fence is hidden by a comment; put the comment on its own line` | The fence that closes the block stands after a `#`. The whole block is dropped. |
| `code fences are not allowed on a cf-tunnel: line` | A backtick fence on a one-line route. |
| `a cf-tunnel: line cannot continue on the next line; use a fenced block` | The line after a one-line route is indented and starts with an arrow or an option. The entry is dropped, so that it is not published without its options. |
| `expected a route after cf-tunnel:` | The line has the prefix and nothing else. |
| `tagged cf-tunnel but no routes found in Notes` | The guest has the tag, and the Notes hold no route and no error. The name is the gate tag in the settings. |

After a syntax error the parser drops that entry and skips ahead to the next line that
does not continue it. The hostnames in the skipped text stay reserved, because they may
well belong to the broken entry. So a typo in one route never makes the next one
disappear, and never lets a hostname change hands.

## What there is not

Routes come from Notes only. In this release there is no command, and no documented
format, for routes that an admin makes by hand, and so no supported way to publish an
address that is not a guest's own; the daemon does read route files from
`/etc/pve/pco/routes/`, which [Operations](operations.md) and [Security](security.md)
describe. Cloudflare Access policies, IPv6 origins, and protocols other than HTTP and
HTTPS are not supported either.
