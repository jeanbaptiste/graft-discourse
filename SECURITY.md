# Security audit — v1 findings and v2 fixes

Scope: the `graft-discourse` bridge as first written (v1). Each item lists
the finding, its impact, and what v2 does about it.

## High

### H1 — Issue impersonation via title matching
The v1 bridge linked a Discourse topic to a repo issue purely by matching
titles. Anyone able to create or rename a topic to an issue's title could
have every post in that topic posted as a comment on the repository issue.
**Fix:** title matching is now **off by default**. Bindings come from an
admin-controlled `mappings` list (`internal/config`, `bridge.Options.Explicit`).
The heuristic survives only behind `allow_title_matching: true` and logs a
warning when it fires.

### H2 — SSRF via remote actor inbox
`graft.Client` POSTed signed activities to whatever `inbox` URL a fetched
actor advertised, using a stock `http.Client` (follows redirects, dials any
address). A hostile or compromised actor could point the bridge at internal
services.
**Fix:** new `internal/netguard` builds clients that (a) validate the URL
scheme/host, (b) refuse loopback/private/link-local/CGNAT and other
special-purpose ranges, (c) resolve the host and dial a validated public IP
directly, defeating DNS rebinding, and (d) validate every redirect hop. The
Graft client enables all of this and validates `actor.Inbox` before delivery.

### H3 — Credential leakage and cleartext transport
The Discourse API key travelled over whatever scheme `base_url` used, and a
redirect could replay the `Api-Key` to another host. The AP private key is
also sensitive to transport and file permissions.
**Fix:** `config.Load` requires `https` for `public_base_url`,
`graft.base_url` and `discourse.base_url` unless `allow_insecure_http` is set.
The hardened client strips `Api-Key`/`Api-Username` on any cross-host redirect
and caps redirect chains. The state file (which holds the private key) is
written `0600` and a group/world-readable state or config file is refused at
startup.

## Medium

### M1 — Actor HTTP server had no timeouts
`http.Server` without `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout` is
exposed to Slowloris-style connection exhaustion.
**Fix:** explicit timeouts and `MaxHeaderBytes` in `cmd/bridge`, plus
`GET`/`HEAD`-only method checks in the actor handlers and graceful shutdown.

### M2 — Inbound signature verification was too lax
`ap.VerifyRequest` skipped the digest check when the `Digest` header was
absent and never checked the declared algorithm. Though the running bridge
exposes no inbox, the exported function is a foot-gun.
**Fix:** it now requires a `Date` (replay window), a `Digest`, a `keyId`, and
if an `algorithm` is declared requires `rsa-sha256`, matching `rsa-sha256`
verification.

### M3 — Unbounded message size / unbounded work
A single huge forum post or fediverse reply could be mirrored at any size,
and a pass had no ceiling on how many comments it would deliver.
**Fix:** `max_content_runes` caps each mirrored message; `max_deliveries_per_pass`
rate-limits outbound comments; Discord response bodies are already read
through `io.LimitReader`.

### M4 — No access control on forwarding
Every post in a mapped topic was forwarded regardless of who wrote it or
where.
**Fix:** optional `allowed_categories` allowlist; with a list configured, an
unknown topic is denied.

### M5 — Weak outbound dedup
Forward dedup keyed only on a content hash, so two identical posts collapsed
and a crash after delivery could duplicate a comment.
**Fix:** forward dedup is keyed on the stable Discourse `post_id` per series
(`state.PostDelivered`); the content hash remains only as an echo guard for
reverse sync.

## Low / accepted

### L1 — Echo suppression is heuristic
Graft does not expose a comment's origin in its ActivityPub notes, so the
bridge still recognizes its own fediverse posts by the `via Fediverse` marker
and by truncated-content hashes. A crafted comment could in principle mimic
the marker to suppress its own mirroring into Discourse. Impact is limited to
that message; noted rather than fully solved.

### L2 — Outbox is capped
Graft's outbox returns a bounded window, so a burst larger than that window
between two polls can drop reverse-direction comments. Acceptable for v1/v2;
the durable fix is following the actor and consuming inbox deliveries, or an
explicit cursor.

### L3 — ATProto path unused
Graft's Bluesky client is inert without credentials and only reads replies to
its own posts, so no ATProto path is wired. Not a vulnerability.

## v2 addition — admin API threat model

The `/admin/mappings` API lets an operator bind a topic to an issue at
runtime. Its attack surface is deliberately small:

- **Disabled unless a token is configured.** With an empty token the routes
  are never registered, so it cannot be exposed by accident.
