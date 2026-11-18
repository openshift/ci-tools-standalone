# Ops alert dashboard

A standalone, Slack-backed operations dashboard. It reads the ops channel, serves
an in-memory queue, and posts accepted workflow actions as DPTP-bot thread replies.
Slack holds the durable record. There is no database, PVC, alert-proxy dependency,
or dependency on `openshift/ci-tools`.

## Try it locally

```sh
make build-ops-alert-dashboard
_output/ops-alert-dashboard --demo --listen=127.0.0.1:8080
```

Open `http://127.0.0.1:8080`. Demo mode uses synthetic Slack messages and the real
classification, replay and action code. It seeds the workflow views, a bot PR list and a
Chai reply. It never connects to Slack or the GitHub PR API. Demo state resets when the process stops.

## Live configuration

Copy `examples/config.yaml` and replace its Slack identity and access placeholders.
Mount the classification file from
`openshift/release/core-services/slack-bot/_ops-alerts.yaml`. The copy in
`examples/classification.yaml` is a demo fixture, not another production catalog.

```sh
ops-alert-dashboard \
  --config=/etc/ops-alerts/config.yaml \
  --classification=/etc/ops-alerts/_ops-alerts.yaml \
  --slack-token-file=/etc/slack/token \
  --history-window=48h \
  --listen=127.0.0.1:8080
```

Validate files without credentials or Slack calls:

```sh
ops-alert-dashboard --validate-only \
  --config=/etc/ops-alerts/config.yaml \
  --classification=/etc/ops-alerts/_ops-alerts.yaml
```

Use the DPTP-bot token for writes. Startup checks its workspace, user, bot and
workspace URL against `auth.test`. Configure trusted producers by Slack-issued
user, bot or app IDs, never display names. The Chai source must include its bot
user ID so the dashboard can mention it explicitly. Tokens are read from files
and never sent to the browser. If needed, `--slack-history-token-file` selects a
separate read token in the same workspace.

SHIP notifications are published by DPTP-bot. Configure that producer as
`kind: ship` with the same user ID as `slack.writer.user_id`, as in the example.
Only SHIP may share the writer identity. Dashboard action replies do not become
alerts, and other DPTP-bot parent notifications default to Orange / Other.

