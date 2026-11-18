'use strict';
(() => {
  const maxMessageBytes = 4000;
  const maxStoredThreads = 8;
  const maxBlocks = 50;
  const maxRichDepth = 8;
  const maxRichNodes = 400;
  const maxInlineDepth = 6;
  const maxAliasDepth = 4;
  const refreshInterval = 15000;
  const minRetryDelay = 1000;
  // setTimeout treats delays past 2^31-1 milliseconds as immediate, so automatic retries stay inside a safe range.
  const maxRetryDelay = 2147483647;
  const bytes = value => new TextEncoder().encode(value).length;
  const storageKey = user => 'ops-alert-dashboard.thread.pending.v1.' + encodeURIComponent(user);
  const validPending = value => value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 3 && typeof value.request_id === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value.request_id) && typeof value.text === 'string' && !!value.text.trim() && bytes(value.text) <= 4000 && typeof value.occurrence_id === 'string' && !!value.occurrence_id;
  const instant = value => /^\d{10}(?:\.\d+)?$/.test(String(value)) ? new Date(Number(value) * 1000).toISOString() : value;
  /** Slack timestamps compare as fractional epoch seconds; the integer part keeps this exact. */
  const tsValue = value => { const n = Number(value); return Number.isFinite(n) ? n : 0; };
  const text = value => String(value ?? '');
  const decodeSlack = value => text(value).replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
  const emoji = {rotating_light: '🚨', warning: '⚠️', eyes: '👀', zzz: '💤', white_check_mark: '✅', heavy_check_mark: '✔️', fire: '🔥', x: '❌', red_circle: '🔴', large_orange_circle: '🟠', large_yellow_circle: '🟡', information_source: 'ℹ️', rocket: '🚀', tada: '🎉', thumbsup: '👍', '+1': '👍', thumbsdown: '👎', '-1': '👎', hourglass_flowing_sand: '⏳', construction: '🚧', bell: '🔔', no_entry: '⛔', heart: '❤️', pray: '🙏', smile: '😄', bug: '🐛', robot_face: '🤖', wave: '👋', clap: '👏', ok_hand: '👌', thinking_face: '🤔', point_right: '👉', raised_hands: '🙌', sparkles: '✨', star: '⭐', question: '❓', exclamation: '❗', sunglasses: '😎', seedling: '🌱'};
  /** Keep untrusted external links outside executable and credential-bearing URL schemes. */
  const safeURL = value => {
    if (typeof value !== 'string' || /[\u0000-\u0020\u007f-\u009f\\]/.test(value) || /^https:\/\/[^/?#]*@/i.test(value)) return null;
    try { const url = new URL(value); return url.protocol === 'https:' && !url.username && !url.password ? url : null; } catch { return null; }
  };
  /** Allow images only from the public Slack CDN families, the avatar host and safe raster data forms. */
  function safeImage(value) {
    if (typeof value !== 'string' || !value || /[\u0000-\u0020\u007f-\u009f\\]/.test(value)) return null;
    if (value.length <= 65536 && /^data:image\/(?:png|gif|jpeg|webp);base64,[A-Za-z0-9+/=]+$/.test(value)) return value;
    try {
      const url = new URL(value);
      if (url.protocol !== 'https:' || url.username || url.password || /[:@]/.test(value.match(/^https:\/\/([^/?#]+)/i)?.[1] || '')) return null;
      const host = url.hostname.toLowerCase();
      if (!/(?:^|\.)slack-edge\.com$/.test(host) && !/(?:^|\.)slack-imgs\.com$/.test(host) && !/(?:^|\.)slack\.com$/.test(host) && host !== 'secure.gravatar.com') return null;
      return url.href;
    } catch { return null; }
  }
  /** Accept only the documented thread and message envelopes before rendering untrusted data. */
  const validThread = value => !!value && typeof value === 'object' && !Array.isArray(value) && typeof value.occurrence_id === 'string' && !!value.occurrence_id && typeof value.root_ts === 'string' && !!value.root_ts && !!value.root && typeof value.root === 'object' && !Array.isArray(value.root) && Array.isArray(value.messages);
  const validMessage = value => !!value && typeof value === 'object' && !Array.isArray(value) && typeof value.timestamp === 'string' && !!value.timestamp;
  /** Automatic retries follow a positive server Retry-After in seconds within safe timer bounds; anything else keeps the steady cadence. */
  const nextRetryDelay = response => {
    const seconds = Number(response?.retryAfter);
    if (!Number.isFinite(seconds) || seconds <= 0) return refreshInterval;
    return Math.min(maxRetryDelay, Math.max(minRetryDelay, seconds * 1000));
  };
  /** Own one non-modal original-thread window with open-only Slack refresh, no workflow changes and no Chai routing. */
  function create(options) {
    const root = options.root, doc = root.ownerDocument || document;
    const make = (tag, nodeText, className) => { const element = doc.createElement(tag); if (nodeText != null) element.textContent = String(nodeText); if (className) element.className = className; return element; };
    const button = (label, action, className = 'thread-icon-button') => { const element = make('button', label, className); element.type = 'button'; element.addEventListener('click', action); return element; };
    const anchor = (url, label, className) => { const parsed = safeURL(url); if (!parsed) return null; const a = make('a', label, className); a.href = parsed.href; a.target = '_blank'; a.rel = 'noopener noreferrer'; return a; };
    const date = options.formatDate || (value => new Date(instant(value)).toLocaleString());
    const channelNameNow = () => (typeof options.channelName === 'function' ? text(options.channelName()) : text(options.channelName)) || 'Operations channel';
    const usergroupsNow = () => (typeof options.usergroups === 'function' ? options.usergroups() : options.usergroups) || {};
    const botName = text(options.botName) || 'the dashboard bot';
    const state = {user: '', snapshot: null, occurrence: null, threads: new Map(), drafts: new Map(), pending: null, pendingDraft: null, pendingUncertain: false, pendingRestored: false, sending: false, loading: false, loadingOlder: false, paused: false, error: '', storageError: '', generation: 0, threadRevision: 0, renderedKey: '', timer: null, retryDelay: refreshInterval, destroyed: false};
    const panel = make('section', null, 'thread-panel'); panel.id = 'alert-thread-panel'; panel.hidden = true; panel.tabIndex = -1; panel.setAttribute('role', 'region'); panel.setAttribute('aria-label', 'Original Slack discussion');
    const header = make('div', null, 'thread-header'), heading = make('div', null, 'thread-heading'); heading.append(make('strong', '💬 Slack discussion'), make('span', 'Original alert thread · shared cached history', 'thread-subtitle'));
    const controls = make('div', null, 'thread-controls'), close = button('✕', closePanel); close.setAttribute('aria-label', 'Close discussion'); close.className = 'thread-close';
    const refreshControl = button('↻ Refresh Slack conversation', () => refresh(), 'thread-refresh'); controls.append(refreshControl, close); header.append(heading, controls);
    const context = make('div', null, 'thread-context');
    const toolbar = make('div', null, 'thread-toolbar');
    const status = make('div', null, 'thread-status'); status.setAttribute('role', 'status');
    const errorBox = make('div', null, 'thread-error'); errorBox.setAttribute('role', 'alert');
    const body = make('div', null, 'thread-body');
    const loadOlder = button('↑ Load older messages', loadOlderPage, 'thread-load-older'); loadOlder.hidden = true;
    const retainedText = make('div', null, 'thread-pending-preview');
    const composer = make('div', null, 'thread-composer'), label = make('label', null, 'thread-composer-label'); label.htmlFor = 'alert-thread-draft';
    const mentionHint = make('p', null, 'thread-subtitle'); mentionHint.id = 'alert-thread-mentions';
    const draft = make('textarea', null, 'thread-draft'); draft.id = 'alert-thread-draft'; draft.rows = 3; draft.placeholder = 'Write a reply to the original thread…'; draft.setAttribute('aria-describedby', 'alert-thread-counter alert-thread-mentions');
    draft.addEventListener('input', () => { if (state.occurrence && !state.paused) { state.drafts.delete(state.occurrence.id); state.drafts.set(state.occurrence.id, draft.value); trimCache(state.drafts); } renderComposer(); });
    const footer = make('div', null, 'thread-composer-footer'), counter = make('span', null, 'thread-counter'); counter.id = 'alert-thread-counter';
    const send = button('Post reply', sendNew, 'thread-send'), retry = button('Retry retained reply', sendPending, 'thread-retry');
    const discard = button('Discard unresolved request after checking Slack', discardPending, 'thread-discard'); discard.hidden = true;
    footer.append(counter, send);
    panel.append(header, context, toolbar, status, errorBox, body, retainedText, retry, discard, composer);
    const entry = () => state.occurrence ? state.threads.get(state.occurrence.id) : null;
    const ready = () => !!state.user && !state.paused && !state.storageError && state.snapshot?.status?.phase === 'ready';
    /** Persist the exact delivery key before a write can reach Slack. */
    function savePending(value, uncertain = state.pendingUncertain) {
      try { const serialized = JSON.stringify({request: value, uncertain}); sessionStorage.setItem(storageKey(state.user), serialized); if (sessionStorage.getItem(storageKey(state.user)) !== serialized) throw new Error('Storage unavailable'); state.pending = value; state.pendingUncertain = uncertain; return true; }
      catch { state.storageError = 'The reply could not be retained in session storage. Sending is paused to prevent duplicates.'; render(); return false; }
    }
    /** Release a delivery guard only when its removal can be verified locally. */
    function clearPending() {
      try { sessionStorage.removeItem(storageKey(state.user)); if (sessionStorage.getItem(storageKey(state.user)) !== null) throw new Error('Storage unavailable'); state.pending = null; state.pendingDraft = null; state.pendingUncertain = false; state.pendingRestored = false; return true; }
      catch { state.storageError = 'The confirmed reply could not be cleared from session storage. Sending is paused.'; return false; }
    }
    /** Recover this operator's guarded reply without automatically resending it. */
    function restorePending() {
      try {
        const raw = sessionStorage.getItem(storageKey(state.user)); if (!raw) return;
        const retained = JSON.parse(raw); if (!validPending(retained.request) || typeof retained.uncertain !== 'boolean') throw new Error('Invalid retained request');
        state.pending = retained.request; state.pendingUncertain = retained.uncertain; state.pendingRestored = true;
      } catch { state.storageError = 'A retained thread reply could not be read safely. Restore session storage before sending.'; }
    }
    /** A trusted receipt or in-page confirmation must match the exact actor, destination and request. */
    const containsRequest = (thread, request) => thread.occurrence_id === request.occurrence_id && (
      (thread.confirmation?.occurrence_id === request.occurrence_id && thread.confirmation.request_id === request.request_id && thread.confirmation.actor === state.user) ||
      [thread.root, ...thread.messages].some(message => message?.request_id === request.request_id && message.actor === state.user));
    /** Resolve retained delivery uncertainty only from a matching confirmed thread message. */
    function reconcile(thread) {
      if (!state.pending) return;
      const request = state.pending, originalDraft = state.pendingDraft;
      if (!containsRequest(thread, request)) return;
      if (!clearPending()) return;
      if (originalDraft !== null && state.drafts.get(request.occurrence_id) === originalDraft) state.drafts.delete(request.occurrence_id);
      state.error = '';
    }
    /** Bound the private in-memory thread cache while keeping the opened occurrence resident. */
    function trimCache(cache) {
      while (cache.size > maxStoredThreads) {
        const oldest = [...cache.keys()].find(key => key !== state.occurrence?.id);
        if (oldest === undefined) return;
        cache.delete(oldest);
      }
    }
    /** Merge one server page: the latest page stays authoritative while older pages survive by timestamp. */
    function mergeThread(thread, older = false) {
      let view = state.threads.get(thread.occurrence_id);
      if (!view) {
        view = {root: null, replies: [], people: Object.create(null), emojis: Object.create(null), meta: {}, cursor: '', complete: false, version: 0, latestStart: null, olderLoaded: false};
        state.threads.set(thread.occurrence_id, view);
        trimCache(state.threads);
      }
      view.version++;
      view.root = validMessage(thread.root) ? thread.root : null;
      view.meta = {root_ts: thread.root_ts, channel_id: typeof thread.channel_id === 'string' ? thread.channel_id : '', slack_url: typeof thread.slack_url === 'string' ? thread.slack_url : '', fetched_at: typeof thread.fetched_at === 'string' ? thread.fetched_at : '', total_replies: Number.isFinite(Number(thread.total_replies)) ? Number(thread.total_replies) : view.replies.length, warnings: Array.isArray(thread.warnings) ? thread.warnings.filter(item => typeof item === 'string').slice(0, 20) : []};
      if (thread.people && typeof thread.people === 'object' && !Array.isArray(thread.people)) {
        for (const [id, person] of Object.entries(thread.people)) {
          if (!person || typeof person !== 'object' || typeof id !== 'string' || !id) continue;
          view.people[id] = {id, name: typeof person.name === 'string' ? person.name : '', avatar_url: typeof person.avatar_url === 'string' ? person.avatar_url : '', is_bot: person.is_bot === true};
        }
      }
      if (thread.emojis && typeof thread.emojis === 'object' && !Array.isArray(thread.emojis)) {
        for (const [name, value] of Object.entries(thread.emojis)) if (typeof name === 'string' && name && typeof value === 'string') view.emojis[name] = value;
      }
      const page = thread.messages.filter(validMessage).filter(message => message.timestamp !== thread.root_ts).sort((left, right) => tsValue(left.timestamp) - tsValue(right.timestamp));
      const boundary = page.length ? tsValue(page[0].timestamp) : Number.POSITIVE_INFINITY;
      const cursor = typeof thread.next_cursor === 'string' ? thread.next_cursor : '';
      const retainBefore = Math.min(view.latestStart ?? boundary, boundary);
      const retained = older ? view.replies : cursor ? view.replies.filter(message => tsValue(message.timestamp) < retainBefore) : [];
      const merged = new Map(retained.map(message => [message.timestamp, message]));
      for (const message of page) merged.set(message.timestamp, message);
      view.replies = [...merged.values()].sort((left, right) => tsValue(left.timestamp) - tsValue(right.timestamp));
      if (older) { view.olderLoaded = true; view.complete = !cursor; view.cursor = cursor; }
      else {
        // A moving recent window can leave a gap even after older history was fully loaded.
        const shifted = view.latestStart !== null && boundary > view.latestStart;
        view.latestStart = boundary;
        if (!cursor || !view.olderLoaded || shifted) { view.complete = !cursor; view.cursor = cursor; }
      }
    }
    /** Resolve a custom or standard emoji, degrading to the original shortcode. */
    function emojiNode(name, view, depth = 0) {
      const shortcode = text(name);
      const custom = view?.emojis?.[shortcode];
      if (typeof custom === 'string' && depth < maxAliasDepth) {
        const alias = custom.match(/^alias:([a-z0-9_+-]+)$/i);
        if (alias) return emojiNode(alias[1], view, depth + 1);
        const url = safeImage(custom);
        if (url) { const img = make('img', null, 'thread-emoji'); img.src = url; img.alt = ':' + shortcode + ':'; return img; }
      }
      if (Object.hasOwn(emoji, shortcode)) return make('span', emoji[shortcode]);
      return make('span', ':' + shortcode + ':');
    }
    const emojify = (value, view) => text(value).replace(/:([a-z0-9_+-]+):/g, (match, name) => Object.hasOwn(emoji, name) ? emoji[name] : match);
    /** Make known Slack user and channel references actionable without trusting arbitrary link hosts. */
    function referenceLink(kind, id, label, view) {
      const pattern = kind === '@' ? /^[UW][A-Za-z0-9_]{1,127}$/ : /^[CG][A-Za-z0-9_]{1,127}$/;
      const original = safeURL(view?.meta.slack_url);
      if (!pattern.test(text(id)) || !original || !/(?:^|\.)slack\.com$/.test(original.hostname)) return make('span', label, 'thread-mention');
      const path = kind === '@' ? '/team/' : '/archives/';
      return anchor(original.origin + path + encodeURIComponent(id), label, 'thread-mention thread-link');
    }
    /** Render one Slack angle-bracket token as a resolved mention, channel, group or safe link. */
    function specialToken(token, view) {
      const [target, supplied] = token.split('|');
      const person = target.match(/^@([A-Z0-9_]+)$/i);
      if (person) {
        const resolved = view?.people?.[person[1]];
        return referenceLink('@', person[1], '@' + (text(supplied).trim() || resolved?.name || person[1]), view);
      }
      const channel = target.match(/^#([A-Z0-9_]+)$/i);
      if (channel) return referenceLink('#', channel[1], '#' + (text(supplied).trim() || (channel[1] === view?.meta.channel_id ? channelNameNow() : channel[1])), view);
      const group = target.match(/^!subteam\^([A-Z0-9]+)$/i);
      if (group) { const groups = usergroupsNow(); return make('span', '@' + (text(supplied).trim() || (Object.hasOwn(groups, group[1]) ? text(groups[group[1]]).trim() : '') || group[1]), 'thread-mention'); }
      const broadcast = target.match(/^!(here|channel|everyone)$/i);
      if (broadcast) return make('span', '@' + broadcast[1], 'thread-mention');
      const label = text(supplied).trim() ? decodeSlack(supplied) : decodeSlack(target);
      return anchor(decodeSlack(target), emojify(label, view), 'thread-link') || make('span', emojify(decodeSlack(label), view));
    }
    /** Render supported Slack mrkdwn inline tokens as safe DOM nodes with bounded nesting. */
    function threadInline(container, source, view, depth = 0) {
      if (depth > maxInlineDepth) { container.append(make('span', emojify(decodeSlack(source), view))); return; }
      const tokens = /(`)([^`\n]+)`|<([^>\n]+)>|([*_~])([^\n]+?)\4|https:\/\/[^\s<>]+|:([a-z0-9_+-]+):/g;
      let cursor = 0;
      for (const match of source.matchAll(tokens)) {
        container.append(make('span', emojify(decodeSlack(source.slice(cursor, match.index)), view)));
        if (match[1]) container.append(make('code', decodeSlack(match[2])));
        else if (match[3]) container.append(specialToken(match[3], view));
        else if (match[4]) {
          const tag = {'*': 'strong', '_': 'em', '~': 's'}[match[4]], wrapper = make(tag);
          threadInline(wrapper, match[5], view, depth + 1); container.append(wrapper);
        } else if (match[6]) container.append(emojiNode(match[6], view));
        else container.append(anchor(decodeSlack(match[0]), decodeSlack(match[0]), 'thread-link') || make('span', emojify(match[0], view)));
        cursor = match.index + match[0].length;
      }
      container.append(make('span', emojify(decodeSlack(source.slice(cursor)), view)));
    }
    /** Preserve readable message structure: code fences, quotes, lists and line breaks. */
    function threadText(source, view) {
      const bodyNode = make('div', null, 'thread-text');
      const parts = text(source).split('```');
      parts.forEach((part, index) => {
        if (index % 2) { const pre = make('pre'); pre.append(make('code', decodeSlack(part.replace(/^\n/, '')))); bodyNode.append(pre); return; }
        const lists = [];
        for (const line of part.split('\n')) {
          const bullet = line.match(/^([ \t]*)(?:([•*-])|(\d+)[.)])[ \t]+(.+)$/);
          const number = bullet?.[3] === undefined ? null : Number(bullet[3]);
          if (bullet && (number === null || Number.isSafeInteger(number) && number <= 2147483647)) {
            const indent = bullet[1].replace(/\t/g, '    ').length, tag = number === null ? 'ul' : 'ol';
            while (lists.length && lists[lists.length - 1].indent > indent) lists.pop();
            if (lists.length && lists[lists.length - 1].indent === indent && lists[lists.length - 1].tag !== tag) lists.pop();
            let current = lists[lists.length - 1];
            if (!current || current.indent < indent) {
              if (lists.length >= maxRichDepth) { bodyNode.append(make('div', decodeSlack(line))); lists.length = 0; continue; }
              const list = make(tag);
              if (number !== null) list.setAttribute('start', String(number));
              (current?.lastItem || bodyNode).append(list);
              current = {indent, tag, list, lastItem: null}; lists.push(current);
            }
            const item = make('li');
            if (number !== null) item.setAttribute('value', String(number));
            threadInline(item, bullet[4], view); current.list.append(item); current.lastItem = item; continue;
          }
          lists.length = 0;
          const quote = line.match(/^&gt;\s?(.*)$|^>\s?(.*)$/);
          const row = make(quote ? 'blockquote' : 'div'); threadInline(row, quote ? quote[1] ?? quote[2] : line, view);
          if (!line) row.append(make('br')); bodyNode.append(row);
        }
      });
      return bodyNode;
    }
    /** Render the whitelisted rich_text leaves; unknown leaves are recorded, never interpreted. */
    function richLeaf(leaf, view, unsupported) {
      if (!leaf || typeof leaf !== 'object') return null;
      switch (leaf.type) {
        case 'plain_text': return make('span', decodeSlack(leaf.text));
        case 'mrkdwn': return threadText(leaf.text, view);
        case 'image': {
          const url = safeImage(leaf.image_url);
          if (!url) { unsupported.add('image with an untrusted URL'); return null; }
          const image = make('img', null, 'thread-emoji'); image.src = url; image.alt = text(leaf.alt_text); return image;
        }
        case 'text': {
          if (leaf.style?.code) return make('code', decodeSlack(leaf.text));
          let node = make('span', decodeSlack(leaf.text));
          for (const [flag, tag] of [['strike', 's'], ['italic', 'em'], ['bold', 'strong']]) if (leaf.style?.[flag]) { const wrapper = make(tag); wrapper.append(node); node = wrapper; }
          return node;
        }
        case 'link': {
          const label = typeof leaf.text === 'string' && leaf.text ? decodeSlack(leaf.text) : decodeSlack(leaf.url);
          return anchor(decodeSlack(leaf.url), label, 'thread-link') || make('span', label);
        }
        case 'user': return referenceLink('@', leaf.user_id, '@' + (text(leaf.name).trim() || view?.people?.[leaf.user_id]?.name || leaf.user_id || 'unknown user'), view);
        case 'usergroup': { const groups = usergroupsNow(); return make('span', '@' + ((Object.hasOwn(groups, leaf.usergroup_id) ? text(groups[leaf.usergroup_id]).trim() : '') || leaf.usergroup_id || 'unknown group'), 'thread-mention'); }
        case 'channel': return referenceLink('#', leaf.channel_id, '#' + (text(leaf.name).trim() || (leaf.channel_id === view?.meta.channel_id ? channelNameNow() : leaf.channel_id) || 'unknown channel'), view);
        case 'emoji': return emojiNode(leaf.name, view);
        case 'broadcast': return make('span', '@' + (text(leaf.range).trim() || 'channel'), 'thread-mention');
        case 'color': return make('span', text(leaf.value));
        case 'date': return make('span', text(leaf.fallback).trim() || date(instant(leaf.timestamp)));
        default: unsupported.add(String(leaf.type || 'unknown') + ' rich text element'); return null;
      }
    }
    /** Walk bounded rich_text containers: sections, lists, quotes and preformatted blocks. */
    function richElement(container, element, view, unsupported, depth, budget) {
      if (!element || typeof element !== 'object' || depth > maxRichDepth) { if (element) unsupported.add('deeply nested rich text'); return; }
      const children = Array.isArray(element.elements) ? element.elements : [];
      switch (element.type) {
        case 'rich_text': richElements(container, children, view, unsupported, depth + 1, budget); return;
        case 'rich_text_section': { const section = make('div', null, 'thread-rich-section'); richLeaves(section, children, view, unsupported, depth + 1, budget); container.append(section); return; }
        case 'rich_text_list':
          if (element.indent) unsupported.add('rich text list indentation without a parent');
          container.append(richList(element, view, unsupported, depth, budget)); return;
        case 'rich_text_quote': { const quote = make('blockquote'); richLeaves(quote, children, view, unsupported, depth + 1, budget); container.append(quote); return; }
        case 'rich_text_preformatted': { const pre = make('pre'); const codeNode = make('code'); richLeaves(codeNode, children, view, unsupported, depth + 1, budget, true); pre.append(codeNode); container.append(pre); return; }
        default: unsupported.add(String(element.type || 'unknown') + ' rich text element');
      }
    }
    function richLeaves(container, leaves, view, unsupported, depth, budget, plain = false) {
      for (const leaf of leaves) {
        if (++budget.count > maxRichNodes) { unsupported.add('truncated rich text'); return; }
        if (depth > maxRichDepth) { unsupported.add('deeply nested rich text'); return; }
        if (plain && leaf?.type === 'text') { container.append(make('span', decodeSlack(leaf.text))); continue; }
        const node = richLeaf(leaf, view, unsupported);
        if (node) container.append(node);
      }
    }
    /** Slack list offsets are zero-based; explicit item values preserve restarts when adjacent lists join. */
    function richList(element, view, unsupported, depth, budget) {
      const list = make(element.style === 'ordered' ? 'ol' : 'ul');
      if (!['ordered', 'bullet'].includes(element.style)) unsupported.add('unsupported rich text list style');
      let offset = element.offset ?? 0;
      if (!Number.isInteger(offset) || offset < 0 || offset > 2147483646) { unsupported.add('unsupported rich text list offset'); offset = 0; }
      if (element.style === 'ordered') list.setAttribute('start', String(offset + 1));
      for (const item of Array.isArray(element.elements) ? element.elements : []) {
        if (++budget.count > maxRichNodes) { unsupported.add('truncated rich text'); break; }
        if (item?.type !== 'rich_text_section') { unsupported.add('unsupported rich text list item'); continue; }
        const li = make('li');
        if (element.style === 'ordered' && !list.children.length) li.setAttribute('value', String(offset + 1));
        richLeaves(li, Array.isArray(item.elements) ? item.elements : [], view, unsupported, depth + 1, budget); list.append(li);
      }
      return list;
    }
    /** Adjacent Slack list elements encode nesting through indent, rather than nested elements arrays. */
    function richElements(container, elements, view, unsupported, depth, budget) {
      const lists = [];
      for (const element of elements) {
        if (++budget.count > maxRichNodes) { unsupported.add('truncated rich text'); return; }
        if (element?.type !== 'rich_text_list' || depth > maxRichDepth) {
          lists.length = 0; richElement(container, element, view, unsupported, depth, budget); continue;
        }
        let indent = element.indent ?? 0;
        if (!Number.isInteger(indent) || indent < 0 || indent >= maxRichDepth) { unsupported.add('unsupported rich text list indentation'); indent = 0; lists.length = 0; }
        while (lists.length && lists[lists.length - 1].indent > indent) lists.pop();
        const list = richList(element, view, unsupported, depth, budget);
        let current = lists[lists.length - 1];
        if ((!current && indent !== 0) || (current && indent > current.indent + 1)) { unsupported.add('rich text list indentation without a parent'); indent = 0; lists.length = 0; current = null; }
        if (current?.indent === indent && current.style === element.style) { current.list.append(...Array.from(list.children)); continue; }
        if (current?.indent === indent) { lists.pop(); current = lists[lists.length - 1]; }
        const parentItem = current?.list.children[current.list.children.length - 1];
        if (indent && !parentItem) { unsupported.add('rich text list indentation without a parent'); lists.length = 0; indent = 0; }
        (indent ? parentItem : container).append(list);
        lists.push({indent, style: element.style, list});
      }
    }
    /** Render the whitelisted block types; anything else is surfaced as unsupported, not dropped silently. */
    function renderBlocks(container, blocks, view, unsupported) {
      const list = Array.isArray(blocks) ? blocks.slice(0, maxBlocks) : [];
      if (Array.isArray(blocks) && blocks.length > list.length) unsupported.add('truncated blocks');
      for (const block of list) {
        if (!block || typeof block !== 'object') { unsupported.add('malformed block'); continue; }
        switch (block.type) {
          case 'section': {
            const section = make('div', null, 'thread-block-section');
            if (block.accessory) unsupported.add('section accessory');
            const bodyText = block.text && typeof block.text === 'object' && typeof block.text.text === 'string' ? block.text.text : '';
            if (bodyText && !['mrkdwn', 'plain_text'].includes(block.text.type)) unsupported.add('unsupported section text');
            if (bodyText) block.text.type === 'mrkdwn' ? section.append(threadText(bodyText, view)) : section.append(make('span', decodeSlack(bodyText)));
            if (Array.isArray(block.fields)) for (const field of block.fields.slice(0, 10)) {
              if (!field || typeof field !== 'object' || typeof field.text !== 'string') { unsupported.add('malformed section field'); continue; }
              if (!['mrkdwn', 'plain_text'].includes(field.type)) unsupported.add('unsupported section field');
              const row = make('div', null, 'thread-field');
              row.append(field.type === 'mrkdwn' ? threadText(field.text, view) : make('span', decodeSlack(field.text))); section.append(row);
            }
            container.append(section); break;
          }
          case 'header': container.append(make('h4', decodeSlack(typeof block.text === 'string' ? block.text : block.text?.text), 'thread-block-header')); break;
          case 'context': { const contextLine = make('div', null, 'thread-block-context'); richLeaves(contextLine, Array.isArray(block.elements) ? block.elements : [], view, unsupported, 1, {count: 0}); container.append(contextLine); break; }
          case 'divider': container.append(make('hr', null, 'thread-divider')); break;
          case 'image': {
            const url = safeImage(block.image_url);
            const caption = typeof block.title === 'string' ? block.title : typeof block.title?.text === 'string' ? block.title.text : '';
            if (url) { const img = make('img', null, 'thread-attachment-image'); img.src = url; img.alt = text(block.alt_text) || decodeSlack(caption) || 'Slack image'; container.append(img); }
            else unsupported.add('image with an untrusted URL');
            if (caption) container.append(make('span', decodeSlack(caption), 'thread-attachment-caption'));
            break;
          }
          case 'rich_text': richElement(container, block, view, unsupported, 0, {count: 0}); break;
          default: unsupported.add(String(block.type || 'unknown') + ' block');
        }
      }
    }
    const attachmentKeys = ['title', 'title_link', 'pretext', 'text', 'fallback', 'author_name', 'author_icon', 'author_link', 'footer', 'footer_icon', 'fields', 'image_url', 'thumb_url', 'blocks', 'color', 'id', 'mrkdwn_in', 'service_name'];
    /** Render ordinary attachment fields and flag interactive or media surfaces as unsupported. */
    function renderAttachment(container, attachment, view, unsupported) {
      if (!attachment || typeof attachment !== 'object' || Array.isArray(attachment)) { unsupported.add('malformed attachment'); return; }
      const article = make('article', null, 'thread-attachment');
      if (typeof attachment?.author_name === 'string' && attachment.author_name.trim()) {
        const line = make('div', null, 'thread-attachment-author');
        const icon = safeImage(attachment.author_icon);
        if (icon) { const img = make('img', null, 'thread-avatar'); img.src = icon; img.alt = ''; line.append(img); }
        line.append(anchor(attachment.author_link, decodeSlack(attachment.author_name), 'thread-link') || make('span', decodeSlack(attachment.author_name)));
        article.append(line);
      }
      if (typeof attachment?.pretext === 'string' && attachment.pretext.trim()) article.append(threadText(attachment.pretext, view));
      if (typeof attachment?.title === 'string' && attachment.title.trim()) article.append(anchor(attachment.title_link, decodeSlack(attachment.title), 'thread-link') || make('strong', decodeSlack(attachment.title)));
      if (typeof attachment?.text === 'string' && attachment.text.trim()) article.append(threadText(attachment.text, view));
      if (!attachment?.text && !attachment?.pretext && !attachment?.title && typeof attachment?.fallback === 'string') article.append(threadText(attachment.fallback, view));
      if (Array.isArray(attachment?.fields)) for (const field of attachment.fields.slice(0, 20)) {
        if (!field || typeof field !== 'object' || typeof field.value !== 'string') continue;
        const row = make('div', null, 'thread-field' + (field.short === true ? ' short' : ''));
        if (typeof field.title === 'string' && field.title) row.append(make('strong', decodeSlack(field.title)));
        row.append(threadText(field.value, view)); article.append(row);
      }
      const image = safeImage(attachment?.image_url) || safeImage(attachment?.thumb_url);
      if (image) { const img = make('img', null, 'thread-attachment-image'); img.src = image; img.alt = text(attachment?.title) || 'Slack attachment image'; article.append(img); }
      else if (attachment?.image_url || attachment?.thumb_url) unsupported.add('attachment image with an untrusted URL');
      if (typeof attachment?.footer === 'string' && attachment.footer.trim()) {
        const line = make('div', null, 'thread-attachment-footer');
        const icon = safeImage(attachment.footer_icon);
        if (icon) { const img = make('img', null, 'thread-avatar'); img.src = icon; img.alt = ''; line.append(img); }
        line.append(make('span', decodeSlack(attachment.footer))); article.append(line);
      }
      if (Array.isArray(attachment?.blocks)) renderBlocks(article, attachment.blocks, view, unsupported);
      if (attachment && typeof attachment === 'object') for (const key of Object.keys(attachment)) if (!attachmentKeys.includes(key)) unsupported.add('attachment content (' + key + ')');
      container.append(article);
    }
    /** One message with its author card, body, reactions and any unsupported-content notice. */
    function renderMessage(message, view, rootMessage) {
      const article = make('article', null, 'thread-message' + (rootMessage ? ' thread-root' : ''));
      const meta = make('div', null, 'thread-message-meta');
      const rawAuthor = message.author && typeof message.author === 'object' ? message.author : {};
      const knownAuthor = view.people[rawAuthor.id] || {};
      const author = {id: rawAuthor.id, name: rawAuthor.name || knownAuthor.name, avatar_url: rawAuthor.avatar_url || knownAuthor.avatar_url, is_bot: rawAuthor.is_bot ?? knownAuthor.is_bot};
      const avatar = safeImage(author.avatar_url);
      if (avatar) { const img = make('img', null, 'thread-avatar'); img.src = avatar; img.alt = ''; meta.append(img); }
      else meta.append(make('span', (text(author.name).trim() || text(author.id) || '?').slice(0, 1).toUpperCase(), 'thread-avatar thread-avatar-fallback'));
      meta.append(make('strong', decodeSlack(text(author.name).trim() || text(author.id) || 'Slack participant')));
      if (author.is_bot === true) meta.append(make('span', 'APP', 'thread-bot-label'));
      if (typeof message.actor === 'string' && message.actor.trim()) meta.append(make('span', message.actor + ' via dashboard', 'thread-actor'));
      meta.append(make('time', date(instant(message.timestamp))));
      if (message.edited === true) meta.append(make('span', 'edited', 'thread-edited'));
      article.append(meta);
      const messageBody = make('div', null, 'thread-message-body');
      const unsupported = new Set(Array.isArray(message.unsupported) ? message.unsupported.filter(item => typeof item === 'string') : []);
      const blocks = Array.isArray(message.blocks) ? message.blocks : [];
      if (blocks.length) renderBlocks(messageBody, blocks, view, unsupported);
      if (!messageBody.children.length && typeof message.text === 'string' && message.text) messageBody.append(threadText(message.text, view));
      if (Array.isArray(message.attachments)) for (const attachment of message.attachments.slice(0, 10)) renderAttachment(messageBody, attachment, view, unsupported);
      if (message.attachments?.length > 10) unsupported.add('truncated attachments');
      article.append(messageBody);
      if (Array.isArray(message.reactions)) {
        const row = make('div', null, 'thread-reactions');
        for (const reaction of message.reactions.slice(0, 50)) {
          if (!reaction || typeof reaction !== 'object' || typeof reaction.name !== 'string' || !reaction.name) continue;
          const users = (Array.isArray(reaction.users) ? reaction.users.slice(0, 50) : []).map(user => text(view?.people?.[user]?.name).trim() || text(user)).filter(Boolean);
          const chip = make('span', null, 'thread-reaction');
          chip.setAttribute('title', users.length ? users.join(', ') : reaction.name);
          const count = Math.max(0, Number(reaction.count) || 0);
          chip.setAttribute('aria-label', ':' + reaction.name + ': reaction, ' + count + (users.length ? ' by ' + users.join(', ') : ''));
          chip.append(emojiNode(reaction.name, view), make('span', String(count)));
          row.append(chip);
        }
        if (row.children.length) article.append(row);
      }
      if (unsupported.size) {
        const notice = make('p', null, 'thread-unsupported');
        notice.append(make('span', 'Not shown here: ' + [...unsupported].join(', ') + '. Open the original thread in Slack for the full message.'));
        const original = anchor(view.meta.slack_url, ' Open in Slack', 'thread-link') || anchor(state.occurrence?.slackURL, ' Open in Slack', 'thread-link');
        if (original) notice.append(original);
        article.append(notice);
      }
      return article;
    }
    /** Render the cached conversation for the opened occurrence, keeping scroll when data is unchanged. */
    function renderThread() {
      const view = entry();
      const key = state.occurrence ? state.occurrence.id + ':' + (view?.version || 0) : '';
      const nearBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 64;
      const previousScroll = body.scrollTop;
      body.replaceChildren();
      if (!state.occurrence) return;
      if (!view) { body.append(make('p', state.loading ? 'Loading the cached conversation…' : 'The cached conversation has not been loaded.', 'thread-empty')); return; }
      if (view.root) body.append(renderMessage(view.root, view, true));
      else body.append(make('p', 'The original root message is no longer retained.', 'thread-empty'));
      const replies = make('div', null, 'thread-replies');
      for (const message of view.replies) replies.append(renderMessage(message, view, false));
      if (view.replies.length) body.append(replies);
      else body.append(make('p', 'No retained replies.', 'thread-empty'));
      body.append(loadOlder);
      for (const warning of view.meta.warnings) body.append(make('p', warning, 'thread-unsupported'));
      body.scrollTop = nearBottom ? body.scrollHeight : previousScroll;
      state.renderedKey = key;
    }
    /** Keep edits available while readiness and retained requests control sending. */
    function renderComposer() {
      const count = bytes(draft.value.trim());
      draft.disabled = !state.occurrence || state.paused || !state.user;
      counter.textContent = count + ' / 4000 bytes';
      draft.setAttribute('aria-invalid', count > maxMessageBytes ? 'true' : 'false');
      send.disabled = !state.occurrence || !ready() || !!state.pending || state.sending || count === 0 || count > maxMessageBytes;
      retainedText.hidden = !state.pending || state.paused;
      retainedText.textContent = state.pending && !state.paused ? 'Retained reply for ' + state.pending.occurrence_id + ': ' + state.pending.text : '';
      if (state.pending && !state.paused) {
        const pendingID = state.pending.occurrence_id;
        const occurrence = state.snapshot?.occurrences?.find(item => item?.identity?.id === pendingID);
        const original = anchor(state.threads.get(pendingID)?.meta.slack_url, ' Open retained reply thread in Slack', 'thread-link') || anchor(occurrence?.message?.slack_url || (state.occurrence?.id === pendingID ? state.occurrence.slackURL : ''), ' Open retained reply thread in Slack', 'thread-link');
        retainedText.append(original || make('span', ' Select the original alert and check its source thread in Slack before discarding.'));
      }
      retry.hidden = !state.pending;
      retry.disabled = !ready() || state.sending;
      discard.hidden = !state.pending || state.sending;
    }
    /** Render panel state without ever rebuilding the composer or stealing its focus. */
    function render() {
      if (state.destroyed) return;
      panel.hidden = !state.open;
      context.textContent = state.occurrence ? 'Alert · ' + state.occurrence.title : '';
      const view = entry();
      toolbar.replaceChildren();
      const slackLink = anchor(view?.meta?.slack_url, '↗ Open in Slack', 'thread-link') || anchor(state.occurrence?.slackURL, '↗ Open in Slack', 'thread-link');
      if (slackLink) toolbar.append(slackLink);
      if (view && Number.isFinite(view.meta.total_replies)) toolbar.append(make('span', view.replies.length + ' of ' + view.meta.total_replies + ' replies shown', 'thread-subtitle'));
      status.textContent = state.paused ? 'Session expired. Refresh the dashboard to sign in again.' : state.sending ? 'Posting your reply…' : state.pending ? 'Delivery is unconfirmed. Retry only checks for this exact reply; it cannot repost it. Check Slack before explicitly discarding an unresolved request.' : !state.user ? 'Waiting for your session…' : state.loading ? 'Loading the cached conversation…' : state.loadingOlder ? 'Loading earlier messages…' : view ? view.meta.fetched_at ? 'Cached conversation · synced ' + date(view.meta.fetched_at) + ' · Auto-refreshes from Slack while open.' : 'Cached conversation.' : 'Not loaded.';
      errorBox.textContent = state.storageError || state.error;
      errorBox.hidden = !errorBox.textContent;
      refreshControl.disabled = state.loading || state.loadingOlder || state.sending || state.paused || !state.user || !state.occurrence;
      loadOlder.hidden = !view || view.complete || !view.cursor || state.loadingOlder;
      loadOlder.disabled = state.loading || state.loadingOlder || state.sending;
      loadOlder.textContent = state.loadingOlder ? 'Loading earlier messages…' : '↑ Load older messages';
      label.textContent = 'Reply as ' + botName + ' on behalf of ' + (state.user || 'the current operator');
      mentionHint.textContent = 'Use workspace @handles, #' + channelNameNow() + ', or Slack references such as <@USERID> and <#CHANNELID>. The shared user directory may need time to load; retry explicitly if asked. Unknown names are rejected; quote references as code to send them literally.';
      const desired = state.occurrence ? state.drafts.get(state.occurrence.id) || '' : '';
      if (draft.value !== desired) draft.value = desired;
      renderComposer();
      const key = state.occurrence ? state.occurrence.id + ':' + (view?.version || 0) : '';
      if (key !== state.renderedKey || !body.children.length) renderThread();
    }
    /** Bound transport time without interpreting a timeout as a rejected write. */
    async function transport(method, requestBody) {
      const controller = new AbortController(), timeout = window.setTimeout(() => controller.abort(), method === 'GET' ? 12000 : 25000);
      try { return await options.request(method, requestBody, controller.signal); } finally { window.clearTimeout(timeout); }
    }
    /** Arm the one automatic refresh timer only while the panel is open, visible, authenticated and ready. */
    function scheduleAuto() {
      window.clearTimeout(state.timer); state.timer = null;
      if (!state.destroyed && state.open && state.occurrence && state.user && !state.paused && !doc.hidden && state.snapshot?.status?.phase === 'ready') state.timer = window.setTimeout(autoRefresh, state.retryDelay);
    }
    /** One automatic fresh read per armed tick; the next read is never overlapping and never a post. */
    function autoRefresh() {
      state.timer = null;
      if (state.destroyed || !state.open || state.paused || !state.user || !state.occurrence || doc.hidden || state.snapshot?.status?.phase !== 'ready') return;
      load(state.occurrence.id, '');
    }
    /** Suspend automatic refreshes while the tab is hidden and read fresh data as soon as it is visible again. */
    function visibility() {
      if (doc.hidden) { window.clearTimeout(state.timer); state.timer = null; }
      else if (state.open) refresh();
    }
    /** Remove private display state when the authenticated identity is no longer verified. */
    function pause() {
      clearPrivate(); state.paused = true;
      render();
      options.onAuthError?.();
    }
    /** Leave actor-bound durable guards in storage but remove all private in-memory display data. */
    function clearPrivate() {
      state.generation++; state.error = ''; state.storageError = '';
      window.clearTimeout(state.timer); state.timer = null; state.retryDelay = refreshInterval;
      state.threads.clear(); state.drafts.clear(); state.occurrence = null; state.pending = null;
      state.pendingDraft = null; state.pendingUncertain = false; state.pendingRestored = false; state.snapshot = null;
      state.loading = false; state.loadingOlder = false; state.sending = false; state.renderedKey = '';
      body.replaceChildren();
    }
    /** Read the latest page as fresh Slack data, or one cache-only older page; superseded reads can never overwrite confirmed state. */
    async function load(occurrenceID, before = '') {
      if (state.destroyed || state.paused || !state.user || !occurrenceID || state.loading || state.loadingOlder || state.sending) return;
      const generation = state.generation, revision = state.threadRevision;
      const older = !!before;
      if (older) state.loadingOlder = true; else state.loading = true;
      render();
      try {
        const response = await transport('GET', older ? {occurrence_id: occurrenceID, before} : {occurrence_id: occurrenceID, refresh: true});
        if (state.destroyed || generation !== state.generation || revision !== state.threadRevision || state.occurrence?.id !== occurrenceID) return;
        if (response.status === 401 || response.status === 403) { pause(); return; }
        if (!response.ok) {
          state.error = response.body?.error || 'The cached conversation could not be loaded.';
          if (!older) state.retryDelay = nextRetryDelay(response);
          return;
        }
        const responseBody = response.body;
        if (responseBody?.user !== state.user) { pause(); return; }
        const thread = responseBody?.thread;
        if (!validThread(thread) || thread.occurrence_id !== occurrenceID) throw new Error('The server returned an invalid discussion snapshot.');
        mergeThread(thread, older);
        reconcile(thread);
        state.error = '';
        if (!older) state.retryDelay = refreshInterval;
      } catch (err) {
        if (generation === state.generation && revision === state.threadRevision) {
          state.error = err.name === 'AbortError' ? 'Loading the cached conversation timed out. Your draft is preserved.' : err.message || 'The cached conversation is unavailable.';
          if (!older) state.retryDelay = refreshInterval;
        }
      } finally {
        if (generation === state.generation) { state.loading = false; state.loadingOlder = false; scheduleAuto(); }
        render();
      }
    }
    /** Load the previous cached page only through this explicit control. */
    function loadOlderPage() {
      const view = entry();
      if (!view || view.complete || !view.cursor || state.loading || state.loadingOlder || state.sending) return;
      return load(state.occurrence.id, view.cursor);
    }
    /** Manually refresh the currently opened conversation with fresh Slack data. */
    function refresh() {
      if (state.destroyed || state.paused || !state.user || !state.occurrence || state.loading || state.loadingOlder || state.sending) return;
      return load(state.occurrence.id, '');
    }
    /** Freeze one explicit reply and its destination before allocating a delivery key. */
    async function sendNew() {
      const replyText = draft.value.trim();
      if (!ready() || !state.occurrence || state.pending || state.sending || !replyText || bytes(replyText) > maxMessageBytes) return;
      if (!globalThis.crypto?.randomUUID) { state.error = 'A secure browser context is required to create a request ID.'; render(); return; }
      const payload = {occurrence_id: state.occurrence.id, request_id: crypto.randomUUID(), text: replyText};
      if (!savePending(payload, false)) return;
      state.pendingDraft = draft.value;
      state.pendingRestored = false;
      await sendPending();
    }
    /** Reuse the retained delivery key; uncertain or restored requests only ask the server to reconcile. */
    async function sendPending() {
      if (!ready() || !state.pending || state.sending) return;
      const payload = state.pending, originalDraft = state.pendingDraft, user = state.user, generation = state.generation;
      const previouslyUncertain = state.pendingUncertain;
      const reconcile = previouslyUncertain || state.pendingRestored;
      if (!savePending(payload, true)) return;
      state.sending = true; state.error = ''; render();
      try {
        const response = await transport('POST', reconcile ? {...payload, reconcile_only: true} : payload);
        if (state.destroyed || generation !== state.generation || user !== state.user) return;
        if (response.status === 401 || response.status === 403) { pause(); return; }
        if (!response.ok) {
          const code = response.body?.code;
          if (!reconcile && ['validation', 'not_found', 'not_ready', 'conflict', 'delivery_failed'].includes(code)) clearPending();
          state.error = code === 'delivery_uncertain'
            ? 'The server could not confirm this reply and will not repost it. Check the original thread in Slack; if the reply is missing, discard the retained request and send it again.'
            : response.body?.error || 'Delivery could not be confirmed. Retry asks the server to reconcile the retained reply.';
          return;
        }
        const responseBody = response.body;
        if (responseBody?.user !== state.user) { pause(); return; }
        const thread = responseBody?.thread;
        if (!validThread(thread) || !containsRequest(thread, payload)) throw new Error('The server did not confirm the reply.');
        state.threadRevision++;
        if (!clearPending()) return;
        if (originalDraft !== null && state.drafts.get(payload.occurrence_id) === originalDraft) state.drafts.delete(payload.occurrence_id);
        mergeThread(thread);
        state.error = '';
      } catch (err) {
        if (generation === state.generation) state.error = (err.name === 'AbortError' ? 'Request timed out.' : err.message || 'Request interrupted.') + ' Delivery is unconfirmed. The original request ID is retained for a reconcile retry.';
      } finally {
        if (generation === state.generation) { state.sending = false; scheduleAuto(); }
        render();
      }
    }
    /** Explicitly drop an unresolved guard only after the operator confirms they checked Slack. */
    function discardPending() {
      if (!state.pending || state.sending) return;
      const question = 'Discard the unresolved reply after checking Slack?\n\nRequest ' + state.pending.request_id + ' is not confirmed. Discarding never sends or undoes anything in Slack. You can then write and explicitly send a new reply.';
      const confirmed = typeof options.confirm === 'function' ? options.confirm(question) : window.confirm?.(question);
      if (confirmed !== true) return;
      if (!clearPending()) return;
      state.error = 'The unresolved request was discarded. Delivery stays unconfirmed; nothing was sent or undone. Send a new reply explicitly when ready.';
      render();
    }
    /** Open the discussion for one occurrence; every open reads fresh Slack data while cached pages, drafts and guards survive. */
    function open(occurrence) {
      if (state.destroyed || state.paused || !state.user) return;
      state.open = true;
      const occurrenceID = occurrence?.occurrenceID || occurrence?.identity?.id;
      if (typeof occurrenceID === 'string' && occurrenceID) {
        if (state.occurrence?.id !== occurrenceID) { state.generation++; state.loading = false; state.loadingOlder = false; state.sending = false; state.error = ''; }
        const existing = state.threads.get(occurrenceID);
        if (existing) { state.threads.delete(occurrenceID); state.threads.set(occurrenceID, existing); }
        state.occurrence = {id: occurrenceID, title: text(occurrence.title).trim() || 'Selected alert', slackURL: text(occurrence.slackURL).trim()};
        load(occurrenceID, '');
      }
      render(); panel.focus();
    }
    /** Hide the panel and stop automatic refreshes while keeping cached pages, drafts and any retained request. */
    function closePanel() {
      state.open = false; window.clearTimeout(state.timer); state.timer = null; render();
      const control = doc.getElementById?.('thread-open-' + state.occurrence?.id);
      if (control) control.focus();
    }
    /** Apply identity and readiness only; snapshot updates never fetch discussion, erase drafts or reset an armed refresh timer. */
    function update(snapshot) {
      const user = typeof snapshot?.user === 'string' ? snapshot.user : '';
      const paused = !user || snapshot?.authPaused === true;
      const reset = user !== state.user || paused !== state.paused;
      if (reset) { clearPrivate(); state.user = user; }
      state.snapshot = paused ? null : snapshot; state.paused = paused;
      if (reset && !paused) restorePending();
      if (!state.timer) scheduleAuto();
      render();
    }
    /** Invalidate outstanding reads before removing this window, its timer and its listeners. */
    function destroy() { state.destroyed = true; state.generation++; window.clearTimeout(state.timer); state.timer = null; doc.removeEventListener('visibilitychange', visibility); root.replaceChildren(); }
    composer.append(label, draft, mentionHint, footer);
    root.append(panel);
    doc.addEventListener('visibilitychange', visibility);
    render();
    return {update, open, close: closePanel, refresh, destroy};
  }
  window.OpsSlackThread = {create};
})();
