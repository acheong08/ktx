# AGENTS.md

Working notes for agents on **ktx**, Melzi's company context layer ("the brain"). README.md is the user-facing reference (formats, API, config, deployment). This file is what you need to work on the code and the box without the history.

## What it is

One Go service, **brain-server**, owning a clone of the data repo `git@github.com:melzi-cloud/company-brain.git`. Everything it stores is a file in that repo, committed and pushed; git is the source of truth. Private state (users, sessions, tokens, encrypted connector credentials, sync runs) is in SQLite.

```
transcripts/<type>/<YYYY-MM-DD>-<HHMM>-<slug>-<id>.md   Markdown + YAML front matter; folder = type ("unsorted" = none)
calendar/<owner>/<calendar>/calendar.json             name, colour, description
calendar/<owner>/<calendar>/<uid>.ics                 one event (series + overrides + VTIMEZONEs)
notes/<YYYY-MM-DD>-<HHMM>-<slug>-<id>.md              scratchpad notes: title, created, author (+ edited_*)
.ktx/deleted                                          ids deleted in the UI, so syncs don't bring them back
```

Ways in:
- **API:** `POST /v1/transcripts` (Bearer API token). There is no other upload endpoint; meetings are transcripts of a type.
- **Web UI** (htmx 4 + templ), sign-in through Pocket ID (OIDC). Pages: Transcripts (list, search, bulk type edit, detail, edit, new, delete), Notes, and Settings → Connections / Calendar / Tokens / AI.
- **Connectors** (inside the service, per-user credentials): Granola (REST API, Business key) and Pocket. Each has a "Type for …" setting for where *new* documents go; documents already stored stay where they are.
- **Notes** (web UI scratchpad): saved to `pending_notes` in SQLite, then a worker titles them (Neuralwatt via fantasy, workspace key set by admins in Settings → AI; fallback first line) and commits. Edits/deletes are synchronous.
- **CalDAV** at `/dav/` (Apple Calendar, Thunderbird), Basic auth with a calendar-scoped token.
- **MCP** at `/mcp` (read-only tools over transcripts, notes and calendars; every call needs an `intent`, logged as `mcp call`). ktx is its own OAuth server (DCR, PKCE, rotating refresh tokens); consent at `/oauth/authorize` uses the normal Pocket ID sign-in. Only when OIDC is configured.

Key behaviours:
- Document id = sha256(source+source_id)[:12], else hash of content. A file is found by its `-<id>.md` suffix anywhere under `transcripts/`, so moving between type folders keeps identity.
- Every write is one commit (fetch + rebase → write → commit → push, 3 retries; unpushed commits retried by a 5-minute sync). Connector runs and bulk moves are one commit each; the bookmark is saved only after the commit.
- A transcript edited in the UI gets `edited_at`/`edited_by`; connector syncs then skip it.
- **Calendar linking:** every transcript write, every calendar change, and server start re-match transcripts to events and write `calendar_event:` plus `participants` entries with `from: calendar`. Order: Granola `calendar_event_id`; else events within 15 min of the start or that the call started during; ranked by has-guests, then participant overlap (email among guests or full name in title), then nearest start, then title similarity; a full tie is no match. `match: manual|none` is set by people and never recomputed. Calendar participants are hidden from the edit form and re-added on save.
- Everyone sees every calendar; only the owner writes (others get read-only over CalDAV as `<owner>~<id>`). The first CalDAV sign-in or Settings → Calendar visit creates `<user>/personal`.

## Code map

