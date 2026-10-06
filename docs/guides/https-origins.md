# Publish an HTTPS origin

This guide publishes a service that speaks TLS on its port, and chooses how the connector
checks its certificate. The VM in the examples is `qemu/103`, named `vault`, at `10.0.0.13`.

## When you need it

The service answers only in HTTPS, or redirects every plain request to HTTPS: a web interface
that comes with a certificate of its own, a service behind a reverse proxy that ends TLS, an
appliance with a self-signed certificate.

A visitor's TLS ends at Cloudflare's edge whatever the route says. The route decides the leg
from the connector on the node to the guest: plain HTTP for `http`, the default, and TLS for
`https`. With `https` the connector checks the certificate the service presents, and the
options below change what it checks.

## Before you start

- The guest is published as in [Publish a container](publish-a-container.md) or
  [Publish a virtual machine](publish-a-vm.md), or is ready to be.
- Know the certificate the service presents. From the node:

  ```sh
  openssl s_client -connect 10.0.0.13:8443 -servername vault.example.com </dev/null 2>/dev/null \
    | openssl x509 -noout -subject -issuer -ext subjectAltName
  ```

  The issuer says whether it is from a public authority or self-signed, and the names say
  which hostnames it is for.

## Steps

1. Choose the form of the route by the certificate:

   | The certificate | The route |
   |---|---|
   | From a public authority, for the public hostname | `vault.example.com -> https://:8443` |
   | From a public authority, for another name | `admin.example.com -> https://:443 sni=admin.internal.example.com` |
   | Self-signed, or from an authority of your own | `vault.example.com -> https://:8443 no-tls-verify` |

   By default the connector asks for the public hostname in the TLS handshake and verifies
   the certificate against the authorities its system trusts. `sni=` names another name to
   ask for and to verify. `no-tls-verify` checks nothing: the leg to the guest is encrypted,
   and the connector does not know whom it talks to beyond the address that pco proved. pco
   has no option for an authority of your own.

2. When the service chooses its site by the Host header and expects another name than the
   public one, add `host-header=`. It works with `http` and `https`, and it does not change
   the name asked for in the handshake:

   ````text
   ```cf-tunnel
   vault.example.com -> https://:8443 no-tls-verify host-header=vault.internal.example.com
   ```
   ````

3. Write the routes into the Notes of the guest, with the gate tag on it:

   ````text
   ```cf-tunnel
   vault.example.com -> https://:8443 no-tls-verify
   admin.example.com -> https://:443  sni=admin.internal.example.com
   ```
   ````

   `no-tls-verify` and `sni=` are refused on an `http` target, and `sni=` takes no wildcard;
   `pco status` lists such a mistake under `Issues`.

4. Ask for a cycle:

   ```sh
   pco sync
   pco routes
   ```

   ```text
   HOSTNAME           STATE   LEVEL  SERVICE                 OWNER     ZONE         NOTE
   admin.example.com  active  port   https://10.0.0.13:443   qemu/103  example.com  -
   vault.example.com  active  port   https://10.0.0.13:8443  qemu/103  example.com  -
   ```

## Check

The last step of `pco diagnose` makes the request the connector would make, with the Host
header and the TLS settings of the route:

```sh
pco diagnose vault.example.com
```

```text
✓ route      qemu/103 (vault) holds it; state active
✓ zone       zone example.com is active
✓ dns        its record points at the tunnel
✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to https://10.0.0.13:8443 (configuration version 4)
✓ connector  active, ready, 4 connections
✓ identity   10.0.0.13 is the address of qemu/103, verified at identity level port (static)
✓ tcp        10.0.0.13:8443 answers
✓ http       the origin answered 200 OK
```

When the route and the service do not agree, the `http` step says what to change, and
visitors get a 502:

| The `http` step says | What to change |
|---|---|
| `origin speaks TLS: use https:// in the route` | The route says `http`, and the port speaks TLS. |
| `origin speaks plain HTTP: use http:// in the route` | The route says `https`, and the port speaks plain HTTP. |
| `TLS verification failed (unknown authority): set sni=<name> in the route when the certificate is of another name, or no-tls-verify` | The certificate is self-signed or from an authority of your own: `no-tls-verify`. |
| `TLS verification failed (name mismatch): ...` | The certificate is for another name: `sni=` with that name. |
| `TLS verification failed (expired or not yet valid): ...` | Renew the certificate of the service, or use `no-tls-verify` until you have. |
| `the origin refused the TLS handshake (...)` | The service refuses the name asked for, or the TLS of the handshake. |

The request of `pco diagnose` comes from the daemon, not from the connector; the egress filter
confines only the connector. When every step passes and visitors still get a 502, see
[Work with the egress filter](egress-filter.md).

## Undo

Write the route back as it was, as `-> :8080` for plain HTTP, or take out the option, and
`pco sync`. The rule of the tunnel changes in the next cycle; the DNS record stays as it is.

## Read on

- [Annotations](../annotations.md#options): every option, and what is refused.
- [Security](../security.md#what-cloudflare-sees): what Cloudflare sees of the traffic.
- Reference: [pco diagnose](../cli/pco-diagnose.md).
