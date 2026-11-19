# ktx

The company context layer. Today that is **brain-server**, plus the Caddy build and deploy tooling that run it.

## brain-server

A small Go server that owns a clone of the brain git repository.
- **Upload:** you send a transcript over HTTP. The server checks its metadata, writes it to `transcripts/<type>/` (or `transcripts/unsorted/`), commits, and pushes to GitHub.
- **Web UI:** you can read, search and sort transcripts in a browser. People sign in through Pocket ID (passkeys).
- **Connectors:** ktx pulls meeting notes from Granola on a schedule into `transcripts/meetings/`. Each person adds their own credential in the UI; it's stored encrypted.
- **Notes:** a scratchpad in the web UI. Notes are committed to `notes/`, and get a title from an AI model (Neuralwatt) in the background.
- **MCP:** AI agents read transcripts, notes and calendars through an MCP server at `/mcp`, after a person allows them over OAuth.
- **Calendar:** ktx is a CalDAV server (Apple Calendar, Thunderbird). Calendars live in the repo as `.ics` files, and every transcript is linked to the event it was recorded in, which gives it the guests' email addresses.

```
API client ──POST /v1/transcripts──┐
                                   ├─► brain-server ──git pull --rebase / commit / push──► GitHub
browser ──htmx──► web UI ──────────┘        │
                                            └── working copy on disk (BRAIN_WORKDIR)
```

## Transcript format

Markdown with YAML front matter between `---` lines, which GitHub and Obsidian render. `+++` is also accepted on upload. Stored files always use `---`.

```markdown
---
title: Intro call with Acme                # optional; used for the file name
date: 2026-11-14T10:00:00-08:00             # required; RFC 3339 with an offset
timezone: America/Los_Angeles               # optional IANA zone; required if date has no offset
duration_minutes: 30                        # optional
type: customer_discovery                    # optional; lowercase, [a-z0-9_-]; picks the folder
participants:                               # required; at least one, each needs name or email
  - name: Jane Doe
    email: jane@acme.com
    company: Acme
    role: CTO
  - Maxim Iwanskyj <maxim.iwanskyj@icloud.com>   # string form also works
source: granola                             # optional; required if source_id is set
source_id: not_1d3tmYTlCICgjy               # optional; makes uploads idempotent
tags: [pricing]                             # optional
---

**Jane:** ...
```

- **Other keys are kept as written.** So are comments and key order.
- **The server sets these keys:** `id`, `ingested_at` and `uploaded_by`. It overwrites any values the client sent.
- **`date` without an offset** (e.g. `2026-11-14 10:00`) is accepted only when `timezone` is set.

A full example is in [examples/transcript.md](examples/transcript.md).

### Files and ids

The path is `transcripts/<folder>/<YYYY-MM-DD>-<HHMM>-<slug>-<id>.md`.
- The date and time are the meeting's local time.
- The slug comes from `title`, or from the participants' names if there is no title.
- The folder is the `type` (e.g. `transcripts/customer_discovery/`). Without a `type`, it's `transcripts/unsorted/`.

The `id` is the first 12 hex characters of a SHA-256:
- of `source` + `source_id` if a `source_id` is given;
- otherwise, of the uploaded bytes.

**Uploading a transcript whose id already exists overwrites it.** Git keeps the old versions. The server searches all of `transcripts/` for `*-<id>.md`:
- **Identical content:** nothing is committed (`unchanged`).
- **Different content:** the file is overwritten (`updated`).
  - If the `type`, date or title changed, the file is moved or renamed in the same commit.
  - Without a `type`, the file stays in whatever folder it's in, so files you sorted by hand stay sorted.
  - `ingested_at` keeps the first ingestion time. `uploaded_by` is the latest uploader.

Moving a file by hand is fine as long as its name keeps the `-<id>.md` suffix.

## API

All requests need `Authorization: Bearer <token>`.

### `POST /v1/transcripts`

Body: the raw Markdown file (`Content-Type: text/markdown`). Maximum size is `BRAIN_MAX_UPLOAD_MB`.

```sh
curl -X POST https://brain.example.com/v1/transcripts \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: text/markdown" \
  --data-binary @examples/transcript.md
```

| Status | Meaning |
|---|---|
| `201` | New transcript. `{"id","path","result":"created","commit","pushed"}` |
| `200` | Id already existed. `result` is `updated` (with `commit`, plus `previous_path` if it moved) or `unchanged` |
| `400` | Invalid. `{"error","details":[...every problem...]}` |
| `401` | Missing or wrong token |
| `413` | Too large |

