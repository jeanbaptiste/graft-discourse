# graft-discourse

A standalone Go daemon that bridges a **Discourse** forum and a
**Graft**-mirrored repository (Forgejo/Radicle), in both directions, using
Graft's public [ActivityPub](https://www.w3.org/TR/activitypub/) surface.

```
 Discourse post  ──signed Create{Note}──▶ Graft inbox ──▶ Forgejo/Radicle comment
 repo comment    ◀──Graft outbox notes──── Graft outbox
```

## How it maps

- A Discourse **topic** is bound to a Graft-mirrored repo **issue** through
  an explicit, admin-controlled `mappings` entry (`topic_id` + `note_uri`).
  Title matching exists but is **off by default** (`allow_title_matching`),
  because it would let anyone who can name a topic impersonate an issue.
- A Discourse **post** in a mapped topic is delivered to the Graft series
  inbox as a signed `Create{Note}` whose `inReplyTo` is the issue's Graft
  note URI. Graft then creates a real comment on the Forgejo **and** Radicle
  issue.
- Graft **comment** notes appearing in the series outbox are mirrored back as
  Discourse posts.
- The topic **opening post** (`post_number == 1`) is not forwarded: it is the
  issue body itself, and Graft mirrors issues create-only.

## Compatibility with Graft

The protocol layer (`internal/ap`) is written to be byte-compatible with
Graft's `internal/activitypub`:

- draft-cavage HTTP Signatures over `(request-target) host date digest`,
  `rsa-sha256` — the exact header set Graft signs and verifies.
- A `Service` actor with a `#main-key` public key, so Graft can fetch it and
  verify deliveries.
- Replies only count for `issue`/`patch` notes; replies to commit notes are
  dropped by Graft, so the bridge doesn't send them.

## Security

This is **v2.1**, following a full audit of v1 and a second independent
pass over v2. Every finding and its fix is tracked in
[SECURITY.md](SECURITY.md); the highlights:

- **Explicit, admin-controlled topic↔issue bindings** — no title-based
  impersonation by default.
- **SSRF-hardened HTTP client** (`internal/netguard`): public-address-only
  dialing with DNS-rebinding protection, validated redirect hops, and inbox
  URL validation before any signed delivery.
- **HTTPS enforced unconditionally** for `public_base_url` and
  `graft.base_url`; only `discourse.base_url` may drop to plaintext, via
  `allow_insecure_http`, scoped so it can't relax the Graft-facing or
  public endpoints. API credentials are stripped on cross-host redirects.
- **`0600` secrets**: a group/world-readable `state.json` (holds the actor
  private key) or `config.json` (holds the API key) is refused at startup.
- **Timeouts everywhere**: actor server read/write/idle limits, a per-pass
  deadline, and panic recovery in the loop.
- **Rate/size limits**: `max_content_runes`, `max_deliveries_per_pass`, and
  an optional `allowed_categories` allowlist.

## Why an actor server

Graft verifies every inbound request by fetching the sender's actor over the
network and rejecting private/loopback URLs. The bridge therefore serves its
own actor document and **must be reachable over public HTTPS**
(`public_base_url`). A plaintext `http://` or a `localhost` URL will be
rejected by Graft's SSRF guard.

## Build & run

```sh
go build -o graft-bridge ./cmd/bridge
cp config.example.json config.json
chmod 600 config.json          # required: it holds the Discourse API key
# edit config.json: public_base_url, mappings, graft.series, discourse.*
./graft-bridge -config config.json
./graft-bridge -config config.json -once   # a single pass, then exit
```

The state file (JSON) holds the actor keypair, the topic↔issue mapping and
the dedup sets, and is written `0600`. Back it up; losing the keypair
invalidates verification, and losing the dedup sets can re-deliver.

## Admin API

Binding a topic to an issue no longer requires editing config and
restarting. Set `admin_token` in config (or, better, the
`GRAFT_BRIDGE_ADMIN_TOKEN` environment variable, which overrides it). The
API is **disabled entirely when the token is empty**.

```sh
TOKEN=...
# discover replyable issues/patches to bind (optional ?series= and ?q= filters)
curl -H "Authorization: Bearer $TOKEN" \
  'https://bridge.example.org/admin/issues?series=federation-x&q=flux'
# list current bindings
curl -H "Authorization: Bearer $TOKEN" https://bridge.example.org/admin/mappings
# bind topic 100 to issue note
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"topic_id":100,"note_uri":"https://f1.cyberwild.org/actors/federation-x/notes/7"}' \
  https://bridge.example.org/admin/mappings
# unbind
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://bridge.example.org/admin/mappings/100
```

`GET /admin/issues` returns `{series, entry_id, kind, title, note_uri, url}`
for every replyable note in the configured series' outboxes, so you can copy
the `note_uri` straight into a binding.

`Bind` refuses a note URI that isn't on the configured Graft host, isn't in a
configured series, or is a commit note (Graft only accepts replies to
issues/patches). Requests are bearer-authenticated in constant time and
body-limited.

## Tests

```sh
go test ./...
```

The AP signature round-trip, note parsing/matching, and both sync directions
(including echo suppression and idempotency) are covered with fakes — no live
Graft or Discourse needed.

## Limitations (v1)

- **Manual mapping.** Topics are bound to issues via the `mappings` config.
  There is no discovery service; a new issue needs a new mapping entry. The
  opt-in title heuristic is a stopgap, not a substitute.
- **Create-only.** Graft does not propagate edits or deletes, in either
  direction, so neither does the bridge.
- **Flattened comments.** Graft stores comments as text (HTML stripped); the
  bridge mirrors them as plain posts, without threading.
- **ATProto is not wired.** Graft's Bluesky client only reads replies to its
  own posts and is inert without credentials, so only the ActivityPub path is
  used here.
- **Echo handling is heuristic.** Fediverse-origin comments (including our
  own, which Graft re-publishes) are recognized by the `via Fediverse` marker
  and truncated-content hashes and dropped from reverse sync.
- Graft itself is young; its note format is not frozen.

## Layout

```
cmd/bridge        wiring, actor HTTP server, poll loop
internal/ap       ActivityPub actor, signatures, notes (Graft-compatible)
internal/admin    authenticated topic<->issue binding API
internal/netguard SSRF-safe HTTP clients (public-only dialing, redirect hygiene)
internal/graft    Graft REST/AP client + outbox note parsing
internal/discourse Discourse REST client
internal/state    JSON bookkeeping (keys, mapping, dedup), 0600
internal/bridge   the two-direction reconciliation
```
