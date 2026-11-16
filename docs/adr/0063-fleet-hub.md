# 0063. A fleet hub: one console for several boxes, members dial out

Date: 2026-11-15

Extended by [ADR-0064](0064-fleet-administration.md) (administration).

## Context

A box serves one cluster. An organisation with three clusters has three
consoles, three sign-ins and three addresses for one question ("why was
this build slow"), and one pipeline whose jobs ran in two clusters is
split across two consoles. The clusters are often behind NAT: nothing
outside may reach a controller's API.

## Decision

- A fleet is a hub and members. The hub is a release of the control-plane
  chart with `fleet.role: hub`: one process (role `hub` of the same binary
  and image) with the pages, sign-in, the members' connections and the
  management API, and SQLite for its sessions and its members. It runs no
  daemons, controller, web or routers.
- A member is a box with `fleet.role: member`. Its controller keeps one
  outgoing WebSocket to the hub's console address (through the user's
  Ingress); over it runs one TLS 1.3 session in which each side proves a
  key the other has pinned (Ed25519 or ECDSA, no CA), and inside it a
  multiplexer (hashicorp/yamux): the hub opens a stream per request and
  the member answers with its controller's own API, unchanged. TLS per
  stream is not used: net/http's read deadlines turn yamux's timeouts
  into permanent TLS errors and every kept-alive request after the first
  fails (spike, 2026-11-15).
- Build data stays in each member; the hub serves each member's console
  pages under `/m/<name>/` from that member's controller. The pages write
  their addresses from the root and move them under the base when they
  render; the pipeline and job addresses the runners print, which name no
  cluster, are found at the hub by asking every member.
- A member's own web is not installed: the hub is its console. Members
  join by name and public key through the hub's management API
  (`/api/v1/fleet/`, marked alpha: it may change between releases); the
  hub's owners (`signIn.gitlab[].owners`) and its bootstrap token manage
  it. A removed member's record is kept for a while (its name can be
  registered again at once); a member that comes back with another store
  is accepted and flagged.
- The first stages are experimental: values, the API and the link's
  protocol may change without the deprecation cycle of ADR-0027. The hub
  accepts its link protocol and the one before it (from protocol 1 on);
  a member newer than its hub is refused until the hub is upgraded.

## Consequences

- No inbound address at a member; one address and one certificate at
  the hub, for people and members alike (proven through Traefik and the
  NGINX Ingress Controller, members behind NAT, sessions idle for 30
  minutes, restarts of each side).
- A member that is down shows as not connected; its builds are not lost
  (they stay in the member) but not shown until it reconnects.
- GitLab is read by each member with its own token; reading it at the
  hub with each person's token comes in a later stage. Registration from
  the console with a join token, settings and secrets for new clusters,
  roles in the hub's store and backups came with ADR-0064.