If GitHub can't be reached, the commit stays on the server. The response has `"pushed": false` and a `push_error` message. The next upload or periodic sync pushes it.

### `GET /healthz`

No auth. Returns `{"ok":true}`.

## Web UI

![Transcript list](docs/ui-list.png)

- **List** (`/`):
  - Search across title, participants, tags, source and the transcript text.
  - Tabs for every folder, with counts. Sort by date or title.
  - Type a type into a row's field and press Enter (or leave the field). The file moves to `transcripts/<type>/`, `type:` is updated in its front matter, and the change is committed (`Changed-By: <you>`) and pushed. Leave the field empty or type `unsorted` to move it back.
- **Transcript** (`/transcripts/<id>`):
  - Shows the metadata and the body rendered as Markdown. Raw HTML in transcripts is escaped.
  - **Edit** opens a form for the title, date and time zone, duration, type, participants, tags and the transcript itself (Markdown). Saving commits it (`Edit transcript: …`, `Changed-By:`), moving or renaming the file if the type, date or title changed. Keys the form doesn't show (`source_id`, links, `ingested_at`) are kept. Saving an edit adds `edited_at` and `edited_by`; from then on Granola and Pocket syncs leave that transcript alone (changes made in the source afterwards don't come through). An upload by a person still replaces it.