```
cmd/brain-server/        wiring: config → gitrepo → store (+calendar linker) → state → secrets → connectors → auth → server/dav/web
internal/config          env vars (BRAIN_*); see README "Configuration"
internal/gitrepo         git CLI under one lock: Update(fn) / View(fn) / Sync
internal/transcript      Parse/ParseStored/Render; Doc keeps the YAML node (order, unknown keys); NormalizeType; Participant.From; Meta.CalendarEvent
internal/store           Put/PutBatch(Item{Doc, DefaultType, Sync})/Edit/EditLinks/SetType/SetTypes/Delete/List/Get/Relink; Linker interface
internal/calendar        git-backed calendars, .ics import (upsert, never delete), RRULE expansion, time zones (incl. Windows names), Match/Linker, Service
internal/dav             CalDAV on go-webdav; adds MKCALENDAR, PROPPATCH (name/colour), getctag + supported-report-set rewriting of PROPFIND
internal/state           SQLite with PRAGMA user_version migrations: users, sessions, api_tokens(scope api|caldav), oauth_clients, oauth_grants, settings, pending_notes, connections, runs
internal/secrets         AES-256-GCM sealing, master key from systemd credential ktx-master-key
internal/connectors      Manager (workers, runs, sealing) + granola/ + pocket/
internal/server          POST /v1/transcripts, GET /healthz
internal/ai              Settings (sealed Neuralwatt key + model in `settings`), Title() via charm.land/fantasy openaicompat (reasoning_effort none), CleanTitle/FallbackTitle
internal/notes           notes/ in git + pending queue worker (Submit/List/Get/Edit/Delete)
internal/oauth           OAuth 2.1 authorization server for MCP clients: metadata, /oauth/register, /oauth/token, codes (memory); consent page lives in web/oauth.go
internal/mcpserver       /mcp on modelcontextprotocol/go-sdk (stateless, JSON responses, localhost protection off: Caddy is local); tools.go
internal/web             handlers + *.templ (generated *_templ.go are committed); static/{app.js,style.css,htmx.min.js}
caddy/                   separate Go module: Caddy + cloudflare DNS + custom internal_dns app (A records for *.internal.melzi.dev)
deploy/                  install.sh (idempotent box setup), systemd units, Caddyfiles, Pocket ID quadlet, updater, SSH guard
```

## Conventions

- Go (module `github.com/acheong08/ktx`), stdlib-first, CGO_ENABLED=0 (modernc sqlite). Keep it simple; one service, no extra infra.
- After editing any `.templ`: `go tool templ generate -path internal/web`, and commit the generated files. CI fails otherwise (this has bitten twice: edit templ → regenerate → then build/deploy).
- Strict CSP (`script-src 'self'`): no inline JS, no `hx-on`, no htmx trigger filters (they need eval). Behaviour goes in `static/app.js` keyed on `data-*` attributes.
- Pages must work at phone width (390 px): `.list` tables become labelled cards via `data-label` on every `<td>`; check nothing scrolls sideways.
- Form values must not contain line breaks (browsers submit them as CR LF); encode structured values with `url.Values`.
- go-webdav is pinned to a master commit (needs `caldav.Calendar.ReadOnly`). Its server lacks PROPPATCH/MKCALENDAR/getctag/sync-collection (we add the first three; clients fall back to ETags). Don't wrap request bodies in `http.MaxBytesReader` (breaks its empty-body check); use `io.LimitReader`.
- Tests use real git against a temp bare repo and a fake OIDC provider (`internal/web/auth_test.go`). Add tests with changes; run `go vet ./... && go test -race ./...` (and `cd caddy && go test ./...` if touched).
- Docs: README describes behaviour for users; keep it in step with changes.

## Build, deploy, verify

Local helper keys live in `development/` (gitignored, never commit or print them): `agent_ed25519` (SSH to the box as root, and push access to this repo), `testing_ed25519` (deploy key for company-brain), `known_hosts`.

```sh
SSH="ssh -i development/agent_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=development/known_hosts"
# push
GIT_SSH_COMMAND="$SSH" git push origin main
# build + hot deploy a dev binary to the box
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /tmp/ktx-dist/brain-server ./cmd/brain-server
scp -i development/agent_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=development/known_hosts /tmp/ktx-dist/brain-server root@100.64.0.14:/root/brain-server.new
$SSH root@100.64.0.14 'install -m 755 /root/brain-server.new "$(readlink /opt/ktx/current)/brain-server" && rm /root/brain-server.new && systemctl restart brain-server && systemctl is-active brain-server'
```