- **Bearer token compared in constant time** (`subtle.ConstantTimeCompare`).
- **Bodies capped** at 4 KiB and decoded with `DisallowUnknownFields`.
- **Bind is validated, not trusted**: the note URI must be on the configured
  Graft host, belong to a configured series, and resolve to an issue/patch
  note — so the API cannot be used to point the bridge at an arbitrary repo
  or actor.
- Token is best supplied via `GRAFT_BRIDGE_ADMIN_TOKEN` (env) so it is not
  persisted to disk; a `0600` config is enforced if it is.

Residual risk: the token is a static shared secret with no rotation or
scoping. Put the admin path behind your reverse proxy's access controls and
TLS as well.

## v2.1 — external audit findings

A second, independent read of the full v2 tree surfaced five more items,
none reachable in the current deployment shape but each worth closing on
its own terms:

### M6 — VerifyRequest trusted the sender's own declared header set
The signer's `headers` parameter (part of the attacker-controlled inbound
`Signature` header) was used as-is to decide which headers the signature
actually covers, with no floor. A signature validly covering only `date`
would still pass, even though it binds neither the method/path
(`(request-target)`) nor the body (`digest`) — a captured signed request
could in principle be replayed against a different endpoint. No live
impact today (`ActorServer` advertises `/inbox` but never registers it),
but the function is exported.
**Fix:** `VerifyRequest` now rejects any signature whose declared
`headers` doesn't include both `(request-target)` and `digest`,
regardless of what the sender claims to have signed
(`requiredSignedHeaders`, `internal/ap/sign.go`). Covered by
`TestVerifyRejectsUnderSignedHeaders`.

### M7 — one flag governed three different HTTP-scheme trust boundaries
`allow_insecure_http` relaxed `public_base_url`, `graft.base_url` and
`discourse.base_url` together, at both the config-validation layer and the
`netguard` client-policy layer. An operator enabling it for a legitimately
internal Discourse instance also silently permitted plaintext for the
Graft-facing and public actor endpoints — which should never be plaintext:
Graft's own SSRF guard rejects a non-HTTPS actor/inbox URL outright, so
relaxing those two only breaks interop or exposes the actor endpoint in
transit, never helps.
**Fix:** the flag now scopes to `discourse.base_url` only, in both
`config.Load`'s scheme check and `cmd/bridge`'s `graftPolicy`
(`AllowHTTP: false`, unconditionally).

### L4 — unbounded error bodies in client error strings
`graft.Client` and `discourse.Client` embedded a failed response's full
body (up to the 1 MiB read cap) directly into the returned error, unlike
`ap.PostSigned`'s own truncation. A large HTML error page from either
service could bloat a single log line.
**Fix:** both clients now truncate to 500 bytes the same way
`ap.PostSigned` already did.

### L5 — admin auth had no independent empty-token guard
`admin.Server.auth`'s constant-time comparison relied entirely on
`Register` never mounting the routes when `Token == ""`; `auth` itself
would have accepted an empty bearer token against an empty `Token` if ever
called directly.
**Fix:** `auth` now also checks `s.Token == ""` itself.

### L6 — dedup sets (Delivered/PostsDelivered/SeenNotes) grew forever
Every mutation rewrote the whole state file, and nothing ever removed an
entry — over a long-running bridge's lifetime, `state.json` and the cost
of every save both grow without bound.
**Fix:** each dedup set is now keyed by first-seen unix timestamp rather
than a bare bool, and `saveLocked` prunes anything older than
`dedupRetention` (180 days — far past any realistic in-flight window)
before every write.

### M8 — the bridge's own reverse-mirrors got forwarded back as new replies
`forwardDiscourseToGraft` forwards every post past `post_number` 1 in a
bound topic, with nothing checking who wrote it — including the bridge's
own bot account, which `reverseGraftToDiscourse` uses to post a mirror of
every native Forgejo/Radicle comment. Every native comment mirrored to
Discourse therefore got picked back up on the next pass and forwarded to
Graft as if it were a fresh human reply, producing a duplicate comment
each time. Graft's own outbound "via Fediverse" echo guard stops this
after one round (that duplicate's own reflection back into the outbox
does carry the marker), so it's a single spurious duplicate per native
comment, not an unbounded loop — still real noise on every mirrored
comment.
**Fix:** `Options.BotUsername` (set from `discourse.api_username`) is
checked in `forwardDiscourseToGraft` — a post authored by the bridge's
own account is always its own reverse-mirror and is now skipped
unconditionally. Covered by `TestForwardSkipsBotsOwnPost`.

## Not addressed

- Edits/deletes are not propagated (Graft mirrors create-only).
- No structured link field yet; `mappings` is manual by design.
- No tokenization/secret manager; the API key lives in a `0600` config file.
