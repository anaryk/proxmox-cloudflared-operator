# Monitor pco

This guide sets up the checks that tell you when pco needs you: `pco doctor` from a timer, the
problem lines and fields of `pco status --json`, the events and the journal, and the metrics of
the connectors.

## When you need it

pco publishes what the Notes ask for and holds back when it cannot be sure, and most of what goes
wrong it reports instead of fixing: a token that expires, a record of someone else in the way, a
connector it does not run, a filter that was flushed. Nobody reads those reports unless something
asks for them.

## What to watch

| Signal | Where | Alert when |
|---|---|---|
| The checks of the installation | `pco doctor` | It exits 1: a check failed. Warnings exit 0. |
| The problems of the last cycle | `pco status --json`, `.problems` | The list is not empty. `pco status` exits 1 then. |
| Whether the cycle held | `.complete`, `.writerVerdict`, `.hold` | `complete` is `false`, `writerVerdict` is not `ok`. |
| The egress filter | `.egress.state` | It is not `on`. |
| Connectors pco does not run | `.rogueConnectors` | The list is not empty. |
| Routes that are not served | `.routes[].state` | A route you publish is not `active`. |
| What changed | `pco events`, the journal | An event of the level `warn` or `error`. |
| The connectors themselves | `/metrics` of each connector on 127.0.0.1 | `cloudflared_tunnel_ha_connections` is 0. |

[Operations](../operations.md#exit-codes-and---json) has every field of the state; a script reads
the fields, as the messages are for people and can change. [Problems](../problems.md) has every
problem line, event kind and doctor check.

## Before you start

- You are root on the node: the daemon's socket answers root, and the web interface's user.
- `jq` for the JSON (`apt install jq`).

## Steps

1. Run `pco doctor` from a timer, and have systemd start a unit of yours when it fails.
   Write the service:

   ```ini
   # /etc/systemd/system/pco-doctor.service
   [Unit]
   Description=Check pco
   OnFailure=pco-doctor-alert.service

   [Service]
   Type=oneshot
   ExecStart=/usr/bin/pco doctor
   ```

   the timer:

   ```ini
   # /etc/systemd/system/pco-doctor.timer
   [Unit]
   Description=Check pco every five minutes

   [Timer]
   OnCalendar=*:0/5
   Persistent=true

   [Install]
   WantedBy=timers.target
   ```

   and the unit that tells you, here a mail to root through the mail system of the node, which
   Proxmox VE forwards to the address given at its install:

   ```ini
   # /etc/systemd/system/pco-doctor-alert.service
   [Unit]
   Description=Mail root what pco doctor found

   [Service]
   Type=oneshot
   ExecStart=/bin/sh -c '{ echo "Subject: pco doctor on %H"; echo; /usr/bin/pco doctor; } | /usr/sbin/sendmail root'
   ```

   Then:

   ```sh
   systemctl daemon-reload
   systemctl enable --now pco-doctor.timer
   ```

   `journalctl -u pco-doctor.service` has the output of every run. When the daemon does not
   answer, `pco doctor` run as root still checks the units of pco, the store, `cloudflared` and
   the egress table, and fails when `pco.service` does not run, so the timer reports a daemon
   that is down too.

2. Read the problem lines and the fields that say whether the daemon is in order:

   ```sh
   pco status --json | jq '{mode, complete, writerVerdict, hold, egress, problems}'
   ```

   ```text
   {
     "mode": "enforce",
     "complete": true,
     "writerVerdict": "ok",
     "hold": null,
     "egress": {
       "state": "on"
     },
     "problems": []
   }
   ```

   The routes that are not served, with their reasons:

   ```sh
   pco status --json | jq -r '.routes[] | select(.state != "active") | "\(.hostname) \(.owner) \(.state) \(.reason // "")"'
   ```

   `pco status > /dev/null` alone exits 1 when there are problems, when the inventory is
   incomplete, the writer is not in order or the egress filter does not confine the
   connectors, and 2 when the daemon could not be asked.

3. Follow the events. The daemon keeps the last thousand since it started; ask for those of a
   period, and keep the warnings and errors:

   ```sh
   pco events --since 5m --json | jq -r '.[] | select(.level != "info") | "\(.at) \(.level) \(.kind) \(.subject) \(.message)"'
   ```

4. Or follow the journal, which has every event as well, as JSON lines with the field `event`:

   ```sh
   journalctl -u pco -o cat --since today | jq -cR 'fromjson? | select(.event != null and .level != "info")'
   ```

   ```text
   {"level":"warn","event":"route","seq":2,"subject":"app.example.com","route":"app.example.com","guest":"lxc/120","time":"2026-10-01T14:05:00+02:00","message":"lxc/120: withdrawn (guest is not running)"}
   ```

   `-o cat` prints the lines as the daemon wrote them, and `fromjson?` passes over those of
   systemd. A log shipper on the node can send the same lines on. The connectors log under their
   own units, `journalctl -u 'pco-cloudflared@*'`.

5. Scrape the metrics of the connectors. Each connector serves Prometheus metrics on
   127.0.0.1, on a port from 20300 up that pco gives it, and only there: scrape it from the node
   itself, with an agent that runs there. The addresses are in the state:

   ```sh
   pco status --json | jq -r '.connectors[].metricsAddr'
   ```

   ```text
   127.0.0.1:20300
   ```

   ```sh
   curl -s http://127.0.0.1:20300/metrics | grep -E '^cloudflared_tunnel_(ha_connections|total_requests|request_errors) '
   ```

   ```text
   cloudflared_tunnel_ha_connections 4
   cloudflared_tunnel_request_errors 0
   cloudflared_tunnel_total_requests 0
   ```

   `cloudflared_tunnel_ha_connections` is the number of connections to Cloudflare's edge, 0 when
   the connector is cut off; `cloudflared_tunnel_request_errors` counts the requests it could not
   take to the origin, the 502s of the visitors. `/ready` on the same address answers what pco
   itself reads, `{"status":200,"readyConnections":4,"connectorId":"..."}`. A port can change, as
   when another process held it or a tunnel is added, so make the list of targets from the state
   rather than by hand; for a Prometheus on the node, a file of targets:

   ```sh
   pco status --json | jq '[{targets: [.connectors[].metricsAddr]}]' > /etc/prometheus/pco-connectors.json
   ```

   The egress filter does not stand in the way: it lets a connector answer connections to the
   addresses of the node.

## Check

Fail a check on purpose and see the alert come: stop the daemon at a quiet moment, run the
check by hand, and start the daemon again:

```sh
systemctl stop pco
systemctl start pco-doctor.service
systemctl start pco
```

The connectors keep serving while the daemon is stopped. `systemctl list-timers pco-doctor.timer`
shows when the timer runs next.

## Undo

```sh
systemctl disable --now pco-doctor.timer
rm /etc/systemd/system/pco-doctor.service /etc/systemd/system/pco-doctor.timer /etc/systemd/system/pco-doctor-alert.service
systemctl daemon-reload
```

## Read on

- [Operations](../operations.md#events-and-logs): events, logs and the log level.
- [Troubleshooting](../troubleshooting.md): reading `pco status`, `pco doctor` and
  `pco diagnose` when an alert comes.
- Reference: [pco doctor](../cli/pco-doctor.md), [pco status](../cli/pco-status.md),
  [pco events](../cli/pco-events.md).