CI: workflow `ci` on push (templ freshness, vet, tests, both modules). Check runs via `https://api.github.com/repos/acheong08/ktx/actions/runs`, matching `head_sha`. Releases (tag `v*`) build `ktx_<tag>_linux_<arch>.tar.gz` + SHA256SUMS; the box's `ktx-update.timer` installs new releases with health check and rollback. The box currently runs a hand-deployed binary in `/opt/ktx/releases/bootstrap`; cutting a release moves it onto CI builds.

UI checks without a passkey: write a throwaway test (not committed) that calls `setupAuth`, seeds data, prints the URL and sleeps; open it in a browser at 390 px and compare `documentElement.scrollWidth`.

## The box

- AlmaLinux 10 aarch64 (Hetzner), hostname melzi-i1, tailnet 100.64.0.14, reachable only over Tailscale. SELinux permissive.
- `https://brain.internal.melzi.dev` → Caddy → 127.0.0.1:8080. `https://id.internal.melzi.dev` → Pocket ID (Podman quadlet, no network, Unix socket, auto-update). Wildcard cert via Cloudflare DNS; `internal_dns` publishes records for sites in `/etc/caddy/Caddyfile.d/`.
- Paths: working copy `/var/lib/ktx/company-brain`, state `/var/lib/ktx/state.db` (run sqlite as `sudo -u ktx`), env `/etc/ktx/brain-server.env`, logs `journalctl -u brain-server`.
- Services are sandboxed systemd units (brain-server runs as `ktx`). SSH guard opens SSH only if Tailscale is down; dnf-automatic applies updates and reboots when needed; Pocket ID DB backed up daily to `/var/backups/pocket-id`.
- Pocket ID: groups `melzi` (may use ktx) and `ktx-admins`. Invite people with `pocket-id-provision invite <user> <email> [--admin]` on the box.
- Production tests: create a temporary user/token directly in state.db, test, then delete it and anything it created (a test calendar needs a removal commit in company-brain). Never create tokens on real users' accounts.

## People and context

- Antonio (technical founder, the usual requester; Linux/NixOS + fish, Thunderbird, iPhone) and Maxim (commercial; uses the web UI, not files). Both have ktx accounts.
- Maxim's Granola Business account (maxim@melzi.dev) syncs through the connection, type `customer_discovery`; his Pocket syncs to `internal`. Calls recorded on his old personal Granola account don't sync and get pasted in by hand (this happened for Robin, Swarnim, Johann on Nov 16).
- A separate **outreach** service (`../outreach`, runs on the Mac mini; don't restart it without its handover) logs calls/emails/DMs to Folk and keeps its own call library in `~/.outreach/library`. Its Granola ids are web UUIDs; ktx's connector uses `not_…` ids, so the same call can exist under both: don't bulk re-import the library.
- Folk is the CRM; the link from a call to a Folk person is the participant email (from the calendar event), not stored Folk ids.

## Preferences

- Simple designs; git as source of truth; Go, htmx, templ, Podman; tailnet-only access; no secrets in config where avoidable.
- Ask before big design choices; give options briefly. Answer concisely; report what was verified and what wasn't.
- Don't change real data or settings beyond what was asked; say when you did.

## Open items

- Cut a release so the box runs CI builds.
- SELinux to enforcing (check `ausearch -m avc` first).
- Antonio will remove the agent's root SSH key from the box.
- Calendar: invitations aren't sent; no sync-collection; a name-only participant plus the same person from the calendar (with email) shows twice.
- Johann's pasted transcript says 2:00 PM; Granola says 12:00 PM (unconfirmed which is right).
- fantasy keys provider options by provider name: don't `WithName` the openaicompat provider, or `reasoning_effort` is silently dropped.
- MCP is read-only for now; write tools (uploads, types, event links, events) are a possible next step.
- Later: bookings (Cal.com) through the calendar; possibly move outreach's Granola→Folk logging into ktx.
