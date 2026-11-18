'use strict';
(() => {
  const views = {feed: ['▤', 'Feed', 'Occurrences needing triage'], acknowledged: ['✓', 'WIP', 'Occurrences an operator is investigating'], snoozed: ['◷', 'Snoozed', 'Snoozed occurrences and matching rules with a fixed daily return'], closed: ['☑', 'Closed investigations', 'Completed investigations; source health is unchanged'], unmerged_prs: ['⑂', 'Unmerged PRs', 'Open pull requests from configured public repository bots'], chai_reports: ['✧', 'Chai reports', 'Scheduled findings, grouped by task with latest and history']};
  const categories = {cluster_fleet: 'Cluster fleet', cloud_cleanup: 'Cloud cleanup', ci_platform: 'CI platform', repo_automation: 'Repo automation', other: 'Other'};
  const el = id => document.getElementById(id);
  const state = {snapshot: null, view: 'feed', category: '', selected: '', selectionVersion: 0, pendingSelection: null, pendingTargets: [], batchTargets: new Map(), checked: new Set(), selectedReport: '', reportDisclosures: new Set(), grouped: false, groupDisclosures: new Set(), query: '', color: '', fetching: false, sending: false, pending: null, pendingUnconfirmed: false, pendingUser: '', storageUser: null, storageError: '', dialogOccurrence: null, dialogTargets: [], dialogGroups: [], previewGroups: [], dialogKind: '', lastFocus: null, fetchError: '', authPaused: false, personal: null, personalUser: '', personalError: '', wasReady: false, previewVersion: 0, previewTargets: [], previewValid: false, dialogScope: 'occurrence', batch: null, prs: null, prsLoading: false, generation: 0};
  const desktopNotifications = new Set();
  const sopCache = new Map();
  const sopDisclosures = new Set();
  const node = (tag, text, className) => { const n = document.createElement(tag); if (text !== undefined && text !== null) n.textContent = String(text); if (className) n.className = className; return n; };
  const append = (parent, ...children) => { children.filter(Boolean).forEach(child => parent.append(child)); return parent; };
  const button = (label, handler, className = 'btn') => { const b = node('button', label, className); b.type = 'button'; b.addEventListener('click', handler); return b; };
  const text = value => String(value || '');
  /** Use the browser's timezone rules so daylight-saving changes need no manual offset. */
  const date = value => { const d = new Date(value); return Number.isFinite(d.getTime()) ? new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'short'}).format(d) : 'Not available'; };
  const chaiChat = window.OpsChaiChat?.create({root: el('chai-chat-root'), renderBody: chatBody, formatDate: date, request: chatRequest, onAuthError: chatAuthenticationError});
  const slackThread = window.OpsSlackThread?.create({root: el('alert-thread-root'), request: threadRequest, formatDate: date, channelName: () => channelName(), usergroups: () => state.snapshot?.usergroups || {}, onAuthError: chatAuthenticationError});
  /** Reuse the safe report renderer for conversational Markdown without interpreting HTML. */
  function chatBody(source, reference) { const body = node('div', null, 'report-body'); renderReportBody(body, source, reference); return body; }
  /** Keep chat writes on the same authenticated origin and server-derived actor as actions. */
  async function chatRequest(method, body, signal) {
    const generation = state.generation;
    const headers = {Accept: 'application/json'};
    const options = {method, credentials: 'same-origin', cache: 'no-store', signal, headers};
    if (method === 'POST') { headers['Content-Type'] = 'application/json'; headers['X-CSRF-Token'] = state.snapshot?.csrf_token || ''; options.body = JSON.stringify(body); }
    const active = method === 'GET' && body?.conversation_id ? '?active=' + encodeURIComponent(body.conversation_id) : '';
    const response = await fetch('/api/conversations' + active, options);
    const json = (response.headers.get('content-type') || '').includes('application/json');
    if (generation !== state.generation || response.redirected || !json || response.status === 401 || response.status === 403) return {ok: false, status: 401, body: {error: 'Your authenticated session could not be verified.'}};
    return {ok: response.ok, status: response.status, body: await response.json(), retryAfter: response.headers.get('retry-after')};
  }
  /** Keep original-thread reads and writes on the authenticated origin with the same session checks as chat. */
  async function threadRequest(method, body, signal) {
    const generation = state.generation;
    const headers = {Accept: 'application/json'};
    const options = {method, credentials: 'same-origin', cache: 'no-store', signal, headers};
    if (method === 'POST') { headers['Content-Type'] = 'application/json'; headers['X-CSRF-Token'] = state.snapshot?.csrf_token || ''; options.body = JSON.stringify(body); }
    const query = method === 'GET' && body?.occurrence_id ? '?occurrence_id=' + encodeURIComponent(body.occurrence_id) + (body.before ? '&before=' + encodeURIComponent(body.before) : '') + (body.refresh === true ? '&refresh=true' : '') : '';
    const response = await fetch('/api/alert-thread' + query, options);
    const json = (response.headers.get('content-type') || '').includes('application/json');
    if (generation !== state.generation || response.redirected || !json || response.status === 401 || response.status === 403) return {ok: false, status: 401, body: {error: 'Your authenticated session could not be verified.'}};
    return {ok: response.ok, status: response.status, body: await response.json(), retryAfter: response.headers.get('retry-after')};
  }
  /** Remove private chat content when the authorizing proxy no longer confirms the session. */
  function chatAuthenticationError() { pauseAuthentication('Your authenticated session could not be verified. Sign in again, then refresh.'); render(); }
  /** localUTC formats validated explicit UTC timestamps; clock-only values need a source UTC date. */
  function localUTC(value, reference) {
    const full = value.match(/^(\d{4}-\d{2}-\d{2})[T ](\d{1,2}):(\d{2})(?::(\d{2})(\.\d{1,9})?)?(?:Z|\+00:?00| UTC)$/);
    const clock = value.match(/^(\d{1,2}):(\d{2})(?::(\d{2}))? UTC$/);
    let day, hour, minute, second, fraction = '';
    if (full) [, day, hour, minute, second, fraction = ''] = full;
    else if (clock && reference !== undefined && reference !== null && reference !== '') {
      const anchor = new Date(reference);
      if (!Number.isFinite(anchor.getTime())) return value;
      day = anchor.toISOString().slice(0, 10); [, hour, minute, second] = clock;
    } else return value;
    if (Number(hour) > 23 || Number(minute) > 59 || Number(second || 0) > 59) return value;
    const instant = new Date(day + 'T' + hour.padStart(2, '0') + ':' + minute + ':' + (second || '00') + fraction + 'Z');
    if (!Number.isFinite(instant.getTime()) || instant.toISOString().slice(0, 10) !== day) return value;
    const minutes = -instant.getTimezoneOffset(), absolute = Math.abs(minutes);
    const offset = minutes ? 'GMT' + (minutes > 0 ? '+' : '-') + Math.floor(absolute / 60) + (absolute % 60 ? ':' + String(absolute % 60).padStart(2, '0') : '') : 'GMT';
    return new Intl.DateTimeFormat(undefined, {year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', ...(second ? {second: '2-digit'} : {})}).format(instant) + ' ' + offset;
  }
  /** localProse converts only UTC prose, leaving URLs, markup links and code literal. */
  function localProse(source, reference) {
    const tokens = /```[\s\S]*?(?:```|$)|~~~[\s\S]*?(?:~~~|$)|`+[^`\n]+`+|<[^>\n]+>|!?\[[^\]\n]*\]\([^\s)]+\)|[A-Za-z][A-Za-z0-9+.-]*:\/\/[^\s<>]+|\b\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?(?:Z|\+00:?00)| \d{1,2}:\d{2}(?::\d{2})? UTC)\b(?![+-])|\b\d{1,2}:\d{2}(?::\d{2})? UTC\b(?![+-])/g;
    return text(source).replace(tokens, token => localUTC(token, reference));
  }
  const items = () => state.snapshot?.occurrences || [];
  /** Keep writes disabled until identity, coverage and retained delivery guards agree. */
  const ready = () => !state.authPaused && (!state.batch || state.batch.index === state.batch.requests.length) && state.snapshot?.status?.phase === 'ready' && !state.fetchError && !state.sending && !state.pending && !state.storageError && state.storageUser === state.snapshot?.user;
  const id = occurrence => occurrence.identity.id;
  const workflow = occurrence => occurrence.state || {};
  /** Prefer parsed identifiers while retaining the complete source text in the inspector. */
  const displayTitle = occurrence => {
    const identifiers = occurrence.message.identifiers || [];
    const sourceTitle = text(occurrence.message.title).replace(/\[\s*ops_color=[^\]]*\]/gi, '');
    const title = plainSlack(sourceTitle).replace(/\s+/g, ' ').trim();
    const prefix = title.startsWith('🚨') ? '🚨 ' : '';
    if (identifiers.length) return prefix + (prefix ? identifiers.join(' · ').replace(/^(?:🚨\s*)+/, '') : identifiers.join(' · '));
    return plainSlack(localProse(sourceTitle, occurrence.message.reported_at)).replace(/\s+/g, ' ').trim() || 'Untitled occurrence';
  };
  /** Keep useful identifier evidence without repeating a cleaned title. */
  function identifierSubtitle(title, identifiers = []) {
    const label = identifiers.join(' · ');
    return label && label.replace(/^(?:🚨\s*)+/, '') !== title.replace(/^(?:🚨\s*)+/, '') ? node('span', label, 'identifier') : null;
  }
  /** Reserve the red badge for alerts that still need personal attention. */
  function feedUnreadBadge(count) {
    if (!count) return null;
    const counter = node('span', count, 'count unread-count');
    counter.setAttribute('aria-hidden', 'true'); return counter;
  }
  const outstandingInvestigation = occurrence => ['sent', 'delivery_uncertain'].includes(occurrence.investigation?.status);
  const sorted = occurrences => [...occurrences].sort((a, b) => Date.parse(b.message.reported_at) - Date.parse(a.message.reported_at));
  const isReport = occurrence => occurrence.view === 'chai_reports' || occurrence.classification.category === 'chai_reports';
  const matchesSearch = occurrence => !state.query || [occurrence.message.title, occurrence.message.text, occurrence.message.source, ...(occurrence.message.identifiers || [])].join(' ').toLowerCase().includes(state.query.toLowerCase());
  const visible = () => sorted(items().filter(o => (state.view === 'chai_reports' ? isReport(o) : !isReport(o) && o.view === state.view) && (state.view === 'chai_reports' || ((!state.category || o.classification.category === state.category) && (!state.color || o.classification.color === state.color))) && matchesSearch(o)));
  const openAlert = o => !isReport(o) && o.view !== 'closed' && !workflow(o).closed_by && !workflow(o).closed_at;
  /** Selection belongs only to the currently visible open alerts. */
  function pruneSelection() { const allowed = new Set(visible().filter(openAlert).map(id)); for (const value of state.checked) if (!allowed.has(value)) state.checked.delete(value); }
  function selectionChanged() { state.selectionVersion++; state.pendingSelection = null; }
  /** Preserve the operator's queue and move only the inspector that initiated this confirmed action. */
  function actionSelection(request) {
    const planned = state.batch?.requests[state.batch.index]?.request_id === request.request_id;
    const affected = planned ? state.batchTargets.get(request.occurrence_id) || [request.occurrence_id] : state.pendingTargets.length ? state.pendingTargets : [request.occurrence_id];
    const order = visible().map(id), selected = state.selected;
    if (!selected || !affected.includes(selected) || !order.includes(selected)) return null;
    return {selected, order, skip: planned ? [...state.batchTargets.values()].flat().concat(state.batch.requests.map(request => request.occurrence_id)) : affected, version: state.selectionVersion, generation: state.generation, scope: JSON.stringify([state.view, state.query, state.category, state.color])};
  }
  function advanceSelection(context) {
    if (!context || state.pendingSelection !== context) return;
    state.pendingSelection = null;
    if (context.version !== state.selectionVersion || context.generation !== state.generation || context.scope !== JSON.stringify([state.view, state.query, state.category, state.color]) || (state.selected && state.selected !== context.selected)) return;
    const remaining = new Set(visible().map(id)), index = context.order.indexOf(context.selected), skip = new Set(context.skip);
    state.selected = context.order.slice(index + 1).concat(context.order.slice(0, index).reverse()).find(candidate => remaining.has(candidate) && !skip.has(candidate)) || '';
  }
  /** Reject unsafe source links before they can become interactive DOM nodes. */
  function safeURL(value) {
    if (typeof value !== 'string' || /[\u0000-\u0020\\]/.test(value)) return null;
    try { const parsed = new URL(value); return parsed.protocol === 'https:' && !parsed.username && !parsed.password ? parsed : null; } catch { return null; }
  }
  /** Keep external evidence links isolated from the dashboard window. */
  function link(url, label) {
    const parsed = safeURL(url); if (!parsed) return null;
    const a = node('a', label); a.href = parsed.href; a.target = '_blank'; a.rel = 'noopener noreferrer'; return a;
  }
  const emoji = {rotating_light: '🚨', warning: '⚠️', eyes: '👀', zzz: '💤', white_check_mark: '✅', heavy_check_mark: '✔️', fire: '🔥', x: '❌', red_circle: '🔴', large_orange_circle: '🟠', large_yellow_circle: '🟡', information_source: 'ℹ️', rocket: '🚀', tada: '🎉', thumbsup: '👍', '+1': '👍', thumbsdown: '👎', '-1': '👎', hourglass_flowing_sand: '⏳', construction: '🚧', bell: '🔔', no_entry: '⛔', heart: '❤️', pray: '🙏', smile: '😄', bug: '🐛', robot_face: '🤖'};
  const decodeSlack = value => text(value).replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
  const emojify = value => value.replace(/:([a-z0-9_+-]+):/g, (match, name) => emoji[name] || match);
  /** Hoisted declaration: the Slack thread controller renders once while being created, before later const helpers in this script are initialized. */
  function channelName() { return state.snapshot?.channel_name || 'Operations channel'; }
  /** Produce readable title text without interpreting source markup as HTML. */
  function plainSlack(value) {
    return emojify(decodeSlack(value)).replace(/<([^>|]+)\|([^>]+)>/g, '$2').replace(/<(https:\/\/[^>]+)>/g, '$1').replace(/[*_~`]/g, '');
  }
  /** Render only supported Slack tokens as safe DOM nodes with bounded nesting. */
  function slackInline(container, source, reference, depth = 0) {
    if (depth > 8) { container.append(node('span', localProse(emojify(decodeSlack(source)), reference))); return; }
    const tokens = /(`)([^`\n]+)`|<([^>\n]+)>|([*_~])([^\n]+?)\4|https:\/\/[^\s<>]+|:[a-z_+-][a-z0-9_+-]*:/g;
    let cursor = 0;
    for (const match of source.matchAll(tokens)) {
      container.append(node('span', localProse(emojify(decodeSlack(source.slice(cursor, match.index))), reference)));
      if (match[1]) container.append(node('code', decodeSlack(match[2])));
      else if (match[3]) {
        const [target, supplied] = match[3].split('|');
        const group = target.match(/^!subteam\^([A-Z0-9]+)$/);
        const channel = target.match(/^#([A-Z0-9]+)$/);
        const label = group ? supplied || state.snapshot?.usergroups?.[group[1]] || 'User group' : channel ? supplied || channelName() : supplied || target;
        const anchor = link(decodeSlack(target), localProse(emojify(decodeSlack(label)), reference));
        container.append(anchor || node('span', group ? '@' + label.replace(/^@/, '') : channel ? '#' + label.replace(/^#/, '') : localProse(decodeSlack(label), reference)));
      } else if (match[4]) {
        const tag = {'*': 'strong', '_': 'em', '~': 's'}[match[4]], wrapper = node(tag);
        slackInline(wrapper, match[5], reference, depth + 1); container.append(wrapper);
      } else container.append(link(decodeSlack(match[0]), decodeSlack(match[0])) || node('span', emojify(match[0])));
      cursor = match.index + match[0].length;
    }
    container.append(node('span', localProse(emojify(decodeSlack(source.slice(cursor))), reference)));
  }
  /** Preserve readable message structure while keeping code and evidence inert. */
  function slackBody(source, reference) {
    const body = node('div', null, 'slack-body');
    const parts = text(source).split(/```/);
    parts.forEach((part, index) => {
      if (index % 2) { body.append(append(node('pre'), node('code', decodeSlack(part.replace(/^\n/, ''))))); return; }
      let list = null;
      for (const line of part.split('\n')) {
        const bullet = line.match(/^\s*(?:[•*-]|\d+[.)])\s+(.+)$/);
        if (bullet) {
          if (!list) { list = node('ul'); body.append(list); }
          const item = node('li'); slackInline(item, bullet[1], reference); list.append(item); continue;
        }
        list = null;
        const quote = line.match(/^&gt;\s?(.*)$|^>\s?(.*)$/);
        const row = node(quote ? 'blockquote' : 'div'); slackInline(row, quote ? quote[1] ?? quote[2] : line, reference);
        if (!line) row.append(node('br')); body.append(row);
      }
    });
    return body;
  }
  /** Require an explicit source envelope rather than inferring health from prose. */
  function sourceStatus(o) {
    const match = text(o.message.title).trim().match(/^\[(FIRING|RESOLVED)(?::\d+)?\](?:\s|$)/i);
    return match ? match[1].toUpperCase() : '';
  }
  /** Expose recognized job links without trusting arbitrary source-provided hosts. */
  function prowLink(o) {
    if (!(o.message.identifiers || []).some(value => /^(periodic|pull|postsubmit|rehearse)-/.test(value))) return null;
    for (const match of text(o.message.text).matchAll(/https:\/\/[^\s<>|]+/g)) {
      const url = safeURL(decodeSlack(match[0]));
      if (url && /^(?:prow\.ci\.openshift\.org|prow\.k8s\.io)$/.test(url.hostname)) return link(url.href, '↗ Open Prow job');
    }
    return null;
  }
  function personalKey(user) { return 'ops-alert-dashboard.personal.v1.' + encodeURIComponent(user); }
  function defaultPersonal() { return {known: [], read: [], notifications: false, preferences: {}, last_complete_sync: ''}; }
  /** Merge newer tab baselines before pruning so stale tabs cannot erase unread arrivals. */
  function savePersonal(prune = false) {
    if (!state.personalUser || !state.personal) return;
    try {
      const stored = JSON.parse(localStorage.getItem(personalKey(state.personalUser)) || 'null');
      const localSync = Date.parse(state.personal.last_complete_sync), storedSync = Date.parse(stored?.last_complete_sync);
      const newerStored = Number.isFinite(storedSync) && (!Number.isFinite(localSync) || storedSync > localSync);
      const retained = new Set(state.personal.known);
      if ((!prune || newerStored) && Array.isArray(stored?.known) && (!Number.isFinite(localSync) || storedSync >= localSync)) {
        for (const value of stored.known) if (typeof value === 'string') retained.add(value);
      }
      state.personal.known = [...retained];
      if (Array.isArray(stored?.read)) state.personal.read = [...new Set([...state.personal.read, ...stored.read])].filter(value => retained.has(value));
      if (newerStored) state.personal.last_complete_sync = stored.last_complete_sync;
      localStorage.setItem(personalKey(state.personalUser), JSON.stringify(state.personal));
      const notifiedKey = personalKey(state.personalUser) + '.notified';
      const notified = JSON.parse(localStorage.getItem(notifiedKey) || '[]');
      if (Array.isArray(notified)) localStorage.setItem(notifiedKey, JSON.stringify(notified.filter(value => retained.has(value))));
    }
    catch { state.personalError = 'Personal settings cannot be saved in this browser. Shared actions still use their separate safety storage.'; }
  }
  /** Restore preferences only for the current authenticated identity. */
  function loadPersonal(user) {
    state.checked.clear(); state.personalUser = user; state.personalError = ''; state.personal = defaultPersonal(); state.wasReady = false;
    try {
      const saved = JSON.parse(localStorage.getItem(personalKey(user)) || 'null');
      if (saved && Array.isArray(saved.known) && Array.isArray(saved.read)) {
        state.personal = {known: saved.known.filter(v => typeof v === 'string'), read: saved.read.filter(v => typeof v === 'string'), notifications: saved.notifications === true, preferences: saved.preferences || {}, last_complete_sync: Number.isFinite(Date.parse(saved.last_complete_sync)) ? saved.last_complete_sync : ''};
      }
    } catch { state.personalError = 'Personal settings are unavailable in this browser.'; }
    const prefs = state.personal.preferences;
    state.view = Object.hasOwn(views, prefs.view) ? prefs.view : 'feed';
    state.category = Object.hasOwn(categories, prefs.category) ? prefs.category : '';
    state.color = ['red', 'orange', 'yellow'].includes(prefs.color) ? prefs.color : '';
    state.query = typeof prefs.query === 'string' ? prefs.query : '';
    el('search').value = state.query; el('color').value = state.color;
  }
  /** Keep presentation choices across reloads without changing shared workflow state. */
  function rememberPreferences() {
    if (!state.personal) return;
    state.personal.preferences = {view: state.view, category: state.category, color: state.color, query: state.query}; savePersonal();
  }
  // Grouping is a cosmetic browser-wide choice, so it lives in a cookie instead of the per-user personal storage.
  const groupingCookie = 'ops-alert-dashboard.group-by-name.v1';
  const groupingCookieMaxAge = 31536000;
  /** Read only a known grouping value; absent, malformed or blocked cookies mean the flat list. */
  function readGroupedPreference() {
    try {
      for (const part of String(document.cookie).split(';')) {
        const item = part.trim();
        if (item.startsWith(groupingCookie + '=')) return decodeURIComponent(item.slice(groupingCookie.length + 1)) === 'true';
      }
    } catch { /* A blocked cookie store never blocks rendering. */ }
    return false;
  }
  /** Persist the grouping choice for this browser; a blocked cookie never disables the toggle. */
  function writeGroupedPreference(grouped) {
    try {
      const secure = window.location?.protocol === 'https:' ? '; Secure' : '';
      document.cookie = groupingCookie + '=' + encodeURIComponent(grouped ? 'true' : 'false') + '; Max-Age=' + groupingCookieMaxAge + '; Path=/; SameSite=Lax' + secure;
    } catch { /* The choice still applies to this page without persistence. */ }
  }
  /** Apply the stored grouping choice once when the dashboard loads. */
  function loadGroupingPreference() { state.grouped = readGroupedPreference(); }
  /** Switch between the flat and grouped alert list, persisting only the cosmetic choice. */
  function toggleGrouping(grouped) {
    state.grouped = grouped;
    writeGroupedPreference(grouped);
    render();
  }
  loadGroupingPreference();
  /** Keep personal reading independent of another operator changing workflow state. */
  function unread(o) { return !isReport(o) && state.personal && state.personal.known.includes(id(o)) && !state.personal.read.includes(id(o)); }
  /** Record an explicit reading action without acknowledging the shared alert. */
  function markRead(o) { if (!state.personal || !unread(o)) return; state.personal.read.push(id(o)); savePersonal(); }
  /** Preserve unread history through incomplete coverage and suppress recovery notification bursts. */
  function reconcilePersonal(snapshot, previous, quiet) {
    if (state.personalUser !== snapshot.user) loadPersonal(snapshot.user);
    const known = new Set(state.personal.known), read = new Set(state.personal.read), current = new Set(snapshot.occurrences.map(id));
    const healthy = snapshot.status.phase === 'ready';
    const watermark = Date.parse(state.personal.last_complete_sync);
    const completeSync = Date.parse(snapshot.status.last_sync);
    const canPrune = healthy && Number.isFinite(completeSync) && (!Number.isFinite(watermark) || completeSync >= watermark);
    const mayNotify = state.wasReady && previous?.user === snapshot.user && previous?.status?.phase === 'ready';
    const arrivals = [];
    for (const o of snapshot.occurrences) {
      if (known.has(id(o))) continue;
      const fresh = Number.isFinite(watermark) && Date.parse(o.message.reported_at) > watermark && !isReport(o);
      // Wait for complete coverage before deciding whether newly seen messages are arrivals.
      if (fresh && !healthy) continue;
      if (!fresh) read.add(id(o));
      if (fresh && o.view === 'feed' && !o.snooze) arrivals.push(o);
      known.add(id(o));
    }
    state.personal.known = [...known].filter(value => !canPrune || current.has(value));
    state.personal.read = [...read].filter(value => !canPrune || current.has(value));
    if (healthy && Number.isFinite(Date.parse(snapshot.status.last_sync)) && (!Number.isFinite(watermark) || Date.parse(snapshot.status.last_sync) > watermark)) state.personal.last_complete_sync = snapshot.status.last_sync;
    state.wasReady = healthy; savePersonal(canPrune);
    if (!quiet && mayNotify && healthy && !state.authPaused && !state.fetchError && !state.personalError && Date.now() - Date.parse(snapshot.status.last_sync) < 120000 && state.personal.notifications && arrivals.length) notifyArrivals(arrivals, snapshot.user);
  }
  /** Serialize notification delivery across tabs and recheck identity after waiting for the lock. */
  async function notifyArrivals(arrivals, user) {
    if (!('Notification' in window) || Notification.permission !== 'granted' || !navigator.locks) return;
    await navigator.locks.request('ops-alert-dashboard-notifications', async () => {
      if (state.authPaused || state.snapshot?.user !== user || state.snapshot?.status?.phase !== 'ready' || !Number.isFinite(Date.parse(state.snapshot.status.last_sync)) || Date.now() - Date.parse(state.snapshot.status.last_sync) >= 120000 || !state.personal?.notifications) return;
      const key = personalKey(user) + '.notified';
      try {
        const prior = new Set(JSON.parse(localStorage.getItem(key) || '[]'));
        const fresh = arrivals.filter(o => !prior.has(id(o)));
        if (!fresh.length) return;
        for (const o of fresh) prior.add(id(o));
        const retained = new Set(items().map(id));
        localStorage.setItem(key, JSON.stringify([...prior].filter(value => retained.has(value))));
        const notification = new Notification(fresh.length === 1 ? displayTitle(fresh[0]) : fresh.length + ' new operations alerts', {body: fresh.length === 1 ? channelName() + ' · ' + fresh[0].message.source : channelName() + ' · Open Feed to triage', tag: 'ops-alert-dashboard-' + user});
        desktopNotifications.add(notification); notification.onclose = () => desktopNotifications.delete(notification);
        notification.onclick = () => { selectionChanged(); window.focus(); state.view = 'feed'; state.selected = id(fresh[0]); render(); notification.close(); };
      } catch { state.personalError = 'Desktop notification delivery or storage is unavailable.'; }
    });
  }
  /** Request browser permission only through an explicit operator gesture. */
  async function toggleNotifications() {
    if (!state.personal || state.authPaused) return;
    if (!('Notification' in window) || !navigator.locks) { announce('Desktop notifications are unavailable in this browser.'); return; }
    if (state.personal.notifications) { state.personal.notifications = false; savePersonal(); render(); return; }
    if (Notification.permission === 'denied') { announce('Notifications are blocked. Change this site’s notification permission in browser settings to enable them.'); return; }
    const user = state.personalUser;
    const permission = Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission();
    if (state.authPaused || state.personalUser !== user) return;
    state.personal.notifications = permission === 'granted'; savePersonal(); render();
    if (permission === 'denied') announce('Notifications are blocked. Change this site’s notification permission in browser settings to enable them.');
  }
  /** Remove notifications from the previous session when its identity is no longer verified. */
  function closeDesktopNotifications() { for (const notification of desktopNotifications) notification.close(); desktopNotifications.clear(); }
  /** Clear private display caches while preserving guarded requests for explicit recovery. */
  function pauseAuthentication(message) {
    closeDesktopNotifications();
    state.authPaused = true; state.wasReady = false; state.generation++; state.snapshot = null; state.prs = null; state.selected = ''; state.checked.clear(); state.selectedReport = '';
    sopCache.clear(); metricsCache.clear(); sopDisclosures.clear(); metricsDisclosures.clear(); metricsSelections.clear(); state.reportDisclosures.clear(); state.groupDisclosures.clear();
    if (el('action-dialog').open) closeDialog(); state.fetchError = message;
    document.title = 'Sign in · Test Platform Operations';
    chaiChat?.update(null);
    slackThread?.update(null);
  }
  /** Recognize proxy sign-in responses before treating their HTML as dashboard data. */
  async function responseJSON(response) {
    const json = (response.headers.get('content-type') || '').includes('application/json');
    if (response.redirected || !json || response.status === 401 || response.status === 403) {
      let message = 'Your authenticated session could not be verified. Sign in again, then refresh.';
      if (response.status === 403 && json && !response.redirected) message = 'Access was denied. Your account may lack access, or session/CSRF validation may have failed. Sign in again or contact an administrator.';
      pauseAuthentication(message); throw new Error(message);
    }
    return response.json();
  }
  /** Read the shared server cache without creating browser-driven GitHub polling. */
  async function refreshPRs() {
    if (state.authPaused || !state.snapshot || state.prsLoading) return;
    state.prsLoading = true; const generation = state.generation;
    try {
      const response = await fetch('/api/unmerged-prs', {credentials: 'same-origin', cache: 'no-store', headers: {Accept: 'application/json'}});
      const result = await responseJSON(response); if (generation !== state.generation) return;
      if (!response.ok) throw new Error(result.error || 'Pull requests could not be loaded.');
      state.prs = result;
    } catch (err) { if (!state.authPaused && generation === state.generation) state.prs = {phase: 'degraded', error: err.message, prs: []}; }
    finally { state.prsLoading = false; render(); }
  }
  /** Keep list freshness visible alongside the cached public pull-request evidence. */
  function renderPRs(container) {
    const data = state.prs;
    if (!data) { container.append(node('p', state.prsLoading ? 'Loading pull requests…' : 'Pull requests have not been loaded.', 'empty')); return; }
    container.append(node('p', data.enabled === false ? 'Public bot pull request tracking is not configured.' : (data.phase === 'ready' ? 'GitHub sync ready' : 'GitHub sync ' + (data.phase || 'unavailable')) + ' · Last sync: ' + date(data.last_sync) + (data.error ? ' · ' + data.error : ''), 'banner'));
    for (const pr of data.prs || []) {
      const card = node('article', null, 'pr-card');
      append(card, link(pr.url, '#' + pr.number + ' · ' + pr.title) || node('strong', pr.title), node('p', 'By ' + pr.author + ' · ' + Math.max(0, Math.floor((Date.now() - Date.parse(pr.created_at)) / 86400000)) + ' days old' + (pr.draft ? ' · Draft' : ''), 'fine'), node('p', 'Created ' + date(pr.created_at) + ' · Updated ' + date(pr.updated_at), 'fine'));
      const labels = node('div', null, 'states'); for (const label of pr.labels || []) labels.append(node('span', typeof label === 'string' ? label : label.name, 'workflow')); card.append(labels); container.append(card);
    }
    if (data.enabled && !(data.prs || []).length) container.append(node('p', 'No matching open bot pull requests.', 'empty'));
  }
  function badge(o) {
    if (isReport(o)) return node('span', 'Chai report', 'badge report-badge');
    const color = ['red', 'orange', 'yellow'].includes(o.classification.color) ? o.classification.color : 'orange';
    return node('span', color[0].toUpperCase() + color.slice(1), 'badge ' + color);
  }
  function announce(message) { el('outcome').textContent = message; }
  function error(message) { const shown = state.storageError || message; el('action-error').hidden = !shown; el('action-error').textContent = shown; }
  function pendingKey(user) { return 'ops-alert-dashboard.pending.v1.' + encodeURIComponent(user); }
  /** Identify local retry guards whose original occurrence has left retained coverage. */
  function pendingExpired() {
    const timestamp = state.pending?.occurrence_id?.match(/^[^:]+:(\d+)\.(\d{1,6})$/);
    const start = Date.parse(state.snapshot?.status?.cutover);
    if (!timestamp || !Number.isFinite(start)) return false;
    const reported = Number(timestamp[1]) * 1000 + Number((timestamp[2] + '000').slice(0, 3));
    return Number.isSafeInteger(reported) && reported < start && !items().some(o => id(o) === state.pending.occurrence_id);
  }
  /** Accept only the supported immutable request shape when recovering browser storage. */
  function validPending(request) {
    const fields = ['scope', 'occurrence_id', 'kind', 'expected_revision', 'request_id', 'pattern', 'question'];
    return request && typeof request === 'object' && !Array.isArray(request) && Object.keys(request).every(key => fields.includes(key) && typeof request[key] === 'string') && (!request.scope || ['occurrence', 'regex'].includes(request.scope)) && ['acknowledge', 'unacknowledge', 'close', 'reopen', 'snooze', 'unsnooze', 'investigate'].includes(request.kind) && !!request.occurrence_id && !!request.expected_revision && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(request.request_id);
  }
  /** Recover user-bound delivery guards without silently sending any stored request. */
  function restorePending(user) {
    if (state.sending || (state.storageUser === user && !state.storageError)) return;
    try {
      if (!user || typeof user !== 'string') throw new Error('Authenticated operator is unavailable.');
      const saved = sessionStorage.getItem(pendingKey(user));
      const retained = saved ? JSON.parse(saved) : null;
      if (retained && (retained.user !== user || !validPending(retained.request))) throw new Error('The retained request is invalid; review this browser session before writing.');
      state.storageUser = user; state.storageError = '';
      state.pending = retained?.request || null; state.pendingUser = retained ? user : '';
      // Reload cannot establish whether a retained attempt reached the server.
      state.pendingUnconfirmed = !!retained;
      const savedPlan = sessionStorage.getItem(batchKey(user));
      const plan = savedPlan ? JSON.parse(savedPlan) : null;
      if (plan && !validBatch(plan, user)) throw new Error('The retained action plan is invalid.');
      state.batch = plan; state.batchTargets = new Map(); state.pendingTargets = [];
    } catch (err) {
      state.storageError = 'Cannot restore retained action requests. Actions are paused to prevent duplicate writes. ' + (err.message || 'Session storage is unavailable.');
    }
    error('');
  }
  /** Persist a request before its first write so uncertain delivery can reuse the same identity. */
  function retainPending(user, request) {
    try {
      const saved = JSON.stringify({user, request});
      sessionStorage.setItem(pendingKey(user), saved);
      if (sessionStorage.getItem(pendingKey(user)) !== saved) throw new Error('Session storage did not retain the request.');
      return true;
    } catch {
      state.storageError = 'Cannot retain the action request in session storage. No write was started by this attempt. Allow session storage and refresh before trying again.';
      error(''); render(); return false;
    }
  }
  /** Keep actions paused if browser storage cannot remove a completed or explicitly dismissed guard. */
  function clearPending(user, request) {
    try {
      sessionStorage.removeItem(pendingKey(user));
      if (sessionStorage.getItem(pendingKey(user)) !== null) throw new Error('Retained request was not removed.');
      if (state.pending?.request_id === request.request_id && state.pendingUser === user) { state.pending = null; state.pendingUser = ''; state.pendingUnconfirmed = false; }
    } catch {
      state.storageError = 'This browser could not clear its retained request. Actions are paused. Restore session storage and refresh to reconcile the same request.';
      error('');
    }
  }
  /** Make abandoning an expired local guard explicit without asserting remote delivery. */
  function dismissExpired() {
    if (!pendingExpired() || state.sending || state.snapshot?.status?.phase !== 'ready' || state.fetchError || state.storageError || state.pendingUser !== state.snapshot?.user || state.storageUser !== state.snapshot?.user) return;
    clearPending(state.pendingUser, state.pending);
    if (!state.pending) { error(''); announce('Expired local retry dismissed. Delivery remains unconfirmed; check the original Slack thread. No Slack action was sent or undone.'); }
    render();
  }
  function fact(list, label, value) { append(list, append(node('div'), node('dt', label), node('dd', value))); }
  /** Separate archive coverage and authentication failures from shared workflow outcomes. */
  function renderStatus() {
    const coverage = state.snapshot?.status;
    const box = el('coverage');
    const phase = coverage?.phase || 'catching_up';
    box.className = 'banner ' + (state.fetchError || phase === 'degraded' ? 'error' : phase === 'ready' ? 'ready' : 'warning');
    box.replaceChildren(); box.textContent = state.fetchError || (phase === 'ready' ? 'Coverage ready' : phase === 'degraded' ? 'Coverage degraded · actions paused' : 'Catching up on retained Slack history · actions paused');
    if (coverage) box.textContent += ' · Last complete sync: ' + date(coverage.last_sync) + ' · Window starts: ' + date(coverage.cutover) + (coverage.error ? ' · ' + coverage.error : '');
    if (state.authPaused) { const signIn = node('a', 'Sign in again', 'btn'); signIn.href = '/oauth/start?rd=%2F'; box.append(document.createTextNode(' '), signIn); }
    el('personal-status').textContent = state.personalError;
    const denied = 'Notification' in window && Notification.permission === 'denied';
    el('notifications').textContent = denied ? 'Notifications blocked' : state.personal?.notifications ? 'Notifications on' : 'Enable notifications';
    el('notifications').disabled = !state.snapshot || state.authPaused;
    el('mark-read').disabled = !state.personal || !items().some(unread);
    el('demo').hidden = !state.snapshot?.demo;
    el('operator').textContent = state.snapshot?.user ? 'Operator · ' + state.snapshot.user : state.authPaused ? 'Sign in required' : 'Connecting…';
    el('refresh').disabled = state.fetching;
    el('retry-panel').hidden = !state.pending || state.sending;
    const expired = pendingExpired();
    el('retry-text').textContent = state.pending ? 'Delivery is unconfirmed. Request ' + state.pending.request_id + (expired ? ' belongs to an alert outside the history window. Check the original Slack thread. You can dismiss this local retry to continue; dismissal does not confirm or undo delivery.' : ' is retained. Check the thread or refresh, then retry this same request to reconcile it. No second request will be created.') : '';
    const blocked = state.snapshot?.status?.phase !== 'ready' || !!state.fetchError || state.sending || !!state.storageError || state.pendingUser !== state.snapshot?.user || state.storageUser !== state.snapshot?.user;
    el('retry').disabled = blocked || expired;
    el('dismiss-expired').hidden = !expired;
    el('dismiss-expired').disabled = blocked;
    if (state.storageError) error('');
  }
  /** Keep personal unread counts distinct from shared workflow totals. */
  function renderNav() {
    el('navigation').replaceChildren();
    for (const [key, view] of Object.entries(views)) {
      const b = button(view[0] + ' ' + view[1], () => { selectionChanged(); state.view = key; state.category = ''; state.selected = ''; error(''); rememberPreferences(); if (key === 'unmerged_prs') refreshPRs(); render(); }, key === 'chai_reports' ? 'report-nav' : '');
      if (state.view === key) b.setAttribute('aria-current', 'page');
      if (key === 'feed') {
        const feed = items().filter(o => !isReport(o) && o.view === 'feed'), count = feed.filter(unread).length;
        b.setAttribute('aria-label', 'Feed · ' + feed.length + ' total · ' + count + ' unread');
        append(b, node('span', feed.length, 'count'), feedUnreadBadge(count));
      }
      else if (!['chai_reports', 'unmerged_prs'].includes(key)) append(b, node('span', items().filter(o => !isReport(o) && o.view === key).length, 'count'));
      el('navigation').append(b);
    }
  }
  /** Show category counts within the current workflow view rather than across unrelated queues. */
  function renderFilters() {
    el('filters').replaceChildren();
    el('filters').hidden = ['chai_reports', 'unmerged_prs'].includes(state.view);
    el('color-label').hidden = ['chai_reports', 'unmerged_prs'].includes(state.view);
    el('grouping').hidden = ['chai_reports', 'unmerged_prs'].includes(state.view);
    el('search').closest('label').hidden = state.view === 'unmerged_prs';
    el('group-by-name').checked = state.grouped;
    for (const [key, label] of Object.entries(categories)) {
      const b = button('', () => { selectionChanged(); state.category = state.category === key ? '' : key; rememberPreferences(); render(); }, 'filter');
      b.setAttribute('aria-pressed', String(state.category === key));
      append(b, node('span', label), node('strong', items().filter(o => !isReport(o) && o.view === state.view && o.classification.category === key).length));
      el('filters').append(b);
    }
  }
  /** Refresh the visible snapshot without opening details or marking alerts as read. */
  function render() {
    renderStatus(); renderBatch(); renderNav(); renderFilters();
    if (!state.authPaused) { const count = items().filter(o => o.view === 'feed' && unread(o)).length; document.title = (count ? '(' + count + ') ' : '') + 'Test Platform · Operations'; }
    el('title').textContent = views[state.view][1]; el('subtitle').textContent = views[state.view][2];
    pruneSelection();
    const content = el('content'); rememberGroupDisclosures(content); content.replaceChildren();
    if (!state.snapshot) { content.append(node('p', state.authPaused ? 'Sign in again to reload your operations workspace. Pending action requests stay in this browser session and are never replayed automatically.' : 'Waiting for the first snapshot…', 'empty')); return; }
    if (state.view === 'unmerged_prs') { renderPRs(content); return; }
    const list = visible();
    if (state.view === 'chai_reports') { renderReports(content, sorted(items().filter(isReport))); return; }
    renderAlerts(content, list);
  }
  /** Grouping key: the first parsed identifier, or the readable title when none was parsed. */
  function alertGroupName(o) {
    return text((o.message.identifiers || [])[0]) || displayTitle(o);
  }
  /** Group the filtered occurrences by name in input order; a Map keeps hostile keys like __proto__ distinct. */
  function groupAlerts(list) {
    const groups = new Map();
    for (const o of list) {
      const name = alertGroupName(o);
      if (!groups.has(name)) groups.set(name, []);
      groups.get(name).push(o);
    }
    return groups;
  }
  /** One occurrence row: its optional selection checkbox plus the inspector card. */
  function alertRow(o) {
    const row = node('div', null, 'alert-row' + (o.classification.color ? ' ' + o.classification.color : ''));
    if (openAlert(o)) {
      const check = node('input'); check.id = 'select-alert-' + id(o); check.type = 'checkbox'; check.checked = state.checked.has(id(o)); check.setAttribute('aria-label', 'Select ' + displayTitle(o));
      check.addEventListener('change', () => { if (check.checked) state.checked.add(id(o)); else state.checked.delete(id(o)); render(); el(check.id)?.focus(); }); row.append(check);
    }
    const b = button('', () => { selectionChanged(); state.selected = state.selected === id(o) ? '' : id(o); if (state.selected) markRead(o); render(); el(state.selected ? 'inspector-close' : 'alert-card-' + id(o))?.focus(); }, 'alert-card ' + o.classification.color + (unread(o) ? ' unread' : ''));
    b.id = 'alert-card-' + id(o);
    b.setAttribute('aria-pressed', String(id(o) === state.selected));
    append(b, append(node('span', null, 'row-top'), badge(o), unread(o) ? node('span', 'NEW', 'new-indicator') : null, sourceStatus(o) ? node('span', sourceStatus(o), 'source-status') : null, node('small', categories[o.classification.category] || 'Other')), node('strong', displayTitle(o)), identifierSubtitle(displayTitle(o), o.message.identifiers), append(node('span', null, 'row-meta'), node('span', o.message.source), node('time', date(o.message.reported_at))));
    if (workflow(o).acknowledged_by) b.append(node('span', 'WIP · ' + workflow(o).acknowledged_by, 'identifier'));
    if (o.snooze) b.append(node('span', 'Snoozed until ' + date(o.snooze.until), 'identifier'));
    if (o.investigation) b.append(node('span', 'Chai · ' + statusLabel(o.investigation.status), 'identifier'));
    append(row, b);
    return row;
  }
  /** Collapse same-name occurrences behind one native disclosure while keeping every row and action. Groups with personally unread members render first; the stable sort keeps each section in the list's newest-first order. */
  function appendGroupedRows(cards, list) {
    const groups = [...groupAlerts(list)].map(([name, members]) => ({name, members, fresh: members.filter(unread).length}));
    groups.sort((left, right) => (right.fresh > 0) - (left.fresh > 0));
    for (const {name, members, fresh} of groups) {
      const group = node('details', null, 'alert-group');
      group.dataset.name = name;
      group.dataset.generation = state.generation;
      group.open = state.groupDisclosures.has(name);
      append(group, append(node('summary'), node('strong', name), node('span', members.length + ' occurrence' + (members.length === 1 ? '' : 's'), 'count'), fresh ? node('span', fresh + ' new', 'new-indicator') : null));
      const rows = node('div', null, 'group-items');
      for (const o of members) rows.append(alertRow(o));
      group.append(rows);
      cards.append(group);
    }
  }
  /** Snapshot live disclosures just before a rerender replaces the groups. Reading the nodes directly avoids toggle callbacks, which can fire after replacement or an identity reset, and the generation guard keeps a stale tree from reviving cleared state. */
  function rememberGroupDisclosures(content) {
    for (const group of content.querySelectorAll('.alert-group')) {
      if (Number(group.dataset.generation) !== state.generation) continue;
      if (group.open) state.groupDisclosures.add(group.dataset.name);
      else state.groupDisclosures.delete(group.dataset.name);
    }
  }
  /** Keep selection and inspection independent, with details opened only by an explicit click. */
  function renderAlerts(content, list) {
    pruneSelection();
    if (!list.some(o => id(o) === state.selected)) state.selected = '';
    const workspace = node('div', null, 'workspace' + (state.selected ? ' with-inspector' : ''));
    const listBox = node('section', null, 'list'); listBox.setAttribute('aria-label', 'Alert occurrences');
    append(listBox, append(node('div', null, 'list-head'), node('span', list.length + ' occurrence' + (list.length === 1 ? '' : 's')), node('span', state.grouped ? 'Groups with new alerts first' : 'Latest report first')));
    const selectable = list.filter(openAlert);
    if (selectable.length) {
      const toolbar = node('div', null, 'bulk-actions');
      const all = node('input'); all.id = 'select-all-alerts'; all.type = 'checkbox'; all.checked = state.checked.size === selectable.length; all.indeterminate = state.checked.size > 0 && !all.checked;
      all.setAttribute('aria-label', 'Select all matching open alerts');
      all.addEventListener('change', () => { state.checked = all.checked ? new Set(selectable.map(id)) : new Set(); render(); el('select-all-alerts')?.focus(); });
      const label = append(node('label'), all, node('span', 'Select all matching open alerts'));
      const clear = button('Clear selection', () => { state.checked.clear(); render(); }); clear.disabled = !state.checked.size;
      append(toolbar, label, clear);
      for (const [kind, title] of [['close', 'Close'], ['snooze', 'Snooze'], ['acknowledge', 'WIP']]) {
        const control = button(title + ' selected (' + state.checked.size + ')', () => openSelectedAction(kind));
        control.disabled = !state.checked.size || !ready(); toolbar.append(control);
      }
      listBox.append(toolbar);
    }
    const cards = node('div', null, 'list-items');
    if (state.grouped) appendGroupedRows(cards, list);
    else for (const o of list) cards.append(alertRow(o));
    if (!list.length) cards.append(node('p', 'No occurrences match this view and these filters.', 'empty'));
    listBox.append(cards);
    workspace.append(listBox);
    const selected = list.find(o => id(o) === state.selected);
    if (selected) {
      const detail = node('section', null, 'inspector'); detail.setAttribute('aria-label', 'Selected occurrence details');
      const close = button('Close details', () => { selectionChanged(); state.selected = ''; render(); el('alert-card-' + id(selected))?.focus(); }, 'btn inspector-close'); close.id = 'inspector-close';
      detail.append(close);
      renderDetail(detail, selected); workspace.append(detail);
    }
    content.append(workspace);
  }
  /** Bind each explicit action control to its displayed occurrence and selected scope. */
  function action(label, kind, occurrence, scope = 'occurrence') {
    const b = button(label, () => ['snooze', 'investigate', 'acknowledge', 'close'].includes(kind) ? openDialog(kind, occurrence) : sendNew(kind, occurrence, '', scope));
    b.disabled = !ready(); return b;
  }
  function statusLabel(status) { return ({sent: 'Sent · awaiting reply', reply_received: 'Reply received', delivery_uncertain: 'Delivery uncertain', failed: 'Delivery failed'})[status] || status; }
  /** Keep source evidence, operator workflow and family-rule ownership separately visible. */
  function renderDetail(container, o) {
    const s = workflow(o);
    append(container, node('p', 'Occurrence inspector · ' + (categories[o.classification.category] || 'Other'), 'eyebrow'), node('h2', displayTitle(o)), identifierSubtitle(displayTitle(o), o.message.identifiers), append(node('div', null, 'states'), badge(o), node('span', views[o.view]?.[1] || o.view, 'workflow'), sourceStatus(o) ? node('span', 'Source · ' + sourceStatus(o), 'source-status') : null));
    const facts = node('dl', null, 'facts');
    fact(facts, 'Source', o.message.source); fact(facts, 'Reported', date(o.message.reported_at)); fact(facts, 'Channel', channelName()); fact(facts, 'Classification', (categories[o.classification.category] || 'Other') + ' · ' + o.classification.origin);
    if (s.acknowledged_by) fact(facts, 'WIP', s.acknowledged_by + ' · ' + date(s.acknowledged_at));
    if (s.closed_by) fact(facts, 'Closed', s.closed_by + ' · ' + date(s.closed_at));
    container.append(facts);
    const slack = link(o.message.slack_url, '↗ Open source thread in Slack'); if (slack) container.append(slack); const prow = prowLink(o); if (prow) container.append(prow);
    const actions = node('div', null, 'actions');
    if (s.closed_by || s.closed_at) {
      actions.append(action('↶ Reopen investigation', 'reopen', o));
      if (o.snooze) actions.append(action(o.snooze.scope === 'occurrence' ? '◷ Unsnooze occurrence' : '◷ Unsnooze rule', 'unsnooze', o));
    }
    else {
      if (s.acknowledged_by) {
        actions.append(action('↶ Release this occurrence', 'unacknowledge', o, 'occurrence'));
        if (o.wip_rule) actions.append(action('↶ Cancel WIP rule', 'unacknowledge', o, 'regex'));
      } else actions.append(action('✓ Start work', 'acknowledge', o));
      if (o.snooze) actions.append(action(o.snooze.scope === 'occurrence' ? '◷ Unsnooze occurrence' : '◷ Unsnooze rule', 'unsnooze', o));
      else actions.append(action('◷ Snooze', 'snooze', o));
      actions.append(action('☑ Close investigation', 'close', o));
    }
    const ask = button('✧ Ask Chai', () => chaiChat ? chaiChat.open({occurrenceID: id(o), title: displayTitle(o)}) : openDialog('investigate', o)); ask.disabled = !state.snapshot || state.authPaused; actions.append(ask);
    // Loading the original discussion is isolated from reading state, selection and workflow actions.
    const discussion = button('💬 Load Slack conversation', () => slackThread?.open({occurrenceID: id(o), title: displayTitle(o), slackURL: o.message.slack_url})); discussion.id = 'thread-open-' + id(o); discussion.disabled = !state.snapshot || state.authPaused; actions.append(discussion); container.append(actions);
    container.append(node('p', ready() ? 'Actions are saved in this Slack thread before the dashboard confirms them.' : state.pending ? 'Resolve the unconfirmed request before creating another action.' : 'Actions are available when coverage is ready and the snapshot is current.', 'fine'));
    renderSOPs(container, o);
    renderMetrics(container, o);
    if (o.snooze) {
      const box = node('div', null, 'investigation');
      append(box, node('h3', o.snooze.scope === 'occurrence' ? '◷ Occurrence snoozed' : '◷ Active snooze rule'), node('p', o.snooze.pattern, 'evidence'), node('p', 'Scope: ' + channelName() + ' · source ' + o.snooze.source + ' · until ' + date(o.snooze.until), 'fine'), node('p', 'By ' + o.snooze.actor + '. Unsnooze cancels this scope. Expiry restores the previous workflow; repeated firings do not extend it.', 'fine')); container.append(box);
    }
    if (o.wip_rule) append(container, append(node('div', null, 'investigation'), node('h3', 'WIP · matching identifier rule'), node('p', o.wip_rule.pattern, 'evidence'), node('p', 'By ' + o.wip_rule.actor + ' · Until ' + date(o.wip_rule.until) + ' · ' + channelName() + ' · ' + o.wip_rule.source, 'fine')));
    if (o.investigation) {
      const investigation = o.investigation;
      const box = node('div', null, 'investigation');
      append(box, node('h3', '✧ Chai · ' + statusLabel(investigation.status)), node('p', investigation.question, 'evidence'), node('p', 'Requested by ' + investigation.requested_by + ' · ' + date(investigation.requested_at) + ' · ' + investigation.request_id, 'fine'));
      if (investigation.reply) append(box, node('h3', 'Chai reply'), slackBody(investigation.reply));
      container.append(box);
    }
    append(container, node('h3', 'Source evidence', 'section-head'), slackBody(o.message.text || 'No message text supplied.', o.message.reported_at), append(node('details', null, 'original-evidence'), node('summary', 'Original Slack text'), node('pre', o.message.text, 'evidence')), node('p', 'A resolved message does not close an investigation automatically.', 'fine'), node('h3', 'Thread evidence & shared history', 'section-head'));
    for (const reply of o.message.replies || []) {
      const box = node('article', null, 'reply');
      append(box, node('p', (reply.is_chai ? 'Chai · ' : '') + reply.user + ' · ' + slackDate(reply.timestamp), 'fine'), slackBody(reply.text, slackTime(reply.timestamp))); container.append(box);
    }
    if (!(o.message.replies || []).length) container.append(node('p', 'No retained thread replies.', 'fine'));
  }
  function sopURL(value) {
    try {
      const url = new URL(value);
      return url.origin === 'https://github.com' && !url.username && !url.password && !url.search && /^\/openshift\/release\/blob\/(main|master)\/docs\/dptp-triage-sop\/[^/]+\.md$/.test(url.pathname) ? url : null;
    } catch { return null; }
  }
  function sopLink(value, source) {
    // GitHub resolves repository-root links inside the selected branch.
    if (!value || /[\u0000-\u0020\\]/.test(value) || value.startsWith('//')) return null;
    try {
      const base = new URL(source);
      const root = base.origin + base.pathname.split('/').slice(0, 5).join('/');
      const url = new URL(value.startsWith('/') ? root + value : value, base);
      return link(url.href, '');
    } catch { return null; }
  }
  function sopInline(container, source, url) {
    const tokens = /(`+)([^`]*?)\1|\[([^\]\n]+)\]\(([^\s)]+)\)|\*\*(.+?)\*\*/g;
    let cursor = 0;
    for (const match of source.matchAll(tokens)) {
      container.append(node('span', source.slice(cursor, match.index)));
      if (match[1]) container.append(node('code', match[2]));
      else if (match[3]) {
        const anchor = sopLink(match[4], url);
        if (anchor) { sopInline(anchor, match[3], url); container.append(anchor); }
        else container.append(node('span', match[0]));
      } else {
        const strong = node('strong'); sopInline(strong, match[5], url); container.append(strong);
      }
      cursor = match.index + match[0].length;
    }
    container.append(node('span', source.slice(cursor)));
  }
  function sopHeading(line) {
    return line.match(/^ {0,3}(#{1,6})\s+(.+?)\s*#*\s*$/);
  }
  function sopFence(line) { return line.match(/^\s*(`{3,}|~{3,})(.*)$/); }
  function closesSOPFence(line, fence) {
    const match = sopFence(line);
    return match && match[1][0] === fence[0] && match[1].length >= fence.length && !match[2].trim();
  }
  function sopSection(markdown, url) {
    const lines = markdown.replace(/\r\n?/g, '\n').split('\n');
    let fragment = new URL(url).hash.slice(1);
    try { fragment = decodeURIComponent(fragment); } catch { /* Keep malformed anchors unmatched. */ }
    if (!fragment) return {lines, note: ''};
    const headings = [];
    const slugs = new Map();
    let fence = '';
    for (let index = 0; index < lines.length; index++) {
      if (fence) { if (closesSOPFence(lines[index], fence)) fence = ''; continue; }
      const opening = sopFence(lines[index]);
      if (opening) { fence = opening[1]; continue; }
      const heading = sopHeading(lines[index]);
      if (!heading) continue;
      const slug = heading[2].replace(/\[([^\]]+)\]\([^)]*\)/g, '$1').replace(/<[^>]*>/g, '').replace(/[`*_~]/g, '').toLowerCase().replace(/[^\p{L}\p{N}_\-\s]/gu, '').replace(/\s/g, '-');
      let unique = slug;
      let suffix = 0;
      while (slugs.has(unique)) unique = slug + '-' + (++suffix);
      slugs.set(unique, true);
      headings.push({index, level: heading[1].length, slug: unique});
    }
    const start = headings.findIndex(heading => heading.slug === fragment);
    if (start < 0) return {lines, note: 'Linked section was not found; showing the full SOP.'};
    const heading = headings[start];
    const end = headings.slice(start + 1).find(next => next.level <= heading.level)?.index || lines.length;
    return {lines: lines.slice(heading.index, end), note: 'Linked section'};
  }
  function sopCells(line) { return line.trim().replace(/^\|/, '').replace(/\|$/, '').split(/(?<!\\)\|/).map(cell => cell.trim().replace(/\\\|/g, '|')); }
  function renderSOPMarkdown(container, markdown, url) {
    const section = sopSection(markdown, url);
    if (section.note) container.append(node('p', section.note, 'fine sop-section-note'));
    const lines = section.lines;
    let paragraph = [];
    let lists = [];
    const inline = (tag, value) => { const element = node(tag); sopInline(element, value, url); return element; };
    const flush = () => { if (paragraph.length) container.append(inline('p', paragraph.join('\n'))); paragraph = []; };
    for (let index = 0; index < lines.length; index++) {
      const line = lines[index];
      const opening = sopFence(line);
      if (opening) {
        flush(); lists = [];
        const command = [];
        while (++index < lines.length && !closesSOPFence(lines[index], opening[1])) command.push(lines[index]);
        container.append(append(node('pre'), node('code', command.join('\n')))); continue;
      }
      if (!line.trim()) { flush(); continue; }
      const heading = sopHeading(line);
      if (heading) { flush(); lists = []; container.append(inline('h' + Math.min(6, heading[1].length + 2), heading[2])); continue; }
      if (line.includes('|') && /^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[index + 1] || '')) {
        flush(); lists = [];
        const table = node('table');
        const head = node('tr'); sopCells(line).forEach(cell => head.append(inline('th', cell)));
        table.append(append(node('thead'), head));
        const body = node('tbody'); index++;
        while (index + 1 < lines.length && lines[index + 1].trim() && lines[index + 1].includes('|')) {
          const row = node('tr'); sopCells(lines[++index]).forEach(cell => row.append(inline('td', cell))); body.append(row);
        }
        table.append(body); container.append(append(node('div', null, 'sop-table'), table)); continue;
      }
      const bullet = line.match(/^(\s*)([-+*]|\d+[.)])\s+(.+)$/);
      if (bullet) {
        flush(); const indent = bullet[1].length; const type = /^\d/.test(bullet[2]) ? 'ol' : 'ul';
        while (lists.length && lists[lists.length - 1].indent > indent) lists.pop();
        if (lists.length && lists[lists.length - 1].indent === indent && lists[lists.length - 1].type !== type) lists.pop();
        if (!lists.length || lists[lists.length - 1].indent < indent) {
          const list = node(type);
          if (type === 'ol') list.start = parseInt(bullet[2], 10);
          (lists.length ? lists[lists.length - 1].last : container).append(list);
          lists.push({indent, type, list, last: null});
        }
        const current = lists[lists.length - 1]; current.last = inline('li', bullet[3]); current.list.append(current.last); continue;
      }
      if (lists.length && /^\s+/.test(line)) { lists[lists.length - 1].last.append(inline('p', line.trim())); continue; }
      lists = []; paragraph.push(line);
    }
    flush();
  }
  /** Ignore delayed evidence responses after the authenticated session changes. */
  async function loadSOP(occurrence, url) {
    if (state.authPaused || !state.snapshot || sopCache.get(url)?.loading || typeof sopCache.get(url)?.markdown === 'string') return;
    const generation = state.generation;
    const entry = {loading: true, error: ''}; sopCache.set(url, entry); render();
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 12000);
    try {
      const response = await fetch('/api/sop?occurrence_id=' + encodeURIComponent(id(occurrence)) + '&url=' + encodeURIComponent(url), {credentials: 'same-origin', signal: controller.signal, headers: {Accept: 'application/json'}});
      const result = await responseJSON(response);
      if (!response.ok) throw new Error(result.error || 'SOP is unavailable (HTTP ' + response.status + ').');
      if (typeof result.markdown !== 'string' || result.markdown.length > 262144) throw new Error('The SOP response is invalid or too large.');
      if (generation === state.generation && !state.authPaused) entry.markdown = result.markdown;
    } catch (err) {
      entry.error = err.name === 'AbortError' ? 'SOP request timed out after 12 seconds.' : err.message || 'SOP could not be loaded.';
    } finally { window.clearTimeout(timeout); entry.loading = false; render(); }
  }
  function renderSOPContent(body, occurrence, url) {
    body.replaceChildren();
    const entry = sopCache.get(url);
    if (typeof entry?.markdown === 'string') renderSOPMarkdown(body, entry.markdown, url);
    else if (entry?.error) append(body, node('p', entry.error, 'error-text'), button('Retry loading SOP', () => loadSOP(occurrence, url)));
    else body.append(node('p', entry?.loading ? 'Loading SOP…' : 'Expand to load the SOP.', 'fine'));
  }
  function renderSOPs(container, occurrence) {
    const urls = [...new Set((occurrence.message.sop_urls || []).filter(value => typeof value === 'string' && sopURL(value)))];
    if (!urls.length) return;
    const section = node('section', null, 'sops');
    section.setAttribute('aria-label', 'Alert SOPs'); section.append(node('h3', 'Triage SOP' + (urls.length > 1 ? 's' : '')));
    for (const url of urls) {
      const details = node('details', null, 'sop');
      const key = id(occurrence) + '\n' + url;
      details.open = sopDisclosures.has(key);
      const name = new URL(url).pathname.split('/').pop();
      const summary = node('summary', name.replace(/\.md$/, '').replace(/[-_]/g, ' '));
      const original = link(url, '↗ Open original SOP on GitHub');
      const heading = append(node('div', null, 'sop-heading'), original, details);
      details.append(summary);
      const body = node('div', null, 'sop-body'); body.setAttribute('aria-live', 'polite');
      if (details.open) renderSOPContent(body, occurrence, url);
      details.append(body);
      details.addEventListener('toggle', () => {
        if (details.open) { sopDisclosures.add(key); renderSOPContent(body, occurrence, url); if (!sopCache.has(url)) loadSOP(occurrence, url); }
        else sopDisclosures.delete(key);
      });
      section.append(heading);
    }
    container.append(section);
  }
  const metricsCache = new Map();
  const metricsDisclosures = new Set();
  const metricsSelections = new Map();
  const metricAlerts = o => Array.isArray(o.message.prometheus_alerts) ? [...new Set(o.message.prometheus_alerts.filter(a => typeof a === 'string' && a.length))] : [];
  const metricKey = (o, alert) => JSON.stringify([id(o), alert]);
  const metricsEnabled = () => state.snapshot?.monitoring?.enabled === true && state.snapshot.monitoring.source === 'core-ci';
  /** Keep on-demand metrics bound to the occurrence and authenticated session that requested them. */
  async function loadMetrics(o, alert, refresh = false) {
    if (state.authPaused || !metricsEnabled() || !metricAlerts(o).includes(alert)) return;
    const key = metricKey(o, alert);
    if (metricsCache.get(key)?.loading || (!refresh && metricsCache.get(key)?.data)) return;
    const generation = state.generation;
    const entry = {loading: true, error: ''}; metricsCache.set(key, entry); render();
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 20000);
    try {
      const response = await fetch('/api/metrics?occurrence_id=' + encodeURIComponent(id(o)) + '&alert=' + encodeURIComponent(alert), {credentials: 'same-origin', signal: controller.signal, headers: {Accept: 'application/json'}});
      const result = await responseJSON(response);
      if (!response.ok) throw new Error(result.error || 'Metrics unavailable (HTTP ' + response.status + ').');
      if (result.source !== 'core-ci' || result.alert !== alert || !result.rule || !result.window || !Array.isArray(result.series)) throw new Error('Unexpected metrics response from core-ci.');
      if (generation === state.generation && !state.authPaused) entry.data = result;
    } catch (err) {
      entry.error = err.name === 'AbortError' ? 'Metrics request timed out after 20 seconds.' : err.message || 'Metrics could not be loaded.';
    } finally { window.clearTimeout(timeout); entry.loading = false; render(); }
  }
  function metricsChart(container, data) {
    const start = Date.parse(data.window.start), end = Date.parse(data.window.end), step = Number(data.window.step_seconds) * 1000;
    const valid = p => Number.isFinite(Date.parse(p?.at)) && Date.parse(p.at) >= start && Date.parse(p.at) <= end && typeof p.value === 'number' && Number.isFinite(p.value);
    const series = data.series.slice(0, 12);
    const values = series.flatMap(s => (Array.isArray(s.points) ? s.points : []).filter(valid).map(p => p.value));
    if (!data.series.length) { container.append(node('p', 'No series returned. This is unavailable evidence, not a healthy result.', 'fine')); return; }
    if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start || !Number.isFinite(step) || step <= 0 || !values.length) {
      container.append(node('p', 'No finite samples available in the reported window. Missing data is not zero or a healthy result.', 'fine')); return;
    }
    const scale = Math.max(1, ...values.map(v => Math.abs(v)));
    let low = Math.min(...values.map(v => v / scale)), high = Math.max(...values.map(v => v / scale));
    if (low === high) { low -= 0.1; high += 0.1; }
    const svgNode = (tag, attrs, content) => {
      const n = document.createElementNS('http://www.w3.org/2000/svg', tag);
      for (const [key, value] of Object.entries(attrs || {})) n.setAttribute(key, String(value));
      if (content !== undefined) n.textContent = content;
      return n;
    };
    const svg = svgNode('svg', {viewBox: '0 0 640 200', role: 'img', 'aria-label': values.length + ' finite samples from ' + date(data.window.start) + ' to ' + date(data.window.end) + '. Gaps indicate missing samples. Values are expression results, not live firing state.', class: 'metrics-chart'});
    svg.append(svgNode('title', {}, 'Core-ci alert expression results. Each plotted sample has a time and value tooltip.'));
    svg.append(svgNode('path', {d: 'M65 15 V165 H625', class: 'metrics-axis'}));
    svg.append(svgNode('text', {x: 60, y: 20, 'text-anchor': 'end'}, String(Math.max(...values))), svgNode('text', {x: 60, y: 165, 'text-anchor': 'end'}, String(Math.min(...values))), svgNode('text', {x: 65, y: 190}, date(data.window.start)), svgNode('text', {x: 625, y: 190, 'text-anchor': 'end'}, date(data.window.end)));
    const legend = node('ul', null, 'metrics-legend');
    series.forEach((s, index) => {
      const points = Array.isArray(s.points) ? s.points : [];
      const label = Object.entries(s.labels || {}).map(([k, v]) => k + '=' + String(v)).join(', ') || 'Unlabelled series';
      const className = 'metrics-series metrics-color-' + index % 6;
      let segment = [], previous = null, last = null, count = 0;
      const flush = () => { if (segment.length > 1) svg.append(svgNode('polyline', {points: segment.join(' '), class: className})); segment = []; };
      for (const p of points) {
        const at = Date.parse(p?.at);
        if (!valid(p)) { flush(); previous = null; continue; }
        if (previous !== null && (at <= previous || at - previous > step * 1.5)) flush();
        const x = 65 + (at - start) / (end - start) * 560, y = 165 - (p.value / scale - low) / (high - low) * 150;
        segment.push(x.toFixed(2) + ',' + y.toFixed(2));
        const dot = svgNode('circle', {cx: x.toFixed(2), cy: y.toFixed(2), r: 2, class: className});
        dot.append(svgNode('title', {}, label + ' · ' + date(p.at) + ' · value ' + p.value)); svg.append(dot);
        previous = at; last = p; count++;
      }
      flush();
      legend.append(node('li', label + ' · ' + count + ' samples' + (last ? ' · last ' + last.value + ' at ' + date(last.at) : ' · no finite samples'), 'metrics-color-' + index % 6));
    });
    append(container, svg, legend);
    if (data.series.length > series.length) container.append(node('p', 'Chart limited to the first 12 series.', 'fine'));
  }
  function renderMetricsContent(body, o, alert) {
    body.replaceChildren();
    body.append(node('p', 'Read-only inspection of source core-ci. The Slack notification may refer to another source. Metric values do not establish current firing state or change workflow status.', 'fine'));
    if (state.authPaused || !metricsEnabled()) { body.append(node('p', 'Core-ci metrics are disabled or not configured for this dashboard.', 'fine')); return; }
    const entry = metricsCache.get(metricKey(o, alert));
    if (entry?.loading) { body.append(node('p', 'Loading from core-ci…', 'fine')); return; }
    if (entry?.error) body.append(node('p', entry.error, 'error-text'));
    body.append(button(entry?.error ? 'Retry metrics' : entry?.data ? 'Refresh metrics' : 'Load from core-ci', () => loadMetrics(o, alert, true)));
    if (!entry?.data) return;
    const data = entry.data, rule = data.rule;
    if (data.demo === true) body.append(node('p', 'Synthetic demo metrics', 'metrics-demo'));
    const facts = node('dl', null, 'facts');
    fact(facts, 'Source', data.source); fact(facts, 'Alert', alert); fact(facts, 'Rule', rule.namespace + ' / ' + rule.resource); fact(facts, 'Group', rule.group || 'Not available'); fact(facts, 'For', rule.for || 'Not specified');
    fact(facts, 'Window', date(data.window.start) + ' to ' + date(data.window.end) + ' · step ' + data.window.step_seconds + 's');
    append(body, facts, node('pre', rule.expression, 'metrics-expression'), node('p', 'Expression results may exclude non-firing periods. Missing or nonfinite samples are gaps, never zero-filled.', 'fine'));
    metricsChart(body, data);
    for (const warning of Array.isArray(data.warnings) ? data.warnings : []) body.append(node('p', warning, 'error-text'));
    if (data.truncated) body.append(node('p', 'Metrics response was truncated; some evidence is unavailable.', 'error-text'));
  }
  function renderMetrics(container, o) {
    const alerts = metricAlerts(o);
    if (!alerts.length) return;
    const details = node('details', null, 'metrics');
    details.open = metricsDisclosures.has(id(o));
    details.append(node('summary', 'Core-ci metrics'));
    const body = node('div', null, 'metrics-body'); body.setAttribute('aria-live', 'polite');
    const selected = metricsSelections.get(id(o));
    const alert = alerts.includes(selected) ? selected : alerts[0];
    metricsSelections.set(id(o), alert);
    if (alerts.length > 1) {
      const label = node('label', 'Alert rule '), select = node('select');
      for (const name of alerts) { const option = node('option', name); option.value = name; option.selected = name === alert; select.append(option); }
      select.setAttribute('aria-label', 'Alert rule to inspect in core-ci');
      select.addEventListener('change', () => { metricsSelections.set(id(o), select.value); renderMetricsContent(body, o, select.value); });
      label.append(select); details.append(label);
    }
    details.append(body);
    if (details.open) renderMetricsContent(body, o, alert);
    details.addEventListener('toggle', () => {
      if (details.open) { metricsDisclosures.add(id(o)); renderMetricsContent(body, o, metricsSelections.get(id(o)) || alert); }
      else metricsDisclosures.delete(id(o));
    });
    container.append(details);
  }
  /** slackTime retains the instant when Slack supplies fractional epoch seconds. */
  function slackTime(value) { return /^\d+\.\d+$/.test(text(value)) ? Number(value) * 1000 : value; }
  function slackDate(value) { return date(slackTime(value)); }
  const reportLabels = {
    pending_workloads_scan: 'Pending workloads',
    capacity_autoscale_snapshot: 'Capacity & autoscaling',
    ci_control_plane_deployments: 'Control plane deployments',
    prow_app_ci_health: 'Prow & app.ci',
    build_farm_jobs_infra: 'Build farm jobs & infrastructure',
    csr_nodes_machine_health: 'CSRs, nodes & machines'
  };
  function reportLabel(task) { return Object.hasOwn(reportLabels, task) ? reportLabels[task] : task; }
  function taskName(o) {
    if (text(o.message.scheduled_task).trim()) return text(o.message.scheduled_task).trim();
    const match = (text(o.message.title) + '\n' + text(o.message.text)).match(/\[Scheduled task:\s*([^\]\n]+)\]/i);
    return match ? match[1].split('|')[0].trim().replace(/^`+|`+$/g, '') || 'Scheduled report' : text(o.message.title).trim() || 'Scheduled report';
  }
  function reportBody(o) {
    // Only remove a complete leading task header. Unexpected source formats stay intact.
    return text(o.message.text).replace(/^\s*\[Scheduled task:[^\]\n]+\]\s*\n?/i, '').trim();
  }
  function reportHeadline(o) {
    return localProse(reportBody(o).split('\n').find(line => line.trim())?.trim(), o.message.reported_at) || 'No report text supplied.';
  }
  /** reportInline keeps inline markup inert, with Slack stars treated as bold and bounded nesting. */
  function reportInline(container, source, reference, depth = 0) {
    if (depth >= 8) { container.append(node('span', localProse(emojify(decodeSlack(source)), reference))); return; }
    const tokens = /!\[[^\]\n]*\]\([^\s)]*\)|(`+)([^`\n]+)\1|(?<!!)\[([^\]\n]+)\]\(([^\s)]+)\)|<[^>\n]+>|(\*\*|__|~~|[*_~])([^\n]+?)\5|https:\/\/[^\s<>]+|:[a-z_+-][a-z0-9_+-]*:/g;
    let cursor = 0;
    for (const match of source.matchAll(tokens)) {
      container.append(node('span', localProse(emojify(decodeSlack(source.slice(cursor, match.index))), reference)));
      if (match[0].startsWith('![')) container.append(node('span', emojify(decodeSlack(match[0]))));
      else if (match[1]) container.append(node('code', decodeSlack(match[2])));
      else if (match[3]) {
        const anchor = link(decodeSlack(match[4]), localProse(emojify(decodeSlack(match[3])), reference));
        container.append(anchor || node('span', emojify(decodeSlack(match[0]))));
      } else if (match[5]) {
        const wrapper = node({'**': 'strong', '__': 'strong', '*': 'strong', '_': 'em', '~~': 's', '~': 's'}[match[5]]);
        reportInline(wrapper, match[6], reference, depth + 1); container.append(wrapper);
      } else slackInline(container, match[0], reference);
      cursor = match.index + match[0].length;
    }
    container.append(node('span', localProse(emojify(decodeSlack(source.slice(cursor))), reference)));
  }
  /** reportBlock creates blocks with inline formatting without enabling HTML or remote images. */
  function reportBlock(tag, source, reference) {
    const block = node(tag); reportInline(block, source, reference); return block;
  }
  /** reportCells preserves pipes in balanced Slack tokens, inline code and escaped text. */
  function reportCells(line) {
    const source = line.trim(), protectedTokens = new Map();
    for (const match of source.matchAll(/<[^<>\n]+>|(`+)[^`\n]+\1/g)) protectedTokens.set(match.index, match[0]);
    const cells = [];
    let cell = '';
    for (let index = 0; index < source.length; index++) {
      const token = protectedTokens.get(index);
      if (token) { cell += token; index += token.length - 1; continue; }
      if (source[index] === '\\' && source[index + 1] === '\\') { cell += '\\\\'; index++; continue; }
      if (source[index] === '\\' && source[index + 1] === '|') { cell += '|'; index++; continue; }
      if (source[index] === '|') { cells.push(cell.trim()); cell = ''; continue; }
      cell += source[index];
    }
    cells.push(cell.trim());
    if (source.startsWith('|')) cells.shift();
    if (!cells[cells.length - 1] && source.endsWith('|')) cells.pop();
    return cells;
  }
  /** renderReportBody parses the report subset with at most eight quote and list levels. Source evidence is separate. */
  function renderReportBody(container, source, reference, depth = 0) {
    if (!source) { container.append(node('p', 'No report text supplied.', 'muted')); return; }
    if (depth >= 8) { container.append(node('p', localProse(source, reference))); return; }
    const lines = source.replace(/\r\n?/g, '\n').split('\n');
    let paragraph = [];
    let lists = [];
    const flush = () => {
      if (paragraph.length) container.append(reportBlock('p', paragraph.join('\n'), reference));
      paragraph = [];
    };
    for (let index = 0; index < lines.length; index++) {
      const line = lines[index];
      const opening = sopFence(line);
      if (opening) {
        flush();
        const command = [];
        while (++index < lines.length && !closesSOPFence(lines[index], opening[1])) command.push(lines[index]);
        const destination = lists.length && /^\s/.test(line) ? lists[lists.length - 1].last : container;
        destination.append(append(node('pre'), node('code', decodeSlack(command.join('\n')))));
        if (destination === container) lists = [];
        continue;
      }
      if (!line.trim()) { flush(); continue; }
      const quote = line.match(/^\s*(?:>|&gt;) ?(.*)$/);
      if (quote) {
        flush(); lists = [];
        const quoted = [quote[1]];
        while (index + 1 < lines.length) {
          const next = lines[index + 1].match(/^\s*(?:>|&gt;) ?(.*)$/);
          if (!next) break;
          quoted.push(next[1]); index++;
        }
        const block = node('blockquote'); renderReportBody(block, quoted.join('\n'), reference, depth + 1); container.append(block); continue;
      }
      const heading = sopHeading(line);
      if (heading) { flush(); lists = []; container.append(reportBlock('h' + Math.min(6, heading[1].length + 2), heading[2], reference)); continue; }
      if (line.includes('|') && /^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[index + 1] || '')) {
        flush(); lists = [];
        const table = node('table'), head = node('tr');
        reportCells(line).forEach(cell => head.append(reportBlock('th', cell, reference)));
        table.append(append(node('thead'), head));
        const body = node('tbody'); index++;
        while (index + 1 < lines.length && lines[index + 1].trim() && lines[index + 1].includes('|')) {
          const row = node('tr'); reportCells(lines[++index]).forEach(cell => row.append(reportBlock('td', cell, reference))); body.append(row);
        }
        table.append(body); container.append(append(node('div', null, 'report-table'), table)); continue;
      }
      const bullet = line.match(/^(\s*)([•+*-]|\d+[.)])\s+(.+)$/);
      if (bullet) {
        flush(); const indent = bullet[1].replace(/\t/g, '    ').length, type = /^\d/.test(bullet[2]) ? 'ol' : 'ul';
        while (lists.length && lists[lists.length - 1].indent > indent) lists.pop();
        if (lists.length && lists[lists.length - 1].indent === indent && lists[lists.length - 1].type !== type) lists.pop();
        if (!lists.length || lists[lists.length - 1].indent < indent && lists.length < 8) {
          const list = node(type);
          if (type === 'ol') list.start = parseInt(bullet[2], 10);
          (lists.length ? lists[lists.length - 1].last : container).append(list);
          lists.push({indent, type, list, last: null});
        }
        const current = lists[lists.length - 1]; current.last = reportBlock('li', bullet[3], reference); current.list.append(current.last); continue;
      }
      if (lists.length && /^\s+/.test(line)) {
        const indent = line.match(/^\s*/)[0].replace(/\t/g, '    ').length;
        while (lists.length > 1 && lists[lists.length - 1].indent >= indent) lists.pop();
        lists[lists.length - 1].last.append(reportBlock('p', line.trim(), reference)); continue;
      }
      lists = []; paragraph.push(line);
    }
    flush();
  }
  function reportCoverage(source) {
    const lines = source.split('\n');
    const excerpt = [];
    for (let index = 0; index < lines.length; index++) {
      const label = lines[index].trim().replace(/^(?:[•*-]|\d+[.)])\s+/, '').replace(/^#{1,6}\s+/, '').replace(/^\*{1,2}(.+?)\*{1,2}:?$/, '$1');
      if (!/^(?:coverage(?: and limits| gap| gaps| limits?| limitations)?|registry and coverage|limitations?|limits?|scope)(?::.*)?$/i.test(label)) continue;
      excerpt.push(lines[index]);
      // Explicit section headings can introduce bullets; never absorb unrelated prose.
      if (label.includes(':') && label.split(':').slice(1).join(':').trim()) continue;
      let next = index + 1;
      while (next < lines.length && !lines[next].trim()) next++;
      while (next < lines.length && /^\s*(?:[•*-]|\d+[.)])\s+/.test(lines[next])) {
        excerpt.push(lines[next++]);
        while (next < lines.length && /^\s+\S/.test(lines[next]) && !/^\s*(?:[•*-]|\d+[.)])\s+/.test(lines[next])) excerpt.push(lines[next++]);
      }
      index = next - 1;
    }
    return excerpt.join('\n');
  }
  function reportDisclosure(key, className) {
    const details = node('details', null, className);
    details.open = state.reportDisclosures.has(key);
    details.addEventListener('toggle', () => {
      if (details.open) state.reportDisclosures.add(key);
      else state.reportDisclosures.delete(key);
    });
    return details;
  }
  function originalReport(o) {
    const details = reportDisclosure('source:' + id(o), 'report-source');
    const parts = o.parts || [o];
    details.append(node('summary', parts.length > 1 ? 'Original Slack messages · ' + parts.length + ' parts' : 'Original Slack message'));
    for (const part of parts) {
      if (parts.length > 1) append(details, link(part.message.slack_url, '↗ Source part · ' + date(part.message.reported_at)));
      details.append(node('p', part.message.text || 'No message text supplied.', 'evidence'));
    }
    return details;
  }
  const pendingSelections = new Map();
  /** Show only reported pod snapshots; evidence remains available for every extracted value. */
  function renderPending(container, report, previous) {
    if (taskName(report) !== 'pending_workloads_scan') return;
    const parsed = OpsReports.pending(report);
    const panel = node('section', null, 'pending-summary'); panel.setAttribute('aria-label', 'Pending workload snapshot');
    append(panel, node('h3', 'Pending pods by cluster'), node('p', 'Reported scan snapshots, including system and overprovisioning pods. Counts do not measure blocked jobs or severity.', 'fine'));
    if (!parsed.observations.length) {
      append(panel, node('p', 'No unambiguous cluster counts were found. Read the report below for findings and coverage.', 'report-extraction-note'));
      container.append(panel); return;
    }
    const prior = previous ? OpsReports.pending(previous) : {observations: []};
    const priorByContext = new Map(prior.observations.map(observation => [observation.context, observation]));
    const observed = parsed.observations.filter(observation => observation.coverage === 'observed');
    panel.append(node('p', observed.length + ' clusters with counts · ' + (parsed.observations.length - observed.length) + ' unavailable or conflicting', 'fine'));
    if (parsed.partial) panel.append(node('p', 'Partial extraction. Some findings need the source text; unlisted clusters have no inferred count.', 'report-extraction-note'));
    if (previous) panel.append(node('p', 'Previous report: ' + date(previous.message.reported_at) + '. Scan coverage and qualifiers may differ. — means no observed count.', 'fine'));
    const maximum = Math.max(1, ...observed.map(observation => observation.value));
    const key = id(report);
    const selected = parsed.observations.find(observation => observation.context === pendingSelections.get(key)) || parsed.observations[0];
    const rows = node('div', null, 'pending-rows');
    append(rows, append(node('div', null, 'pending-columns'), node('span', 'Cluster'), node('span', 'This report'), node('span', previous ? 'Previous' : '')));
    for (const observation of parsed.observations) {
      const measured = observation.coverage === 'observed';
      const label = measured ? String(observation.value) : observation.coverage === 'conflict' ? 'Conflicting' : 'Not observed';
      const priorObservation = priorByContext.get(observation.context);
      const priorLabel = priorObservation?.coverage === 'observed' ? String(priorObservation.value) : '—';
      const row = button('', () => { pendingSelections.set(key, observation.context); render(); }, 'pending-row');
      row.setAttribute('aria-pressed', String(observation.context === selected.context));
      row.setAttribute('aria-label', observation.context + ' · ' + label + (measured ? ' Pending pods' : '') + (previous ? ' · previous ' + priorLabel : ''));
      const track = node('span', null, 'pending-track'); track.setAttribute('aria-hidden', 'true');
      if (measured) { const bar = node('span', null, 'pending-fill'); bar.style.width = (100 * observation.value / maximum) + '%'; track.append(bar); }
      append(row, node('span', observation.context, 'pending-context'), append(node('span', null, 'pending-value'), track, node('strong', label)), node('span', previous ? priorLabel : '', 'pending-prior'));
      rows.append(row);
    }
    panel.append(rows);
    const evidence = node('div', null, 'pending-evidence');
    append(evidence, node('h4', selected.context + ' · source evidence'));
    for (const note of selected.notes) evidence.append(node('p', localProse(note, report.message.reported_at), 'fine'));
    for (const span of selected.evidence) evidence.append(node('p', localProse(span, report.message.reported_at)));
    if (selected.coverage === 'conflict') evidence.append(node('p', 'The source contains incompatible counts. No numeric bar is shown.'));
    append(panel, evidence); container.append(panel);
  }
  function renderReports(container, reports) {
    container.append(node('p', 'Scheduled Chai reports are separate from alerts. Findings describe the source scan; the report label does not indicate infrastructure health.', 'banner'));
    const groups = new Map();
    for (const report of OpsReports.runs(reports)) {
      const key = taskName(report);
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(report);
    }
    const query = state.query.toLowerCase();
    const matching = [...groups].filter(([task, history]) => [task, reportLabel(task)].some(label => label.toLowerCase().includes(query)) || history.some(matchesSearch));
    if (!matching.length) { container.append(node('p', 'No scheduled reports match this search in the retained history.', 'empty')); return; }
    if (!matching.some(([task]) => task === state.selectedReport)) state.selectedReport = matching[0][0];
    const workspace = node('div', null, 'report-workspace');
    const overview = node('section', null, 'report-overview'); overview.setAttribute('aria-label', 'Scheduled report tasks');
    append(overview, append(node('div', null, 'list-head'), node('span', matching.length + ' tasks'), node('span', 'Latest per task')));
    for (const [task, history] of matching) {
      const latest = history[0];
      const card = button('', () => { state.selectedReport = task; render(); }, 'report-task');
      card.setAttribute('aria-pressed', String(task === state.selectedReport));
      append(card, node('strong', reportLabel(task)), node('span', task, 'identifier'), node('time', date(latest.message.reported_at), 'report-time'), node('span', reportHeadline(latest), 'report-headline'));
      overview.append(card);
    }
    const [task, history] = matching.find(([key]) => key === state.selectedReport);
    const latest = history[0];
    const detail = node('article', null, 'report-detail'); detail.setAttribute('aria-label', 'Selected scheduled report');
    append(detail, append(node('div', null, 'report-detail-meta'), badge(latest), node('time', 'Latest · ' + date(latest.message.reported_at), 'fine')), node('h2', reportLabel(task)), node('p', task, 'identifier'), link(latest.message.slack_url, '↗ Open latest report in Slack'));
    const body = reportBody(latest);
    const coverage = reportCoverage(body);
    if (coverage) append(detail, append(node('aside', null, 'report-coverage'), node('h3', 'Source coverage & limits'), node('p', localProse(coverage, latest.message.reported_at))));
    renderPending(detail, latest, history[1]);
    const readable = node('div', null, 'report-body'); renderReportBody(readable, body, latest.message.reported_at);
    append(detail, readable, originalReport(latest));
    const previous = history.slice(1);
    const archive = reportDisclosure('archive:' + task, 'report-archive');
    archive.append(node('summary', 'Earlier reports · ' + previous.length + ' retained'));
    for (const [index, report] of previous.entries()) {
      const entry = reportDisclosure('history:' + id(report), 'report-history');
      const content = node('div', null, 'report-body'); renderReportBody(content, reportBody(report), report.message.reported_at);
      append(entry, node('summary', date(report.message.reported_at)), link(report.message.slack_url, '↗ Open report in Slack'));
      renderPending(entry, report, previous[index + 1]);
      append(entry, content, originalReport(report));
      archive.append(entry);
    }
    if (!previous.length) archive.append(node('p', 'No earlier reports retained for this task.', 'fine'));
    append(detail, archive); append(workspace, overview, detail); container.append(workspace);
  }
  function nextSix() { const until = new Date(); until.setUTCHours(6, 0, 0, 0); if (until <= new Date()) until.setUTCDate(until.getUTCDate() + 1); return until; }
  const escapeRegex = value => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const actionLabels = {acknowledge: 'Start work', snooze: 'Snooze', close: 'Close investigations', investigate: 'Ask Chai'};
  /** Require a visible confirmation scope before creating a workflow request. */
  function openDialog(kind, occurrence) {
    if (!ready() || (kind === 'investigate' && outstandingInvestigation(occurrence))) return;
    state.dialogTargets = []; state.dialogGroups = []; state.previewGroups = []; el('selection-patterns').replaceChildren(); el('selection-patterns').hidden = true;
    el('scope-occurrence-label').textContent = 'This occurrence';
    state.dialogKind = kind; state.dialogOccurrence = occurrence; state.dialogScope = 'occurrence'; state.lastFocus = document.activeElement;
    el('dialog-title').textContent = actionLabels[kind] + ' · preview';
    el('dialog-context').textContent = displayTitle(occurrence) + ' · ' + occurrence.message.source + ' · ' + channelName();
    el('scope-controls').hidden = kind === 'investigate';
    for (const input of document.querySelectorAll('input[name="scope"]')) { input.checked = input.value === 'occurrence'; input.disabled = input.value === 'regex' && !(occurrence.message.identifiers || []).length; }
    el('draft').maxLength = kind === 'investigate' ? 4000 : 512;
    el('draft').value = kind === 'investigate' ? 'Analyze this alert: likely cause, evidence, and next read-only checks.' : '^(?:' + (occurrence.message.identifiers || []).map(escapeRegex).join('|') + ')$';
    el('confirm').textContent = actionLabels[kind]; el('form-error').textContent = '';
    updatePreview(); el('action-dialog').showModal(); el(kind === 'investigate' ? 'draft' : 'confirm').focus();
  }
  /** Freeze every checked occurrence before presenting an exact action plan. */
  function openSelectedAction(kind) {
    if (!batchKinds.includes(kind)) return;
    pruneSelection();
    const targets = visible().filter(o => state.checked.has(id(o)) && openAlert(o)).map(freezeTarget);
    if (!ready() || !targets.length) return;
    if (targets.length > 10000) { error('Choose between 1 and 10,000 occurrences for one ' + batchLabel(kind) + ' plan. Narrow the filters or selection and try again.'); return; }
    openDialog(kind, targets[0]);
    state.dialogScope = 'selected'; state.dialogTargets = targets;
    const groups = new Map();
    for (const target of targets) { const key = JSON.stringify([target.identity.channel, target.message.source]); if (!groups.has(key)) groups.set(key, []); groups.get(key).push(target); }
    state.dialogGroups = [...groups.values()].map(members => {
      const anchor = members.find(target => target.message.identifiers?.length && !selectionConflict(kind, target, true));
      const identifiers = [...new Set(members.flatMap(target => target.message.identifiers || []))];
      return {anchor, channel: members[0].identity.channel, source: members[0].message.source, pattern: '^(?:' + identifiers.map(escapeRegex).join('|') + ')$'};
    });
    el('scope-controls').hidden = false; el('scope-occurrence-label').textContent = 'Selected occurrences';
    for (const input of document.querySelectorAll('input[name="scope"]')) { input.checked = input.value === 'occurrence'; input.disabled = input.value === 'regex' && state.dialogGroups.some(group => !group.anchor); }
    const patterns = el('selection-patterns'); patterns.replaceChildren();
    for (const group of state.dialogGroups) {
      const field = node('textarea'); field.value = group.pattern; field.maxLength = 512;
      field.setAttribute('aria-label', 'Identifier regex for channel ' + channelName() + ' / source ' + group.source);
      field.addEventListener('input', () => { group.pattern = field.value.trim(); updatePreview(); });
      patterns.append(append(node('label'), node('span', 'Channel ' + channelName() + ' / source ' + group.source + (group.anchor ? ' · anchor ' + displayTitle(group.anchor) : ' · no eligible parsed anchor')), field));
    }
    el('dialog-context').textContent = targets.length + ' selected open alert(s) · choose exact occurrences or matching rules';
    updatePreview();
  }
  function openSelectedClose() { return openSelectedAction('close'); }
  const freezeTarget = target => ({...target, identity: {...target.identity}, message: {...target.message, identifiers: [...(target.message.identifiers || [])]}});
  /** Invalidate in-flight previews so a closed dialog cannot receive stale targets. */
  function closeDialog() { state.previewVersion++; el('action-dialog').close(); state.dialogOccurrence = null; state.dialogTargets = []; state.dialogGroups = []; state.previewGroups = []; state.previewTargets = []; state.previewValid = false; state.lastFocus?.focus(); }
  /** Match the server's UTF-8 input limit before retaining a new request. */
  function chaiQuestionError(question) {
    if (!question) return 'Enter a question for Chai before sending.';
    return new TextEncoder().encode(question).length > 4000 ? 'This question is too long. Shorten it before sending.' : '';
  }
  /** Use server regex semantics and reject previews superseded by a newer scope or session. */
  async function updatePreview() {
    const o = state.dialogOccurrence; if (!o) return;
    const version = ++state.previewVersion, kind = state.dialogKind, scope = state.dialogScope, draft = el('draft').value.trim();
    const investigate = kind === 'investigate', regex = scope === 'regex' && !investigate;
    const grouped = state.dialogTargets.length > 0;
    el('selection-patterns').hidden = !(grouped && regex);
    el('draft').hidden = !(investigate || (regex && !grouped)); el('draft-label').hidden = !(investigate || (regex && !grouped));
    el('draft').required = investigate || (regex && !grouped);
    el('draft-label').textContent = investigate ? 'Question for Chai' : 'Identifier regular expression (RE2 syntax)';
    state.previewValid = false; state.previewTargets = []; state.previewGroups = []; el('confirm').disabled = true;
    el('form-error').textContent = '';
    if (grouped && regex) return updateSelectionRegex(version, kind);
    if (grouped && state.dialogTargets.some(target => selectionConflict(kind, target))) { el('form-error').textContent = 'Some selected alerts already have this state. Uncheck them before confirming this exact plan, or choose a matching regex rule.'; return; }
    if (investigate) {
      el('preview').textContent = 'DPTP-bot → configured Chai identity\nDestination: the selected source thread in ' + channelName();
      el('form-error').textContent = chaiQuestionError(draft);
      state.previewValid = !el('form-error').textContent; el('confirm').disabled = !state.previewValid; return;
    }
    let targets = scope === 'selected' ? state.dialogTargets : [o], until = date(nextSix());
    if (regex) {
      if ((state.dialogKind === 'investigate' || state.dialogScope === 'regex') && !draft) { el('preview').textContent = 'Enter an identifier regex to preview matching occurrences.'; return; }
      el('preview').textContent = 'Loading matching occurrences from the server…';
      try {
        const response = await fetch('/api/action-preview', {method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': state.snapshot?.csrf_token || ''}, body: JSON.stringify({occurrence_id: id(o), kind, pattern: draft})});
        const result = await responseJSON(response);
        if (version !== state.previewVersion || state.authPaused) return;
        if (!response.ok) throw new Error(result.error || 'The scope could not be previewed.');
        if (!Array.isArray(result.occurrences) || result.occurrences.some(target => !target.identity?.id || !target.identity?.revision || target.identity.channel !== o.identity.channel || target.message?.source !== o.message.source || isReport(target))) throw new Error('The server returned an invalid scope preview.');
        targets = result.occurrences;
        if (new Set(targets.map(id)).size !== targets.length) throw new Error('The server returned duplicate scope targets.');
        if (result.until) until = date(result.until);
      } catch (err) { if (version === state.previewVersion) el('preview').textContent = err.message || 'Preview unavailable.'; return; }
    }
    if (version !== state.previewVersion) return;
    state.previewTargets = targets.map(freezeTarget); state.previewValid = targets.length > 0;
    const deadline = kind === 'close' ? 'Only the frozen occurrences listed below will be closed. Future matches are not closed.' : kind === 'acknowledge' && !regex ? 'Work remains in progress until released.' : (kind === 'snooze' ? 'Snooze' : 'Matching WIP rule') + ' ends ' + until + '. The server computes the exact deadline.';
    const future = regex && kind !== 'close' ? '\nMatching current and future occurrences share this rule until expiry.' : '';
    el('preview').textContent = (scope === 'selected' ? 'Scope: exact selected occurrences across their listed channels and sources' : 'Scope: ' + channelName() + ' / source ' + o.message.source + (regex ? '\nPattern: ' + draft : '\nThis occurrence only')) + '\n' + deadline + future + '\n\n' + targets.length + ' current occurrence(s):\n' + targets.map(target => '• ' + displayTitle(target) + ' · ' + date(target.message.reported_at) + (kind === 'close' || scope === 'selected' ? ' · channel ' + channelName() + ' / source ' + target.message.source : '')).join('\n');
    if (grouped && state.dialogGroups.some(group => !group.anchor)) el('form-error').textContent = 'Matching regex is unavailable: each channel/source group needs a selected eligible alert with parsed identifiers. Exact selected actions remain available.';
    el('confirm').disabled = !state.previewValid;
  }
  const selectionConflict = (kind, target, regex = false) => kind === 'snooze' ? !!target.snooze : kind === 'acknowledge' && !regex ? !!workflow(target).acknowledged_at && !target.wip_rule : false;
  /** Preview each real server scope independently; never approximate RE2 matches in JavaScript. */
  async function updateSelectionRegex(version, kind) {
    const groups = state.dialogGroups.map(group => ({...group, anchor: group.anchor && freezeTarget(group.anchor)}));
    el('preview').textContent = 'Loading matching occurrences for each selected channel and source…';
    try {
      if (!groups.length || groups.some(group => !group.anchor)) throw new Error('Each channel/source group needs a selected eligible alert with parsed identifiers.');
      const previews = [];
      for (const group of groups) {
        if (!group.pattern || new TextEncoder().encode(group.pattern).length > 512) throw new Error('Enter an identifier regex of at most 512 UTF-8 bytes for every group.');
        const response = await fetch('/api/action-preview', {method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': state.snapshot?.csrf_token || ''}, body: JSON.stringify({occurrence_id: id(group.anchor), kind, pattern: group.pattern})});
        const result = await responseJSON(response);
        if (version !== state.previewVersion || state.authPaused) return;
        if (!response.ok) throw new Error(result.error || 'A selected scope could not be previewed.');
        if (!Array.isArray(result.occurrences) || !result.occurrences.length || !result.occurrences.some(target => target.identity?.id === id(group.anchor)) || result.occurrences.some(target => !target.identity?.id || !target.identity?.revision || target.identity.channel !== group.channel || target.message?.source !== group.source || isReport(target)) || new Set(result.occurrences.map(id)).size !== result.occurrences.length) throw new Error('The server returned an invalid selected scope preview.');
        previews.push({...group, targets: result.occurrences.map(freezeTarget), until: result.until ? date(result.until) : date(nextSix())});
      }
      if (version !== state.previewVersion || state.authPaused) return;
      state.previewGroups = previews;
      state.previewTargets = previews.flatMap(group => group.targets);
      state.previewValid = state.previewTargets.length > 0 && state.previewTargets.length <= 10000;
      el('preview').textContent = (kind === 'close' ? 'Only these frozen current matches will be closed. Future matches are not closed.' : 'Matching current and future occurrences share one rule per listed channel/source until expiry.') + '\n\n' + previews.map(group => 'Channel ' + channelName() + ' / source ' + group.source + '\nAnchor: ' + displayTitle(group.anchor) + '\nPattern: ' + group.pattern + (kind === 'close' ? '' : '\nExpires: ' + group.until) + '\n' + group.targets.length + ' current occurrence(s):\n' + group.targets.map(target => '• ' + displayTitle(target) + ' · ' + date(target.message.reported_at)).join('\n')).join('\n\n');
      if (!state.previewValid) el('form-error').textContent = 'Choose between 1 and 10,000 current matches before confirming.';
      el('confirm').disabled = !state.previewValid;
    } catch (err) { if (version === state.previewVersion) el('preview').textContent = err.message || 'Preview unavailable.'; }
  }
  function batchKey(user) { return 'ops-alert-dashboard.close-plan.v1.' + encodeURIComponent(user); }
  const batchKinds = ['close', 'acknowledge', 'snooze'];
  const batchLabel = kind => ({close: 'close', acknowledge: 'WIP', snooze: 'snooze'})[kind] || 'action';
  /** Recover bounded homogeneous exact requests, including legacy close plans without scope. */
  function validBatch(plan, user) {
    if (!plan || typeof user !== 'string' || !user || plan.user !== user || !Array.isArray(plan.requests) || !plan.requests.length || plan.requests.length > 10000 || !Number.isInteger(plan.index) || plan.index < 0 || plan.index > plan.requests.length) return false;
    const kind = plan.requests[0]?.kind, regex = plan.requests[0]?.scope === 'regex';
    return batchKinds.includes(kind) && plan.requests.every(request => validPending(request) && request.kind === kind && (regex ? kind !== 'close' && request.scope === 'regex' && typeof request.pattern === 'string' && !!request.pattern && new TextEncoder().encode(request.pattern).length <= 512 : (request.scope === 'occurrence' || (kind === 'close' && !request.scope)) && !('pattern' in request)) && !('question' in request)) && new Set(plan.requests.map(request => request.request_id)).size === plan.requests.length && new Set(plan.requests.map(request => request.occurrence_id)).size === plan.requests.length;
  }
  /** Save batch progress before further writes so interruption cannot silently recreate requests. */
  function retainBatch(plan) {
    if (!validBatch(plan, plan?.user)) { error('The frozen action plan is invalid.'); return false; }
    try {
      const saved = JSON.stringify(plan); sessionStorage.setItem(batchKey(plan.user), saved);
      if (sessionStorage.getItem(batchKey(plan.user)) !== saved) throw new Error('Action plan was not retained.');
      state.batch = plan; return true;
    } catch { state.storageError = 'The frozen action plan could not be saved. Actions are paused to prevent duplicate writes.'; error(''); return false; }
  }
  /** Advance the frozen plan only after its exact request is confirmed. */
  function finishBatchRequest(request) {
    const plan = state.batch;
    if (!plan || plan.requests[plan.index]?.request_id !== request.request_id) return;
    retainBatch({...plan, index: plan.index + 1});
  }
  /** Discard only unissued batch intent without undoing already confirmed actions. */
  function discardBatch() {
    if (!state.batch || state.sending || state.pending || state.batch.user !== state.snapshot?.user) return;
    try {
      sessionStorage.removeItem(batchKey(state.batch.user));
      if (sessionStorage.getItem(batchKey(state.batch.user)) !== null) throw new Error('Action plan was not removed.');
      state.batch = null; render();
    } catch { state.storageError = 'The action plan could not be removed. Restore session storage before creating actions.'; error(''); }
  }
  /** Keep interrupted bulk progress and explicit recovery controls visible. */
  function renderBatch() {
    const plan = state.batch, authorized = plan?.user === state.snapshot?.user;
    const unavailable = plan && plan.index < plan.requests.length && !items().some(o => id(o) === plan.requests[plan.index].occurrence_id);
    el('batch-panel').hidden = !plan || !authorized;
    if (!plan || !authorized) return;
    el('batch-text').textContent = plan.index + ' of ' + plan.requests.length + ' frozen ' + batchLabel(plan.requests[0].kind) + ' requests confirmed. ' + (plan.index === plan.requests.length ? 'Plan complete.' : unavailable ? 'The next frozen occurrence is outside the retained snapshot. Discard the remaining plan after resolving any retained request.' : state.pending ? 'Resolve the retained request before continuing. No requests are replayed automatically.' : 'Review the current state, then explicitly resume to send the remaining frozen requests. A conflict stops the plan.');
    el('resume-batch').disabled = state.authPaused || state.sending || !!state.fetchError || !!state.storageError || state.snapshot?.status?.phase !== 'ready' || plan.index >= plan.requests.length || unavailable;
    el('cancel-batch').disabled = state.sending || !!state.pending; el('cancel-batch').textContent = plan.index === plan.requests.length ? 'Dismiss completed plan' : 'Discard remaining ' + batchLabel(plan.requests[0].kind) + ' plan';
  }
  /** Issue actions sequentially and stop immediately when confirmation or concurrency is uncertain. */
  async function resumeBatch() {
    const plan = state.batch;
    if (!validBatch(plan, state.snapshot?.user) || state.authPaused || state.sending || state.storageError || state.fetchError || state.snapshot?.status?.phase !== 'ready') return;
    while (state.batch && state.batch.index < state.batch.requests.length) {
      if (state.authPaused || state.sending || state.storageError || state.fetchError || state.snapshot?.status?.phase !== 'ready' || state.snapshot.user !== plan.user) break;
      const request = state.batch.requests[state.batch.index];
      if (!items().some(o => id(o) === request.occurrence_id)) { error('The next frozen occurrence is outside the retained snapshot. Resolve the retained request or discard the remaining plan.'); break; }
      if (state.pending && (state.pending.request_id !== request.request_id || Object.keys(state.pending).length !== Object.keys(request).length || Object.keys(request).some(key => state.pending[key] !== request[key]))) { error('Resolve the retained request before resuming this plan.'); break; }
      if (!state.pending) {
        if (!retainPending(plan.user, request)) break;
        state.pending = request; state.pendingUser = plan.user; state.pendingUnconfirmed = false;
      }
      const before = state.batch.index;
      await sendPending();
      if (state.pending || state.batch?.index === before || state.batch?.user !== plan.user) break;
    }
    render();
  }
  /** Retain only frozen identities and revisions; arrivals cannot join a confirmed plan. */
  function startActionBatch(kind, targets) {
    if (!batchKinds.includes(kind)) { error('This action does not support an exact plan.'); return; }
    if (!Array.isArray(targets) || !targets.length || targets.length > 10000) { error('Choose between 1 and 10,000 occurrences for one ' + batchLabel(kind) + ' plan. Narrow the filters or selection and try again.'); return; }
    if (!ready() || !globalThis.crypto?.randomUUID) return;
    if (new Set(targets.map(id)).size !== targets.length || targets.some(target => { const current = items().find(o => id(o) === id(target)); return !current || !openAlert(current) || current.identity.revision !== target.identity.revision; })) { error('A previewed occurrence changed. Review the current state before creating a plan.'); return; }
    const plan = {user: state.snapshot.user, index: 0, requests: targets.map(target => ({occurrence_id: id(target), kind, expected_revision: target.identity.revision, request_id: crypto.randomUUID(), ...(kind === 'close' ? {} : {scope: 'occurrence'})}))};
    if (retainBatch(plan)) { state.batchTargets = new Map(targets.map(target => [id(target), [id(target)]])); return resumeBatch(); }
  }
  function startRuleBatch(kind, groups) {
    if (!ready() || !batchKinds.includes(kind) || kind === 'close' || !globalThis.crypto?.randomUUID) return;
    const plan = {user: state.snapshot.user, index: 0, requests: groups.map(group => ({occurrence_id: id(group.anchor), kind, expected_revision: group.anchor.identity.revision, request_id: crypto.randomUUID(), scope: 'regex', pattern: group.pattern}))};
    if (retainBatch(plan)) { state.batchTargets = new Map(groups.map(group => [id(group.anchor), group.targets.map(id)])); return resumeBatch(); }
  }
  function startCloseBatch(targets) { return startActionBatch('close', targets); }
  let snapshotRequest = null, snapshotVersion = 0;
  /** Actions require a fresh read after any older in-flight snapshot has finished. */
  function refresh(options = {}) {
    if (state.authPaused && !options.reauthenticate) return Promise.resolve();
    if (state.fetching) return options.afterAction ? snapshotRequest.then(() => refresh(options)) : snapshotRequest;
    snapshotRequest = refreshSnapshot(options); return snapshotRequest;
  }
  /** Reject superseded responses before they can restore an old identity or pre-action state. */
  async function refreshSnapshot(options) {
    state.fetching = true; renderStatus();
    const generation = state.generation, selection = state.pendingSelection, version = snapshotVersion;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 12000);
    try {
      const response = await fetch('/api/snapshot', {credentials: 'same-origin', cache: 'no-store', signal: controller.signal, headers: {Accept: 'application/json'}});
      const snapshot = await responseJSON(response);
      if (generation !== state.generation || version !== snapshotVersion) return;
      if (!response.ok) throw new Error('Snapshot failed (HTTP ' + response.status + '). Actions are paused.');
      if (!snapshot.status || !Array.isArray(snapshot.occurrences) || typeof snapshot.user !== 'string' || !snapshot.user) throw new Error('The server returned an invalid snapshot. Actions are paused.');
      const previous = state.snapshot;
      if ((state.personalUser && state.personalUser !== snapshot.user) || (previous && previous.user !== snapshot.user)) {
        closeDesktopNotifications();
        el('action-error').hidden = true; el('action-error').textContent = ''; announce('');
        state.generation++; state.selected = ''; state.checked.clear(); state.selectedReport = ''; state.pending = null; state.pendingUser = ''; state.batch = null; state.storageUser = null;
        sopCache.clear(); metricsCache.clear(); sopDisclosures.clear(); metricsDisclosures.clear(); metricsSelections.clear(); state.reportDisclosures.clear(); state.groupDisclosures.clear(); state.prs = null;
        if (el('action-dialog').open) closeDialog();
      }
      state.authPaused = false; state.snapshot = snapshot; state.fetchError = '';
      chaiChat?.update(snapshot);
      slackThread?.update(snapshot);
      reconcilePersonal(snapshot, previous, options.quiet || !previous || options.reauthenticate); restorePending(snapshot.user); pruneSelection(); advanceSelection(selection);
      if (state.view === 'unmerged_prs') refreshPRs();
    } catch (err) { state.wasReady = false; state.fetchError = err.name === 'AbortError' ? 'Snapshot timed out after 12 seconds. Actions are paused; refresh to retry.' : err.message || 'Snapshot unavailable. Actions are paused.'; slackThread?.update(state.authPaused || !state.snapshot ? null : {...state.snapshot, status: {...state.snapshot.status, phase: 'degraded'}}); }
    finally { window.clearTimeout(timeout); state.fetching = false; render(); }
  }
  /** Retain an immutable request identity before its first network attempt. */
  async function sendNew(kind, occurrence, draft = '', scope = 'occurrence', targets = [occurrence]) {
    if (!ready()) return;
    if (!globalThis.crypto?.randomUUID) { error('A secure browser context with crypto.randomUUID is required for actions.'); return; }
    const request = {occurrence_id: id(occurrence), kind, expected_revision: occurrence.identity.revision, request_id: crypto.randomUUID()};
    if (['acknowledge', 'unacknowledge', 'snooze'].includes(kind)) request.scope = scope;
    if (['snooze', 'acknowledge'].includes(kind) && scope === 'regex') request.pattern = draft;
    if (kind === 'investigate') request.question = draft;
    const user = state.snapshot.user;
    if (!retainPending(user, request)) return;
    state.pending = request; state.pendingUser = user; state.pendingUnconfirmed = false; state.pendingTargets = targets.map(id);
    await sendPending();
  }
  /** Retain uncertain requests while allowing definite pre-write rejections to release a fresh guard. */
  async function sendPending() {
    if (!state.pending || pendingExpired() || state.sending || state.snapshot?.status?.phase !== 'ready' || state.fetchError || state.storageError || state.pendingUser !== state.snapshot.user) return;
    const request = state.pending, selection = actionSelection(request);
    const user = state.pendingUser, generation = state.generation;
    if (!retainPending(user, request)) return;
    const previouslyUnconfirmed = state.pendingUnconfirmed;
    state.pendingUnconfirmed = true;
    state.sending = true; error(''); announce('Saving request in the source thread…'); render();
    let refreshAfter = false;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 25000);
    try {
      const response = await fetch('/api/actions', {method: 'POST', credentials: 'same-origin', signal: controller.signal, headers: {'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': state.snapshot.csrf_token || ''}, body: JSON.stringify(request)});
      let result;
      try { result = await responseJSON(response); } catch { throw new Error('The server response could not be confirmed.'); }
      if (generation !== state.generation || state.snapshot?.user !== user) throw new Error('The authenticated operator changed before confirmation.');
      if (!response.ok) {
        const uncertain = result.code === 'delivery_uncertain' || (response.status >= 500 && !['not_ready', 'delivery_failed'].includes(result.code));
        const unresolved = uncertain || previouslyUnconfirmed;
        if (!unresolved) clearPending(user, request);
        error((result.error || 'Action failed (HTTP ' + response.status + ').') + (unresolved ? ' Delivery remains unconfirmed; retry the retained request only.' : ''));

        refreshAfter = result.code === 'conflict' || result.code === 'not_ready';
        announce('');
      } else {
        if (!result.occurrence?.identity) throw new Error('The server did not confirm the saved occurrence.');
        snapshotVersion++;
        if (selection && selection.version === state.selectionVersion) state.pendingSelection = selection;
        clearPending(user, request); if (!state.pending && !state.storageError) finishBatchRequest(request);
        const index = items().findIndex(o => id(o) === id(result.occurrence));
        if (index >= 0) state.snapshot.occurrences[index] = result.occurrence;
        pruneSelection();
        announce('Saved · ' + ({acknowledge: 'WIP started', unacknowledge: 'WIP released', close: 'Investigation closed', reopen: 'Investigation reopened', snooze: 'Snooze rule saved', unsnooze: 'Snooze rule canceled', investigate: 'Chai request saved'})[request.kind] + '.');
        refreshAfter = true;
      }
    } catch (err) { error((err.name === 'AbortError' ? 'Request timed out after 25 seconds.' : err.message || 'Request interrupted.') + ' Delivery is unconfirmed. The original request ID is retained for a safe retry.'); announce(''); }
    finally { window.clearTimeout(timeout); state.sending = false; render(); }
    if (refreshAfter) await refresh({afterAction: true});
  }
  /** Validate the editable draft before creating an explicitly confirmed request. */
  function submitAction(event) {
    event.preventDefault(); const o = state.dialogOccurrence; const draft = el('draft').value.trim();
    const questionError = state.dialogKind === 'investigate' ? chaiQuestionError(draft) : '';
    if (questionError) { el('form-error').textContent = questionError; el('draft').focus(); return; }
    if (state.dialogScope === 'regex' && !state.dialogTargets.length && !draft) { el('form-error').textContent = 'Enter an identifier regex before sending.'; el('draft').focus(); return; }
    if (!ready()) { el('form-error').textContent = 'Coverage or session is unavailable. Close this preview and refresh before sending.'; return; }
    if (items().find(current => id(current) === id(o))?.identity.revision !== o.identity.revision) { el('form-error').textContent = 'This occurrence changed while the preview was open. Close the preview and review the current state.'; return; }
    if (!state.previewValid) { el('form-error').textContent = 'Wait for a valid server scope preview before sending.'; return; }
    const kind = state.dialogKind, scope = state.dialogScope, targets = [...state.previewTargets], grouped = state.dialogTargets.length > 0, groups = [...state.previewGroups];
    if ((kind === 'close' || grouped) && [...targets, ...groups.map(group => group.anchor)].some(target => { const current = items().find(candidate => id(candidate) === id(target)); return !current || !openAlert(current) || current.identity.revision !== target.identity.revision; })) { el('form-error').textContent = 'A previewed occurrence changed. Close the preview and review the current state.'; return; }
    if (grouped && scope === 'regex' && (groups.length !== state.dialogGroups.length || groups.some((group, index) => group.pattern !== state.dialogGroups[index].pattern))) { el('form-error').textContent = 'The patterns changed. Review a fresh scope preview before sending.'; return; }
    closeDialog();
    if (grouped && scope === 'regex' && kind !== 'close') return startRuleBatch(kind, groups);
    if (scope === 'selected' || (kind === 'close' && scope === 'regex')) return startActionBatch(kind, targets);
    return sendNew(kind, o, draft, scope, targets);
  }
  el('refresh').addEventListener('click', () => refresh({quiet: true, reauthenticate: true}));
  el('mark-read').addEventListener('click', () => { for (const o of items()) markRead(o); render(); });
  el('notifications').addEventListener('click', toggleNotifications);
  el('resume-batch').addEventListener('click', resumeBatch); el('cancel-batch').addEventListener('click', discardBatch);
  for (const input of document.querySelectorAll('input[name="scope"]')) input.addEventListener('change', event => { state.dialogScope = event.target.value === 'occurrence' && state.dialogTargets.length ? 'selected' : event.target.value; updatePreview(); });
  el('retry').addEventListener('click', sendPending);
  el('dismiss-expired').addEventListener('click', dismissExpired);
  el('search').addEventListener('input', event => { selectionChanged(); state.query = event.target.value; rememberPreferences(); render(); });
  el('color').addEventListener('change', event => { selectionChanged(); state.color = event.target.value; rememberPreferences(); render(); });
  el('group-by-name').addEventListener('change', event => toggleGrouping(event.target.checked));
  el('draft').addEventListener('input', updatePreview);
  el('cancel').addEventListener('click', closeDialog); el('dialog-close').addEventListener('click', closeDialog);
  el('action-dialog').addEventListener('cancel', event => { event.preventDefault(); closeDialog(); });
  el('action-form').addEventListener('submit', submitAction);
  window.addEventListener('focus', () => refresh({quiet: true, reauthenticate: true}));
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh({quiet: true, reauthenticate: true}); });
  window.addEventListener('storage', event => {
    if (!state.personalUser || event.key !== personalKey(state.personalUser) || !event.newValue) return;
    try { const saved = JSON.parse(event.newValue); if (Array.isArray(saved.read)) { state.personal.read = saved.read.filter(value => state.personal.known.includes(value)); state.personal.notifications = saved.notifications === true; render(); } } catch { /* Other tabs cannot invalidate action safety storage. */ }
  });
  refresh({quiet: true}); window.setInterval(() => refresh(), 15000);
})();