- **Delete** (bottom of a transcript, with a confirm box) removes the file in one commit (`Delete transcript: …`, `Changed-By:`). Its id is added to `.ktx/deleted` in the repo, so Granola or Pocket syncs don't bring it back. Uploading it again by hand (API or UI) restores it and clears the entry. Git history still has the file; truly erasing something means rewriting history.
- **New transcript** (on the list): paste a transcript or notes and fill in the same fields. It's stored with `source: manual` and a random `source_id`, filed under its type (or unsorted).
- **Type suggestions:** `BRAIN_TRANSCRIPT_TYPES` plus every folder that already exists. Any new name matching `[a-z0-9_-]` creates a folder.
- **Built with** [templ](https://templ.guide) components and [htmx 4](https://four.htmx.org). htmx is vendored in `internal/web/static`, so the page loads nothing from a CDN.

**Bulk sorting:** on the list, filter (e.g. search a participant's name or email), tick the rows (the header box ticks all shown), enter a type and Apply. All the moves are one commit.

### Time zone

Settings → General has the workspace's time zone (America/Los_Angeles until set; anyone signed in can change it; stored in `state.db`). Every time in the UI is shown in it: transcripts, including Granola's, which arrive in UTC, plus notes, calendar events and "last used" dates. A transcript that recorded its own zone also shows its local time, e.g. "(09:00 Asia/Tokyo)". The zone also applies to calendar times without a zone, to MCP dates without an offset, and to new note file names. Changing it re-matches transcripts to calendar events.

### Sign-in

With `BRAIN_OIDC_ISSUER` set, every page needs sign-in through the identity provider. On the server that's [Pocket ID](https://pocket-id.org) at `id.internal.melzi.dev` (passkeys, OIDC).
- **Flow:** authorization code with PKCE. A short-lived cookie ties the callback to the browser that started the login.
- **Sessions** live in `state.db`, so a restart doesn't sign anyone out. They last `BRAIN_SESSION_TTL` (24h by default). Removing someone in Pocket ID takes effect at their next sign-in.
- **Admins** are members of the `ktx-admins` group in Pocket ID. That is re-read at every sign-in.
- **Commit attribution:** commits from the UI carry `Changed-By: <username>`.
- **What else protects the UI:** cross-origin form posts are refused, and a strict Content-Security-Policy applies (`script-src 'self'`, no inline scripts or eval). Because of that CSP, htmx features that evaluate JavaScript (`hx-on`, `js:` values, trigger filters) won't work. Put such behaviour in `static/app.js`.
- **Without `BRAIN_OIDC_ISSUER`** the UI is open to anyone who can reach it, and the Connections and API tokens pages are hidden. Only acceptable on a private network.

### Tokens

The **API tokens** page lets each person create tokens for scripts (`POST /v1/transcripts`). A token is shown once, stored only as a hash, and uploads made with it are attributed to its owner. `BRAIN_API_TOKENS` still works for fixed tokens.

**Connected apps** (same page) lists the MCP clients you allowed, when they were last used, and a Revoke button.

## Notes

Free-form notes, separate from transcripts, stored as `notes/<YYYY-MM-DD>-<HHMM>-<slug>-<id>.md`:

```markdown
---
title: Acme pricing follow-up
created: "2026-11-17T16:32:00-08:00"
author: antonio
labels: [customer-feedback]
---
Talked to Jane at Acme. They want usage-based pricing and SSO before signing.
```

- **Scratchpad** (top of **Notes**): write, then Save (or Ctrl/⌘ + Enter). The note is saved right away to a queue in `state.db` and listed as "Generating title…". In the background ktx asks the model for a title, then commits (`Add note: <title>`, `Note-Id:`, `Changed-By:`) and pushes. The page fills in the title when it's ready. Queued notes survive a restart; a failed commit is retried every minute.
- **Labels:** tick any of the defined labels under the scratchpad (or on the edit form). The model adds labels whose meaning fits the note (never removes yours); untick one on the edit form to take it off. The list has a tab per label, and labels are searchable.
- **Titles** come from the model in Settings → AI. Without a key, or if the model fails twice, the title is the note's first line. They're never regenerated: edit the title by hand.
- **List, search, read, edit, delete** like transcripts. Everyone sees every note. Edits and deletes are committed straight away; a note can't be changed until its first commit is done.

### Settings → Labels

Anyone signed in can add labels, change what they mean, or delete them. Definitions live in `notes/labels.yaml` (name and meaning, one commit per change):

```yaml
- name: slop
  description: Pasted AI output, not reviewed; don't trust it as fact
```

Names are lowercase letters, digits, `-` and `_`. The meaning matters: people see it next to the checkbox, the model labels new notes by it, and agents read it with `list_note_labels`. A label that notes still have can't be deleted; take it off them first.

### Settings → AI

One workspace-wide [Neuralwatt](https://neuralwatt.com) key (`https://api.neuralwatt.com/v1/`, OpenAI-compatible, called through [fantasy](https://github.com/charmbracelet/fantasy)). Only admins can set it; it's sealed with the master key like connector credentials, so there's no AI without one. The model defaults to `nw-flash`, Neuralwatt's alias for its current fast model, and is called with reasoning off. Saving checks the key and model with a test title. The first 8000 bytes of each note are sent to Neuralwatt.

## MCP

`https://brain.internal.melzi.dev/mcp` is an MCP server (streamable HTTP, stateless) for AI agents. It needs sign-in to be configured (`BRAIN_OIDC_ISSUER`); without it there's no `/mcp`.

**What an agent can do:** read, as the person who allowed it, everything that person sees in the web UI (every transcript, note and calendar), and write notes. Notes written over MCP always get the `slop` label (the agent can add others but not drop it; a person takes it off in the UI after reviewing), carry `via: "MCP: <app>"` in their front matter and a `Via:` commit trailer, and are attributed to the person. If `slop` isn't defined, the first such note defines it. Nothing else can be changed. Nothing can be changed. Tools:

| Tool | |
|---|---|
| `search_transcripts` | Newest first; filters: words (as in the UI search), type, participant (name, email or company), from/to dates. Returns metadata, the linked event, and a snippet around the match. |
| `get_transcript` | Metadata and text by id; long texts in pages (`offset`, `max_bytes`, `next_offset`). |
| `list_transcript_types` | Types with counts. |
| `list_note_labels` | The note labels, what each means, and how many notes have it. |
| `search_notes` | Notes newest first, with their labels; filters: words, author, label, without_label, from/to dates. Notes still being saved are included (`pending`, no title yet). |
| `get_note` | A note's title, author, labels, dates and text. |
| `create_note` | Writes a note (text, optional title and extra labels), labelled `slop`. Queued and committed like a scratchpad note; without a title the model writes one. |
| `list_calendars` | Every calendar, with owner; `yours` marks the caller's. |
| `list_events` | Occurrences in a range (default: today + 7 days, at most a year), optionally by calendar or words; organizer, guests, location, description (cut at 2000 bytes), and the transcripts recorded in each. |

Every tool requires an `intent`: one sentence on why the call is made. It is written to the log (`journalctl -u brain-server | grep 'mcp call'`) with the user, app and tool, and not stored anywhere else. Dates without an offset are in the workspace's time zone (Settings → General).

**Connecting an app:** add the URL as a remote MCP server. The app discovers the rest and opens a browser:
1. `/mcp` answers 401 with `WWW-Authenticate: Bearer resource_metadata=…/.well-known/oauth-protected-resource/mcp` (RFC 9728), naming ktx as the authorization server (`/.well-known/oauth-authorization-server`, RFC 8414).
2. The app registers itself at `/oauth/register` (dynamic client registration; always a public client). Redirect URIs may be `https`, `http` (ktx is tailnet-only) or an app scheme; `javascript:`, `data:` and the like are refused. Apps that never get allowed are removed after a day.
3. `/oauth/authorize` signs you in through Pocket ID as usual and shows a consent page naming the app, what it can do (read everything, write notes labelled slop) and where you'll be sent back to. PKCE (S256) is required.
4. `/oauth/token` swaps the code (once, within 5 minutes) for an access token (1 hour) and a refresh token (60 days, rotated on every use). Tokens are stored as hashes in `state.db` (`oauth_grants`); revoking the app deletes them.

## Connectors

A connection is a service ktx pulls from, with one person's credential. You add them on the **Connections** page; the owner or an admin can change or delete them.

- **Granola** (Business plan API key) → `transcripts/<type>/<date>-<time>-<title>-<id>.md`. The type is the connection's "Type for new notes" setting (default `meetings`). It only applies to new notes: re-sort one in the UI and later syncs of it stay where you put it.
  - **Front matter:** title, date (calendar time, in the connection's time zone), participants, `source: granola`, `source_id` (the note ID), `granola_url`, `calendar_event_id`, folders.
  - **Body:** summary, the owner's own notes, transcript.
  - **What gets fetched:** only notes Granola has finished (with a summary).
  - **Edits:** a note edited later overwrites its file; git keeps the history.
  - **Schedule:** every 10 minutes, plus **Sync now**.

- **Pocket** (API key from the Pocket app) → `transcripts/internal/` by default (set per connection), with Pocket's AI summary and the timestamped transcript.
  - **Participants:** Pocket rarely knows who spoke (speakers come back as `SPEAKER_00`), so each connection can list its usual participants. Otherwise the owner is listed.
  - **What gets fetched:** completed recordings only; the "Getting Started with Pocket" sample is skipped. The first sync fetches everything.
  - **Re-reads:** later syncs re-read the last week of recordings, because editing a summary in Pocket doesn't change the recording's `updated_at`. Unchanged ones aren't committed.
  - **Schedule:** every 15 minutes, plus **Sync now**.

How a run works ([internal/connectors](internal/connectors/connectors.go)):
1. **Fetch:** the connector fetches everything changed since the connection's bookmark.
2. **Commit:** ktx writes it as **one commit per run** (in batches of 50), with `Connection:` and `Uploaded-By: <owner>` trailers.
3. **Bookmark:** saved only after that commit exists. A failed run keeps what it fetched, and the next run starts from the same place. Re-fetching is harmless because documents are overwritten by id and unchanged ones aren't committed.
4. **History:** each connection's page shows its last runs and any error.

**Credentials are encrypted at rest** ([internal/secrets](internal/secrets/secrets.go)), with AES-256-GCM and each one bound to its connection.
- **The master key** is a systemd encrypted credential (`LoadCredentialEncrypted=ktx-master-key`), decrypted at start with the host key. No password is needed at restart.
- **What that protects against:** a copied `state.db` is useless.
- **What it doesn't:** root on the server, which can read everything.
- **In the UI** a credential is write-only: you see its last four characters, and can replace or delete it.

**Adding a connector:** implement [`connectors.Connector`](internal/connectors/connectors.go) (fields, `Check`, `Sync`) and register it in `cmd/brain-server/main.go`. [granola](internal/connectors/granola/granola.go) is the example.

### Editing templates

```sh
go tool templ generate -path internal/web   # after changing views.templ; commit the generated views_templ.go
```

## Calendar

Calendars are stored in the brain repo, one `.ics` file per event or task (a repeating event and its changed occurrences share a file):

```
calendar/<owner>/<calendar>/calendar.json      name, colour, description
calendar/<owner>/<calendar>/<uid>.ics          one event (VEVENT) or task (VTODO)
```

**Tasks:** every calendar holds tasks (to-dos) as well as events, so it shows up in task apps (Apple Reminders, Thunderbird Tasks) too. A file holds either an event or a task, never both. Tasks aren't matched to transcripts. Tags are the standard `CATEGORIES`.

**In the web UI:**
- **Calendar** (`/agenda`): a week at a time, day by day (previous / this week / next), with times, location, guests and the transcripts recorded in each event. **Mine** shows your calendars, **Everyone** all of them.
- **Tasks** (`/tasks`): open tasks sorted by due date (undated last, then by priority); overdue ones in red. **Sort: Due date | Manual** switches your own list (remembered per person): in Manual, drag a task by its ⠿ handle (mouse or touch; or focus the handle and press ↑/↓) to put it where you want. Tasks you haven't placed yet, such as new ones, sit at the top in due-date order; done tasks stay below. Your order is kept in `calendar/<you>/task-order.json` (task keys `<calendar>/<file>`; one commit per move) and is kept when you switch back to Due date. Task files aren't touched, so phones don't re-download anything. **Everyone** is always by due date. Add a task with a title and an optional due date and time (in the workspace's time zone; a date alone is an all-day due date); it goes into your `personal` calendar, or pick another if you have several. Tick a task to mark it done (or untick to reopen); **Show done** lists completed ones, newest first. Open a task to edit its title, due date, tags and notes, or delete it. Edits change only those fields; alarms, relations and whatever else an app wrote are kept. If the task changed elsewhere since the page loaded (e.g. on your phone), the change is refused rather than overwriting it. **Everyone** shows other people's tasks, read-only. **Repeating tasks:** pick a repeat when adding or editing (every day, weekday, week, 2 weeks, month, year); a repeat needs a due date, and it counts from there (DTSTART is set to the due date if the task has none, as Apple Reminders does). A rule set in another app shows as Custom and is kept. Ticking a repeating task done moves it to its first date after both its current one and now (start and due move together, `COUNT` is used up) and keeps it open; it's completed only when the rule has no date left. Moving the due date of a repeating task moves the series.

Every change, from a calendar app or the UI, is one commit (`Calendar: add "Acme intro" (antonio/personal)`, with `Changed-By:`).

**Who can see what:** everyone sees every calendar; only its owner can change it. Over CalDAV, other people's calendars appear as read-only, named `Personal (maxim)`.

**Connecting an app:** make a calendar token on **Settings → Tokens** (separate from API tokens: a calendar token only works for CalDAV, and an API token never does). Sign in with your ktx username and the token as the password. Tailnet only.
- **iPhone:** Settings → Apps → Calendar → Calendar Accounts → Add Account → Other → Add CalDAV Account; server `brain.internal.melzi.dev`.
- **Thunderbird:** New Calendar → On the Network; location `https://brain.internal.melzi.dev/dav/`.

The first sign-in creates a `personal` calendar. Rename a calendar on **Settings → Calendar** or in your calendar app (name and colour); its id, and so its path, never change. Clients can create more (MKCALENDAR or extended MKCOL), or use **Settings → Calendar**.

**Seeding from an export:** **Settings → Calendar** → Import an .ics file (Google, Apple and Outlook exports, up to 50 MB). Events are upserted by UID; nothing is deleted, so importing again is safe. Tasks (VTODO) are imported the same way. The page reports what was added, updated, unchanged and skipped (e.g. entries without a UID), and counts events and tasks per calendar.

**CalDAV support:** discovery (`/.well-known/caldav`, principal, home set), PROPFIND, calendar-query with time ranges (repeating events and tasks expanded; tasks by RFC 4791 §9.9), calendar-multiget, GET/PUT/DELETE with ETags and `If-Match`/`If-None-Match`, and `getctag` for change detection. Not supported: sync-collection (clients fall back to ETags), scheduling (invitations aren't emailed), free/busy.

### Linking transcripts to events

When a transcript is stored, and whenever calendars change, ktx looks for the event it was recorded in, across everyone's calendars:

1. **Exact:** the recording names its event (Granola's `calendar_event_id`, including Google's per-occurrence ids).
2. **By time:** an event (not all-day) that started within 15 minutes of the call, or that the call started during. An event with guests beats one without; then the one with more of the call's own participants (email among its guests, or full name in its title); then the nearest start; then the title most like the call's. A tie is no match. Every transcript is re-matched when the server starts, so rule changes apply to existing ones. The same event in several calendars counts once.

The result goes into the transcript's front matter, and the event's organizer and guests (minus declined ones and rooms) are added to its participants, marked `from: calendar`:

```yaml
calendar_event:
  uid: 2su99n6iiik37iiknmb5t4fkfh@google.com
  recurrence_id: "2026-11-14T18:00:00Z"   # only for one occurrence of a repeating event
  calendar: maxim/personal
  title: Acme <> Melzi
  start: "2026-11-14T18:00:00Z"
  match: time                             # exact, time, manual, none
participants:
  - name: Jane Doe
    email: jane@acme.com
    from: calendar
```

Those emails are the link to Folk (people are matched by email); no Folk ids are stored. On a transcript's page you can pick another event, say there is none, or go back to automatic. A choice made by hand (`manual`, `none`) is never changed automatically. Calendar participants are replaced whenever the link changes, and are left out of the edit form (saving re-adds them).

## Configuration

| Variable | Default | |
|---|---|---|
| `BRAIN_REPO_URL` | required | e.g. `git@github.com:org/brain.git` |
| `BRAIN_API_TOKENS` | | Optional fixed tokens, `antonio:<token>,maxim:<token>`; at least 24 chars (`openssl rand -hex 32`). The name goes into `uploaded_by` and the commit's `Uploaded-By:` trailer. People can also create their own tokens in the UI. |
| `BRAIN_STATE_DB` | `./data/state.db` | SQLite: users, sessions, API tokens, OAuth apps and grants, connections, run history, settings (sealed AI key), notes waiting to be committed |
| `BRAIN_MASTER_KEY_FILE` | | 32-byte key (raw or base64) that seals connector credentials and the AI key. Without it, the systemd credential `ktx-master-key` is used; without either, connectors and AI titles are off. |
| `BRAIN_OIDC_ISSUER` | | Identity provider URL, e.g. `https://id.internal.melzi.dev`. Turns on sign-in. |
| `BRAIN_OIDC_CLIENT_ID`, `BRAIN_OIDC_CLIENT_SECRET` (or `_FILE`) | | ktx's client at the identity provider |
| `BRAIN_PUBLIC_URL` | | ktx's own URL; the callback is `<url>/auth/callback`. Also the OAuth issuer, and the MCP resource is `<url>/mcp`. |
| `BRAIN_OIDC_ADMIN_GROUP` | `ktx-admins` | Members are ktx admins |
| `BRAIN_SESSION_TTL` | `24h` | How long a sign-in lasts |
| `BRAIN_SSH_KEY_PATH` | | Deploy key (private key file) |
| `BRAIN_SSH_KNOWN_HOSTS` | | Pinned `known_hosts`. Without it, the host key is trusted on first use. |
| `BRAIN_BRANCH` | `main` | |
| `BRAIN_WORKDIR` | `./data/repo` | Clone location; created on first start. Keep it on a persistent disk. |
| `BRAIN_LISTEN_ADDR` | `:8080` | |
| `BRAIN_SYNC_INTERVAL` | `5m` | Pull, and push unpushed commits, on this interval. `0` turns it off. |
| `BRAIN_GIT_AUTHOR_NAME` / `_EMAIL` | `brain-server` / `brain-server@users.noreply.github.com` | Commit author |
| `BRAIN_MAX_UPLOAD_MB` | `10` | |
| `BRAIN_TRANSCRIPT_TYPES` | `advisor,customer_discovery,investor,internal` | Type suggestions in the UI |

### Deploy key

```sh
ssh-keygen -t ed25519 -N "" -C brain-server -f deploy.key
# GitHub → repo → Settings → Deploy keys → Add deploy key: paste deploy.key.pub, tick "Allow write access"
ssh-keyscan github.com > known_hosts
# Compare against https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
```

### Run

```sh
go run ./cmd/brain-server   # with the variables above set
```

```sh
docker build -t brain-server .
docker run -p 8080:8080 -v brain-data:/data \
  -v $PWD/deploy.key:/secrets/deploy.key:ro -v $PWD/known_hosts:/secrets/known_hosts:ro \
  -e BRAIN_REPO_URL=git@github.com:org/brain.git \
  -e BRAIN_SSH_KEY_PATH=/secrets/deploy.key -e BRAIN_SSH_KNOWN_HOSTS=/secrets/known_hosts \
  -e BRAIN_API_TOKENS="antonio:...,maxim:..." brain-server
```

ssh refuses private keys that other users can read, so mount the key with owner-only permissions (`chmod 600`) and readable by uid 10001.

Serve it over HTTPS (a reverse proxy, or a platform that terminates TLS). The proxy must:
- keep the `Host` header, because cross-origin protection compares it with `Origin`;
- set `X-Forwarded-Proto: https`, which marks the session cookie `Secure`.

## How git is handled

- **One lock covers every git operation.** The server is the only writer to its working copy.
- **Each upload does:** `fetch` → `rebase` onto `origin/<branch>` → write file → `commit` → `push`. If the push is rejected because someone else pushed, it rebases and retries up to three times. So you can push from your laptop at the same time.
- **If a write or commit fails,** the file is removed again so the working copy stays clean.
- **On startup:**
  - A missing clone is cloned. An empty GitHub repo works; the first upload creates the branch.
  - An existing clone must point at `BRAIN_REPO_URL`.
  - Leftover uncommitted changes (e.g. from a crash) are stashed, not deleted.
- **If your commits conflict with the server's unpushed ones,** the rebase is aborted and the error is logged. Fix it by hand in `BRAIN_WORKDIR`. This should be rare: the server mostly adds files, and only rewrites one when it is re-uploaded.

## Deployment

The server is reachable only over the tailnet. Caddy serves `*.internal.melzi.dev` on the tailnet address with one wildcard certificate.

```
GitHub release ──► release.yml builds brain-server + caddy, attaches them to the release
                                              │
box (tailnet only)                            ▼
  ktx-update.timer (every minute) ── installs /opt/ktx/releases/<tag>, restarts, rolls back if unhealthy
  caddy        ── 100.64.0.14:443, *.internal.melzi.dev cert via Cloudflare DNS challenge
     └ internal_dns ── publishes an A record → tailnet IP for every site under internal.melzi.dev
  brain-server ── 127.0.0.1:8080: UI, API, connectors (Granola → transcripts/meetings/); clones/pushes melzi-cloud/company-brain
     └ state.db ── users, sessions, API tokens, connections (credentials sealed with the master key)
  pocket-id    ── Podman, no network, Unix socket ← caddy (id.internal.melzi.dev); ktx signs people in here
```

| Path | What |
|---|---|
| `/etc/caddy/Caddyfile` | Global config (from `deploy/caddy/Caddyfile`) |
| `/etc/caddy/Caddyfile.d/*.caddyfile` | One file per site. Saving a file reloads Caddy (`caddy-reload.path`). |
| `/etc/caddy/caddy.env` | `CLOUDFLARE_API_TOKEN` (Zone:Read + DNS:Edit on melzi.dev). Caddy stays stopped until it is set and starts by itself once it is saved. |
| `/etc/ktx/brain-server.env` | brain-server settings; API tokens are generated here at install |
| `/etc/ktx/company-brain.key` | Deploy key (write) for the data repo |
| `/var/lib/ktx/state.db` | Users, sessions, API tokens, connections (credentials sealed) |
| `/etc/credstore.encrypted/ktx-master-key` | Master key, encrypted with the host key (`/var/lib/systemd/credential.secret`) |
| `/etc/ktx/oidc-client-secret` | ktx's client secret at Pocket ID |
| `/etc/containers/systemd/pocket-id.container` | Pocket ID, as a Podman Quadlet |
| `/var/lib/pocket-id` | Pocket ID's database (users, passkeys, clients). Backed up daily to `/var/backups/pocket-id` |
| `/etc/pocket-id/secrets/` | Pocket ID's encryption key and admin API key |
| `/opt/ktx/current` → `releases/<tag>` | Running binaries |
| `/usr/local/libexec/ktx-update` | Release installer (from `deploy/ktx-update/`) |
| `/usr/local/libexec/ktx-ssh-guard` | Closes public SSH while Tailscale is healthy (from `deploy/ssh-guard/`) |

**Pocket ID** (identity for ktx and later apps) runs in Podman ([pocket-id.container](deploy/pocket-id/pocket-id.container)).
- **Isolation:**
  - **No network at all:** Caddy reaches it through a Unix socket in `/run/pocket-id`, whose directory only Caddy can enter. It can't make outbound connections or reach anything else on the box.
  - **Non-root user** (`pocketid`), read-only image, all capabilities dropped.
- **Updates:** `podman-auto-update.timer` follows the `v2-distroless` tag daily and rolls back if the new image doesn't start. v3 has to be adopted on purpose.
- **Backups:** daily, of the database and its encryption key, to `/var/backups/pocket-id` (14 kept). This is the one piece of state that can't be rebuilt.
- **Setup and invites:** [pocket-id-provision](deploy/pocket-id/pocket-id-provision) (run by `install.sh`) creates the groups `melzi` (may use ktx) and `ktx-admins`, plus the `ktx` OIDC client and its secret. To add a person:
  ```sh
  pocket-id-provision invite maxim maxim@example.com          # member
  pocket-id-provision invite antonio ac@melzi.dev --admin     # also ktx admin
  ```
  It prints a one-time link (24h). They open it and add a passkey. People can also be managed in Pocket ID's own admin UI.

**Sandboxing.** Every service runs under systemd's sandbox options. Exposure scores from `systemd-analyze security` (0 is fully locked down, 10 is no sandbox):

| Service | Runs as | Score | Allowed |
|---|---|---|---|
| brain-server | `ktx` | 1.3 | Writes `/var/lib/ktx` only; outbound network; runs git/ssh; no capabilities; syscall filter |
| caddy | `caddy` | 1.8 | Writes `/var/lib/caddy` only; binds 80/443; reads the tailnet address over netlink |
| pocket-id | `pocketid` in Podman | n/a | No network, read-only image |
| ktx-update, ktx-ssh-guard, pocket-id-backup | root, short jobs | ~3.5 | Confined to the files they manage. The guard fails open: if its sandbox ever breaks it, SSH stays reachable. |

SELinux is still in permissive mode. Turning it on would also confine the Pocket ID container, but check a few days of `ausearch -m avc` output first.

**Adding an internal site** means dropping in a file like this:

```
# /etc/caddy/Caddyfile.d/grafana.caddyfile
grafana.internal.melzi.dev {
	reverse_proxy 127.0.0.1:3000
}
```

Caddy reloads. [`internal_dns`](caddy/internaldns/internaldns.go) creates `grafana.internal.melzi.dev A 100.64.0.14` in Cloudflare, and the site uses the wildcard certificate.

What `internal_dns` touches, and what it doesn't:
- **Only** names ending in `.internal.melzi.dev`. The zone apex and other `melzi.dev` names are never changed, and wildcard names are skipped.
- **Deleting records:** when a site is removed, its record is deleted, but only if this box created it and it still points here.
- **Re-syncing:** on every config load and every 10 minutes.

**Releasing:** publish a GitHub release (e.g. `v0.2.0`).
1. `release.yml` tests, builds `linux/amd64` and `linux/arm64`, and attaches `ktx_<tag>_linux_<arch>.tar.gz` and `SHA256SUMS`.
2. Within a minute or so of the assets appearing, `ktx-update` on the box:
   - verifies the checksum, switches `/opt/ktx/current`, and restarts brain-server (and Caddy, only if its binary changed);
   - checks `/healthz`. If the release doesn't come up healthy, it rolls back and never retries that tag.
3. Logs: `journalctl -u ktx-update`.

**SSH guard:** public SSH (port 22) is dropped by an nftables rule while Tailscale is healthy. SSH over the tailnet always works.
- **How it works:** the rule is a dead man's switch. It only applies while a set entry exists, and that entry expires after 3 minutes. `ktx-ssh-guard.timer` renews it every 30s, but only while `tailscaled` is running, logged in, and has a tailnet address.
- **When public SSH opens again:**
  - within about 30s of Tailscale going down;
  - within 3 minutes if the guard itself stops running;
  - at boot, until Tailscale is up.
- **Tested on the box:** stopping `tailscaled` opened public SSH after about 38s, and starting it closed public SSH again. Stopping the guard opened public SSH once the entry expired.
- **Check it:** `nft list table inet ktx_ssh_guard` and `journalctl -u ktx-ssh-guard`.
- **Last resort if both paths fail:** the Hetzner web console.

**Automatic updates:**
- **OS:** dnf-automatic installs all updates daily (06:00 UTC, plus up to an hour of random delay) and reboots when an update needs it.
- **After a reboot**, everything comes back on its own: Tailscale, the SSH guard, Caddy (which retries until the tailnet address exists), brain-server and the timers. Tested with a real reboot.
- **Caddy and Go dependencies** are separate: Dependabot opens weekly PRs. Merge them, then publish a release.

**Setting up a new box:** copy the repo to it and run `deploy/install.sh <brain-server> <caddy>` as root, with binaries from a release or from `go build`. Then add the Cloudflare token and the data-repo key. The script is safe to re-run.

## Tests

```sh
go test ./...                   # uses the local git binary and temporary bare repositories
(cd caddy && go test ./...)     # Caddy build and internal_dns (separate Go module, to keep Caddy's dependencies out of brain-server)
```

The web tests include a fake OIDC provider (discovery, signed ID tokens, PKCE checks), so the sign-in flow is tested without Pocket ID. The Granola tests use a fake API built from Granola's reference.
