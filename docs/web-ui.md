# Web interface

pco web serves the web interface over HTTPS, on the node or in the appliance. These are its
pages as an admin sees them, with an example install: one served zone and a route in every
state, a connector pco does not run, a zone that froze, guests that wait for approval and
changes that wait for a confirmation. `make docs-screenshots` takes them again.

## Overview

![The overview: the routes by state, the requests at the edge over the last 15 minutes, the connectors, what needs you, and the two problems of the last cycle](images/ui-overview-light.png)

![The flow map: the hostnames of each zone, the edge of each tunnel, the connector on the node, the bridges and the guests, with dots on the lines that carry a measured figure](images/ui-overview-map-light.png)

![The flow map focused on www.example.com: its chain from the zone through the edge, the connector and the bridge to port 8080 of guest web-1, and the events of that hostname below it](images/ui-overview-focus-light.png)

## Routes

![The routes: a chip for each state with its count, a filter by zone and by text, and a row for each hostname with its state, owner, service, level and zone](images/ui-routes-light.png)

![The route of www.example.com in a drawer over the routes: its state, owner, zone, service, planned rule, tunnel, path and candidates](images/ui-route-light.png)

![The form of a new manual route: its id and hostname, a guest or an address within the allowed prefixes as its target, the scheme and port, and a Host header](images/ui-route-manual-light.png)

## Plan

![The plan: a deletion of a DNS record held for the grace period, and four changes that wait for a confirmation](images/ui-plan-light.png)

![The confirmation of what waits: the removals, the zone and the tunnel that are gone and the vanished guests, with confirm typed and the button that accepts them at the next run](images/ui-plan-confirm-light.png)

## Guests

![The guests: two waiting for approval and why, and the approved ones with the identity each was approved in](images/ui-guests-light.png)

![The same page as a reader sees it: only the guests the reader may audit, nothing waiting, and Revoke disabled with the privilege it needs](images/ui-guests-reader-light.png)

![The guest web-1: its node, identity and approval, and the routes it asks for with their state](images/ui-guest-light.png)

![The claims: who holds each hostname, in which state, since when, and who waits for it](images/ui-claims-light.png)

## Networks

![The networks: the bridges and VLANs the routes were proven on, with their routes, levels and guests, and the segments of routes at observed](images/ui-networks-light.png)

## Credentials

![The credentials: one usable, with its token, last check, accounts and the zone it leaves out, and one never checked](images/ui-credentials-light.png)

![The credential a1b2c3d4: usable, its write access proven, and its last check: the token, its account and zones, and the checks it passed](images/ui-credential-light.png)

## Zones

![The zones: example.com served, example.net frozen because the credential no longer lists it, and example.org left out](images/ui-zones-light.png)

## Tunnels

![The tunnels: one in each account, whether it is verified, its rollout and connector and how many connectors pco does not run, and a tunnel no credential sees](images/ui-tunnels-light.png)

## Events

![The events of the daemon, newest first, with the filters above them: level, kind, route, guest, account, text and time](images/ui-events-light.png)

![The events filtered to warnings](images/ui-events-filtered-light.png)

## Doctor

![The doctor after a run: the counts of its checks, and the failures first, each with what to do](images/ui-doctor-failing-light.png)

## Settings

![The settings: publishing, which guests are published, and the hostname policy, each choice with what it does](images/ui-settings-light.png)

## First-run setup

![The setup of a node whose install did not finish: the install check gives the command for root on the node, and the steps that follow](images/ui-setup-install-light.png)

![Step 1 of the setup: the permissions the API token needs, the buttons that open the form in the Cloudflare dashboard, and the fields for the token](images/ui-setup-token-light.png)

![Step 2 of the setup: the zones the token lists, served or left out, and why one is left out](images/ui-setup-zones-light.png)

![Step 4 of the setup: a name, zone, scheme and port, and the block for the Notes of the guest that they make](images/ui-setup-route-light.png)

![Step 5 of the setup: the first route is there, what the next cycle would change at Cloudflare, and the button that starts publishing](images/ui-setup-publish-light.png)

## Sign-in

![The sign-in on a node: with the session of Proxmox VE in the same browser, or with an API token](images/ui-signin-light.png)

![The sign-in of the appliance: the user, realm and password of Proxmox VE, or an API token](images/ui-signin-appliance-light.png)