The app must be a member of the channel and its token must be authorized for
channel history and replies, `chat:write`, and `reactions:write`.
For discussion display names, avatars and custom emoji, also grant the read
token `users:read` and `emoji:read`. These directory reads are shared and cached,
not repeated for every message or browser. Missing directory access leaves
readable Slack IDs and emoji shortcodes with a warning; it does not grant access
to additional channels. Bot identity is never changed with `chat:write.customize`.
See Slack's [history](https://docs.slack.dev/reference/methods/conversations.history/)
and [replies](https://docs.slack.dev/reference/methods/conversations.replies/)
documentation for scopes and workspace limits. Read requests fetch every page,
honor `Retry-After`, and stop on cancellation. Writes are never blindly retried.

Run **one replica** behind an authenticated OpenShift SSO reverse proxy. Keep the
backend private; its default listen address accepts only loopback connections.
The proxy must remove incoming identity headers and inject the verified identity
in `access.user_header` (default `X-Forwarded-Email`). Configure the exact public
HTTPS origin, trusted proxy CIDRs and allowed operator identities. The backend
checks them for both reads and writes, derives the actor server-side, and requires
a user-bound CSRF token and matching Origin on mutations. There is no live
unauthenticated mode. Changing the listen address does not change proxy trust.

By default, live mode requires a nonempty `access.operators` allowlist. To use a
proxy that also authorizes group access, set `access.authorization: proxy`, omit
`access.operators`, and explicitly configure `access.trusted_proxies`. The proxy
must enforce authorization before forwarding any dashboard request, as well as
replacing identity headers. For core-ci, configure its OAuth proxy to authorize
`get` on the `ci/ops-alert-dashboard` Service through SubjectAccessReview, with a
RoleBinding granting the existing `test-platform-ci-admins` group that access.
This mode accepts any well-formed authenticated identity forwarded by that trusted
proxy, so keep direct backend access private. It still requires the public HTTPS
origin and exactly one nonempty identity header; actions retain origin and
user-bound CSRF checks and record the current SSO identity for audits. Unknown
authorization values, combining proxy authorization with an operator allowlist,
and using proxy authorization in demo mode are rejected.

## Classification and views

- Managed Alertmanager and Prow messages publish
  `[ops_color=red ops_category=ci_platform]`. Valid pairs from trusted producers
  are used before narrow fallback rules. Alertmanager can use platform and legacy
  rules; markerless Prow jobs use only legacy rules, including historical cleanup
  jobs. Prow report identifiers also recognize `branch-ci-*` job names.
  Source-owned markers remain authoritative. Unknown or invalid classifications
  are Orange / Other.
- Recognized Chai scheduled reports have a neutral Chai report label. They have
  their own latest-per-task overview, report details and history, and are excluded
  from alert counts. The scheduler's `scheduled_task` Slack metadata identifies
  each task, falling back to the task ID before `| Instructions:` in the visible
  header. Instruction paths are source context, not separate task names. Clearly
  labeled coverage and limits remain visible alongside the findings. Presentation keeps
  the original wording and full source, without inferring health from prose.
  The catalog's legacy Green classification remains an internal report type,
  not a displayed priority or health status. Unknown report formats remain readable.
  Multiple parent messages combine only when trusted scheduler metadata supplies
  the same bounded task, persona and request ID, scoped to the actual producer
  and channel. Missing run metadata leaves each parent separate; adjacency never
  establishes a run. Original parts and their permalinks remain available.
  Pending workload reports show explicitly reported Pending pod counts by cluster,
  including zero, with source evidence available by selecting a bar. Unavailable
  or conflicting counts have no numeric bar. Counts include system and
  overprovisioning pods and do not measure blocked jobs or severity. The previous
  retained report supplies a comparison column; coverage and qualifiers can differ.
  Extraction is bounded and conservative; unsupported findings stay in the source
  text and missing clusters are never assigned zero. Other report types keep their
  readable findings and coverage without inferred numeric charts.
  Latest and historical reports render a safe Markdown subset: headings,
  paragraphs, ordered and unordered lists (up to eight nesting levels), indented
  list continuations, blockquotes, fenced code, inline code, simple pipe tables,
  HTTPS Markdown links and Slack links/emoji. Double stars render bold; single
  stars retain Slack bold semantics, underscores render emphasis, and tildes
  render strikethrough. Fence language markers are omitted from code content.
  HTML and remote images are never rendered. Original source remains available
  unchanged; readable coverage excerpts use the same display time conversion.
  Ordinary Chai conversations do not create alerts. Demo examples use the six
  recurring task envelopes with entirely synthetic findings. Healthy scheduled
  scans may deliberately post nothing, so absence of a report does not establish
  scan success or failure.
- SHIP root notifications beginning `🚨 Outage Detected: component/subcomponent`
  (or Slack's `:rotating_light:` alias) are Yellow. Build Farm maps to Cluster
  fleet and Downstream CI / CI-Config to Repo automation. Description prose does not determine classification. Other
  trusted DPTP notifications honor valid producer markers, then use legacy rules
  only for structured `[FIRING]` / `[NO LONGER FIRING IN ALERTMANAGER]` headers
  and Repo brancher fast-forward failure summaries or headings. These summaries
  use the stable identifier `repo-brancher-fast-forward-failures`.
- A grouped notification remains one occurrence, retaining its parsed alert
  identifiers. It uses the highest alert color and Other if categories differ.

Every new parent message is a new occurrence, identified by channel and Slack
timestamp within the configured workspace. Repeated delivery and edits update
that occurrence. Replies add evidence. A resolved notification does not close an
operator investigation automatically.

Alerts with a published SOP link show an expandable SOP panel beside their
workflow actions. Opening it reads the linked Markdown from the public
`openshift/release` repository, with headings, lists, tables and command blocks.
A link to a heading opens that section first; the original GitHub document stays
available. Commands are displayed as text. The preview does not perform workflow
actions or run commands.

Only SOP Markdown under `docs/dptp-triage-sop/` on `main` or `master` is previewed.
The authenticated endpoint accepts a link already present on that occurrence,
uses a bounded request to the fixed raw GitHub host and rejects redirects.
Unreachable or missing documents leave the original source link available.
Browser memory retains opened documents for the page visit. Demo alerts are
synthetic, while their SOP previews read the real public documentation on request.

## Browser preferences and presentation

The workspace uses the full desktop width, with view tabs across the top.
Select a row to open its detail panel, and use **Close details** to return to the
full list. **Ask Chai** stays fixed in the bottom-right corner while scrolling,
with its existing conversation controls.

Alert views also offer an optional **Group by name** checkbox. It collapses
occurrences that share one alert name, the first parsed identifier, or the
readable title when no identifier was parsed, behind expandable groups that
start closed, including single occurrences. Groups with personally unread
members are listed first, newest-first among themselves, followed by fully
read groups, also newest-first. Members keep the list's newest-first order
inside their group. Group counts follow the current view, search,
category and priority filters, and a group with personally unread members also
shows their count next to the total, for example `2 new`, using the same
personal unread state as the row markers. Grouping is display-only: expanding
a group never marks an alert read or opens the inspector, and every row keeps
its own checkbox, links and actions. The flat list remains the default, and
toggling back preserves the current selection and filters. The choice is saved
per browser in a cookie (about one year, `Path=/`, `SameSite=Lax`, `Secure` on
HTTPS) that holds no user or alert data, separately from the per-user local
preferences; unknown, malformed or blocked cookie values fall back to the flat
list. Which groups are open is kept only for the current visit, including
across automatic refreshes, and is never persisted.

Checkboxes select open alerts in the current view and filters; **Select all matching**
includes the entire filtered list. **Close selected**, **Snooze selected**, and
**WIP selected** preview the exact targets before using a resumable action plan.
All workflow actions preserve the current view, search, category and priority
filters. After a confirmed action, an open inspector advances to the next
unaffected alert in the list, or the previous alert at the end. It closes when
no other alert remains. Failed or unconfirmed actions keep the current alert;
explicit navigation while a request is running takes precedence.

Times use the browser's local timezone, including daylight saving. Readable Slack
and Chai report prose also converts explicit UTC ISO timestamps (`Z`, `+00:00`
or `+0000`) and `YYYY-MM-DD HH:mm[:ss] UTC` values, showing the local UTC offset.
Clock-only `H:mm[:ss] UTC` values use that occurrence or report's UTC calendar
date, including each historical report and Slack reply's own timestamp. Without
a source date, clock-only values remain literal. Naive times, code and link
URLs are preserved; Original Slack disclosures retain the exact source text.
The channel name defaults to `ops-testplatform`; set `--slack-channel-name` when
configuring a different channel. The UI displays channel names in cards, dialogs and rules.
Titles prefer parsed identifiers and preserve a leading source 🚨, with source
firing/resolved status shown separately. Prow run links remain directly accessible. Message evidence supports
safe Slack formatting and common Unicode emoji aliases; unsupported custom emoji
remain readable. Raw message HTML is never executed.

Unread IDs and notification preferences are scoped to the authenticated user in
this browser's local storage. They survive page reloads and SSO renewal but do not
synchronize across devices. Existing occurrences form the first-visit baseline;
explicitly opening a card or Mark all read clears personal unread state. Severity
colors, shared WIP state and personal unread indicators are independent.
Feed navigation always shows its shared occurrence total; a separate red badge
appears only for a nonzero personal unread count.
New Feed occurrences since the last complete sync remain unread when returning
after a reload or sign-in. Incomplete coverage preserves the saved read state.

Desktop notifications are opt-in while the page is open. Enable notifications
requests browser permission through a button; the browser retains that permission
separately from the saved dashboard preference. New Feed arrivals are grouped into
bursts, deduplicated across supported browser tabs, and do not include Chai reports
or suppressed occurrences. Suspended or closed tabs are not guaranteed to notify;
there is no push subscription service. Authentication or stale coverage pauses
notification delivery. Personal storage failures do not weaken action retry guards.

An expired proxy session pauses background requests and provides Sign in again.
Old data is marked stale and action delivery remains unconfirmed until reconciled.
No pending mutation is replayed automatically after sign-in. The external proxy
controls session lifetime; the core-ci deployment configures twelve hours.

## Slack discussions

Choose **Load Slack conversation** in an alert's inspector to open its original
Slack thread in a separate, non-modal discussion panel. Loading a conversation
does not mark the alert read, acknowledge it, or start a Chai investigation.
Repeated alert names still have separate source threads; the server resolves
the destination from the retained occurrence, not browser-supplied channel or
thread identifiers. **Open in Slack** remains available for the original view.

The panel shows the root message and a bounded page of replies in chronological
order (up to 30 replies per page). **Load older messages** retrieves earlier
cached replies only when requested. Names, avatars, bot identity, timestamps, edited indicators,
mentions, reactions, custom emoji, supported Slack formatting, attachments and
rich blocks are retained. Unsupported interactive blocks or media are identified
with a link to the original Slack thread rather than silently interpreted as
dashboard actions. Message HTML is never executed. Remote images are restricted
to approved public Slack image hosts and the approved avatar host; arbitrary
external images are not fetched through a server proxy.

Opening or reopening a discussion requests a fresh Slack read. While the panel
is open in a visible browser tab, it refreshes automatically every 15 seconds.
**Refresh Slack conversation** requests the same targeted read immediately;
neither operation waits for the next full channel sync or depends on changed
reply-count metadata. Closing the panel, hiding the tab, or losing authentication
stops automatic refreshes. Returning to a visible open panel requests fresh data.
Drafts, loaded older pages, and pending delivery guards survive refreshes.

Reads still share the complete Slack thread cache used for workflow recovery.
Recent successful reads are reused across viewers for five seconds, and targeted
reads share the active Chai conversation worker's read budget and Slack backoff.
**Load older messages** remains cache-only. The panel shows the thread's actual
last successful Slack read time. Failed or throttled refreshes retain displayed
messages with an error instead of presenting them as newly synchronized, and
automatic retries respect `Retry-After`. This is polling, not a streaming
connection or a guarantee that every reply appears within 15 seconds: upstream
rate limits, read failures, and contention can extend that delay. Refreshes never
post Slack messages, reactions, or workflow commands.

User profiles and relevant custom emoji are resolved lazily through a
bounded shared directory cache, including cached failures, so reopening a thread
does not repeat fresh directory lookups. Successful metadata is cached for one
hour and failures for five minutes; Slack rate-limit backoff is shared across
viewers. The directory cache retains at most 512 identities and 4,096 custom
emoji, with bounded page-level enrichment and no remote image proxy.

Only **Post reply** sends a message. Replies go to the selected alert's original
thread as the configured dashboard bot, with the authenticated operator clearly
attributed in the visible Slack header as well as the delivery record. They do
not impersonate a Slack user or change alert workflow state. Chai is notified
only when explicitly mentioned. The composer accepts up to 4,000 UTF-8 bytes.

Explicit mentions remain actionable. Workspace `@handles` resolve to active,
unambiguous Slack user IDs, including users who have never appeared in a cached
discussion. Use the Slack handle, not a display name. `@chai-bot` uses the
configured Chai Slack user ID, and the configured channel name (for example
`#ops-testplatform`) resolves to its channel ID.
Canonical Slack references such as `<@U123ABC>` and `<#C123ABC>` are also retained.
User and channel references in the discussion viewer link to their Slack
profiles and channels.

To limit Slack API usage, the server loads the workspace user directory only
when an unresolved handle needs it, using the reader token's existing
`users:read` scope. All operators share one cursor-paginated load per server
instance, with 200 users per page and at most 10 page requests per minute.
Complete indexes, including unknown-handle results, are reused for 24 hours;
unknown names do not force extra scans. There are no scans on startup, page
loads, or keystrokes. The trade-off is that new or renamed handles (and account
deactivations) may take up to 24 hours to appear. Explicit `<@USERID>` references
and configured Chai/current-channel references do not require a directory scan.

A cold or refreshing directory may take time to load in a large workspace. A
send waits at most two seconds for that shared load, then returns a definite
"no reply was posted" error with the draft retained. Loading continues in the
background, but never sends a deferred reply: click **Post reply** again when
ready. Directory loading does not hold the workflow/posting lock. Slack
`Retry-After` delays are respected; failed loads have a shared 15-minute cooldown
(or longer if Slack requires it). Loads stop at server shutdown and are bounded
to 1,000 pages, 200,000 users, and two hours; incomplete indexes are never used.

Unknown, ambiguous, or deactivated handle references are rejected before posting,
with a readable error and the draft retained, rather than silently becoming
ordinary text. Use an explicit reference from Slack if a recently added or
renamed handle is not in the daily directory snapshot. Code blocks, inline code,
escaped references, email addresses, URL fragments, and numeric issue references
such as `#123` and `#123.` stay literal. User-group and broadcast mentions are not
supported by this composer; use Slack for those deliberately wider notifications.

New replies containing the reserved `OPS_ALERT_ACTION_V1` workflow-control marker
are rejected rather than rewritten, so quoted text cannot become an action
record when read by an older dashboard version. Existing Slack evidence remains
visible without changing its text.
Slack may normalize emoji and links when returning messages. Delivery records
identify the original submission independently of that display formatting;
edited messages keep their edited indicator and actual Slack sender identity.
Legacy generic reply headers remain readable. Once attributed headers have been
posted, rollback requires a version that recognizes those headers.
Drafts survive closing the panel and normal dashboard refreshes, but private
display state and drafts are cleared when authentication or operator identity
changes.

The exact request ID, text and destination are retained in per-operator session
storage before posting. Retry is explicit and reuses that request; ambiguous
delivery is never treated as a confirmed failure or automatically reposted.
Retries of uncertain or restored requests only reconcile against confirmed
archive evidence, including after a server restart. Confirmation is independent
of pagination, so an older reply can be confirmed without reloading or reposting
it. If confirmation is still
unavailable, check the original Slack thread before explicitly discarding the
retained request; discarding does not prove that the reply was not delivered.
Reads and replies require the existing authorized proxy session, and replies
also require matching Origin and the operator-bound CSRF token. Slack tokens
are never exposed to the browser. The synthetic demo supports discussion loading
and replies without contacting Slack.

## Unmerged bot PRs

The separate **Unmerged PRs** view lists open PRs in `openshift/release` from exact
configured bot logins, including drafts and older PRs outside the Slack history
window. Defaults are `openshift-bot` and `openshift-ci-robot`; Chai bot PRs are
excluded from the default watch list. The Chai reports view is independent.
Override with `--unmerged-pr-authors=login1,login2`, or use an empty value to disable
the view. It is read-only and never merges PRs or posts GitHub comments.

One server cache refreshes every five minutes, independent of browser requests.
A complete paginated search supplies title, link, author, created/updated time,
draft and labels. Incomplete or over-1,000-result searches fail explicitly; narrow
the configured authors rather than treating a partial list as complete. Failures
retain the last complete list with a stale indicator and last-sync timestamp.
No per-PR CI or review queries are issued. GitHub rate limits and retry deadlines
are respected independently of Slack. Implausible header-based delays are capped
at six hours so a malformed response cannot suspend polling indefinitely.

Public GitHub reads work anonymously. `--github-token-file` optionally supplies a
server-only token with the access needed to read this public repository when a
higher API quota is needed. It is not the Slack token and is never returned in
snapshots or logs. Demo mode supplies synthetic PR data and makes no GitHub calls.

## Core-ci metrics

The optional `monitoring` configuration targets deployment on core-ci. Its query
URL is the internal Thanos Query frontend already used by core-ci Grafana:

```yaml
monitoring:
  enabled: true
  rules_namespace: ci-monitoring
  query_url: http://thanos-query-frontend-ci-query.ci-monitoring.svc.cluster.local:9090
```

Use the pod's in-cluster ServiceAccount credentials and CA to read PrometheusRule
definitions in `ci-monitoring`. `examples/monitoring-rbac.yaml` grants only
namespace-scoped `get` and `list` on those rules; adapt its ServiceAccount namespace
to the Deployment. Kubernetes credentials stay on the Kubernetes API client.
The internal Thanos query request uses a separate client, matching Grafana's
existing unauthenticated service connection. Allow network access to that service
and the Kubernetes API. No permissions or cluster resources are applied by the
dashboard itself.

For a recognized Alertmanager occurrence, expand **Core-ci metrics** to inspect
that source explicitly. The server matches an exact alert name against the
configured namespace's rule definitions, then queries its expression unchanged.
It rejects ambiguous rules and does not accept browser-supplied PromQL or URLs.
Build-cluster alerts must not be assumed to originate on core-ci merely because
they share a name or Slack channel.

The graph uses a bounded two-hour window and shows the exact expression, rule
resource, `for` duration, source and time range. Historical occurrences use a
window around the notification rather than silently substituting today's data.
Alert expressions can return no series when a condition does not match. Missing
samples remain gaps; empty results do not establish health or live firing state.
Kubernetes rule definitions do not provide the evaluator's current firing state.

Reads happen on demand, with explicit refresh/retry and bounded response sizes.
Monitoring failures leave the alert feed, SOPs and workflow actions usable.
Disable `monitoring.enabled` to run without cluster monitoring access. Demo mode
uses synthetic rule/graph data and never reads cluster credentials or metrics.

## Actions and persistence

The UI calls acknowledgement **WIP** and shows the operator. Reading an alert is
personal and does not acknowledge it. Each WIP, Snooze and Close dialog offers
**This occurrence** or **Matching identifiers**. Regex previews use the server's
Go regexp engine, show up to 200 current matching occurrences, and restrict scope
to the selected channel and producer. Patterns match parsed source identifiers,
not arbitrary message text. Regex requires a parsed identifier; individual actions
do not. A stale revision is rejected before saving.

The checkbox toolbar offers **Close selected**, **Snooze selected**, and
**WIP selected**. Their dialogs default to exactly the checked occurrences. Switch
to **Matching identifier regex** to edit a pattern for each selected source and
channel. Each server preview includes other current matches in that source and
channel, even outside the selection or current view. WIP and Snooze save one rule
per source/channel and also cover future matches until the next **06:00 UTC**;
Close freezes only the listed current matches. Regex is unavailable when a selected
source/channel has no parsed identifier to anchor its pattern; exact selection
still works. All selected actions retain their requests and progress for explicit
recovery and preserve the current view and filters.

Individual WIP stays assigned until released or closed. Regex WIP covers current
and future matches until the next **06:00 UTC**; unfinished rule-owned occurrences
then return to Feed. Explicit individual actions override older family assignment.
The UI can release an individual occurrence or cancel its whole WIP rule.

Individual Snooze covers only that occurrence. Regex Snooze covers current and
future identifier matches in the same scope. Both expire at the next **06:00 UTC**,
shown in browser-local time. Repeated firings do not extend the deadline. Unsnooze
cancels the displayed rule; another overlapping rule may still apply. Expiry
restores prior workflow, and closed investigations remain closed. Slack reporting
and paging continue.

Close affects individual occurrences permanently until reopened. Regex Close
freezes the current preview's IDs and revisions and sends individual close actions
sequentially. It never creates a rule for future firings. Partial progress remains
visible and the browser retains the original plan and request IDs. Conflict,
rejection or uncertain delivery stops the batch; resuming or discarding the
remaining plan is explicit and never undoes already confirmed actions.

Each accepted mutation saves a short readable thread reply, with the complete
versioned action record in Slack message metadata (`ops_alert_action_v1`, payload
field `record`). Request ID, actor/time, target, rule scope and deadline, and
complete workflow state remain durable without printing JSON in the conversation.
Existing `OPS_ALERT_ACTION_V1` text records are still read. Only replies from the
verified writer identity are replayed as actions; malformed metadata, conflicting
copies, or a dashboard receipt missing its record pause actions rather than
guessing state. An action is successful only after a targeted readback returns the
exact saved metadata. Failed or rate-limited confirmation leaves delivery uncertain,
without repeating the write or sleeping in a confirmation retry loop. The visible reactions
are decoration; removing one does not undo an action. Use the dashboard's explicit
Undo/Unsnooze controls. Expired snooze decoration for visible occurrences is
reconciled on the next scan. Untouched feed messages without status reactions
require no reaction writes. Startup reconciliation uses the status reactions
already returned with channel history; subsequent scans retain successful icon
changes, including partial transitions. Removal affects only the token owner's
reaction, preserving other users' reactions.

A reaction rate limit stops the optional reaction batch until Slack's
`Retry-After` deadline without delaying history reads or durable action writes.
If Slack rejects a reaction with `missing_scope`, optional reactions are disabled
until the process restarts; the dashboard feed and workflow actions continue.
After granting `reactions:write` and reinstalling the Slack app if needed, restart
the dashboard to resume decoration.

Before deploying metadata writes, verify posting and reading metadata with the
actual writer and history tokens in an operator-approved test thread. Metadata can
be ignored with a Slack schema warning even when posting succeeds, so a timestamp
alone is insufficient. Preserve the existing history/replies permissions and
`include_all_metadata` reads. A rejected metadata payload does not fall back to a
visible JSON write. New writes enforce a conservative 3,000-byte application budget
on the serialized metadata envelope, including string escaping. This is not a claim
about Slack's undocumented server limit. Oversized durable records, including a large
retained investigation question, are rejected before posting and leave workflow state
unchanged. Shorten oversized questions or records; there is no visible-JSON fallback.
Chai response text is reconstructed from trusted thread replies for display and is
not copied into action metadata, so long responses do not block workflow actions.
Slack metadata is workspace-readable, not a place for secrets.
Once metadata records exist, rollback requires a metadata-compatible image; older
binaries can ignore those records and must not be used while they are retained.

The browser submits the selected revision and a stable request ID. Stale changes
are rejected; confirmed duplicate requests are reconciled without a second post.
Unconfirmed requests are retained in per-user browser session storage across
reloads, without storing credentials or CSRF tokens. Retry is always explicit.
On uncertain delivery, check the thread and retry the **same** retained request.
Do not create a new request to work around uncertainty. Unconfirmed retry guards
are in memory. An unconfirmed WIP family-rule cancellation pauses new actions
across occurrences until that cancellation is reconciled, because later actions
must not rely on a rule that might already be canceled. After a restart, recovery
can prove only records retained by Slack.
If its occurrence leaves the history window, the browser offers an explicit
Dismiss expired local retry control. It clears only this browser's retry guard;
delivery remains unconfirmed and the operator must check the original thread.

Ask Chai opens a floating, non-modal conversation window. The dashboard remains
interactive behind it. Minimize and reopen the window to retain its selected
conversation and draft, or choose another conversation from the last 48 hours.
New starts a shared Slack parent in the configured operations channel; asking
about an alert uses that alert's existing thread. These conversations are visible
to channel members, not private chats. They do not acknowledge, snooze or close
alerts and standalone conversation parents do not contribute to alert counts.

The question is editable, with the short default "Analyze this alert: likely
cause, evidence, and next read-only checks." for an alert. Only pressing Send
posts a message. Blank questions cannot be sent. The input limit is 4,000 UTF-8
bytes, matching server validation. Every operator message begins with the
configured Chai Slack user mention, displayed as `@chai-bot` in the composer.
The server uses the configured user ID, never a browser-supplied identity or
display-name lookup. DPTP-bot remains the posting identity. Chai is requested to
investigate; delivery alone does not prove it executed, and this UI does not grant
Chai new capabilities.

Slack messages contain only the mention and operator's question. A standalone
parent additionally ends with the small `💬 Dashboard · Chai` label. Request IDs,
operator attribution, timestamps and thread associations stay in versioned Slack
message metadata (`ops_dashboard_conversation_v1`), never serialized into message
text. The question remains in the message text; its hash in metadata verifies the
saved record. Metadata is workspace-readable and must not contain credentials.
The popup reconstructs confirmed dashboard requests, Chai replies and human
thread replies after restart, including earlier alert investigation requests.
Supported Markdown is rendered safely and timestamps use the browser's local time.

While the popup is open in a visible tab, it checks the shared conversation cache
every five seconds. A bounded background worker refreshes interested threads at
most once per ten seconds each, with one Slack thread read at a time. Interest
expires after 30 seconds and is limited to eight threads; rate limits postpone
reads. Open alert discussions share this targeted-read budget and thread cache.
Full channel discovery continues at `--sync-interval` (one minute by
default). This is polling with a short delay, not a streaming connection or a
guarantee of a fixed response time. Minimized and hidden tabs stop chat polling.

An immutable request ID is retained in per-operator session storage before a
write. An ambiguous response preserves that request for explicit reconciliation
and retry; it never automatically sends a replacement. Identity changes clear
private display state. Writes require authenticated proxy identity, same-origin
CSRF protection, complete archive coverage and an explicitly configured Chai user.
Failed or missing metadata confirmation remains unconfirmed rather than falling
back to a visible JSON record. Older clients' `/api/actions` investigation requests
remain compatible with their existing workflow receipts.

An unreadable dashboard chat request is quarantined instead of failing alert
workflow recovery. Valid retained conversations remain readable, and acknowledge,
snooze and close actions continue when their own archive coverage is complete.
Chat sending pauses with a visible recovery warning because missing or damaged
metadata cannot safely establish whether a question was already delivered. The
pause is reconstructed from retained Slack records after a restart, and existing
uncertain requests keep their delivery guards. Correcting the record or its
departure from the retained history window allows recovery on the next complete
sync. Malformed authoritative workflow receipts still fail workflow recovery.

## Recovery and operation

The dashboard shows a rolling **48-hour** window by default. Change it with
`--history-window`, for example `--history-window=72h`. The lower boundary moves
with the clock, including across restarts; older occurrences and Chai reports
leave all dashboard views while their original messages remain in Slack.
`slack.cutover` is optional and can set an earliest go-live date within that window.

Recovery reads up to 24 additional hours of parent messages to preserve active
daily Snooze and WIP rules whose original occurrence has left the visible window. With the default,
the scan covers at most 72 hours. Buffer occurrences are hidden and cannot receive
new actions. Current-day rules still expire at the next 06:00 UTC reset.
Reconciliation rescans this bounded history (default every minute), rereading
threads when reply counts or latest-reply timestamps change. Browser refreshes
of the main dashboard snapshot read memory every 15 seconds, not Slack. Open
Slack discussions additionally request targeted Slack refreshes as described
above. Config changes require a process restart.

Incomplete replay or malformed trusted action records expose Catching up/Degraded
coverage and pause actions. Verify channel retention and full rebuild time before
rollout. Deleted or expired Slack records cannot be recovered by this service.
The channel is the persistence boundary, so retention must cover the history
window and replay buffer (72 hours with the default settings).

`/healthz` checks process liveness; `/readyz` succeeds only with complete archive
coverage. Other routes require authenticated operator access. No production
deployment or live Slack smoke test is performed by the demo.

## Development

```sh
go test ./internal/opsalerts ./cmd/ops-alert-dashboard
go vet ./internal/opsalerts ./cmd/ops-alert-dashboard
make test
make lint
```

The focused table-driven tests cover producer/classification trust, occurrence
identity, scoped daily-reset WIP/Snooze and replay, stale/idempotent/uncertain
writes, Chai thread binding, HTTP authorization/CSRF, Slack pagination, and complete
GitHub search caching. Browser logic unit cases run with Node when available and
cover personal unread state, notification deduplication, safe formatting, alert
name grouping, on-demand Slack discussions, and explicit recovery of partial
close batches. Tests use fixed
clocks and in-memory transports. There is no integration or browser test
framework.

Scoped workflow fields extend durable v1 records. New versions still read old
records, but older binaries reject scoped fields during strict replay. Keep a
compatible image for rollback while scoped records remain in retained history;
do not delete shared action records to force an older writer to start.
