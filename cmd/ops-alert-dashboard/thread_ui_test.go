package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// threadUnitHarness runs the isolated discussion controller with deterministic DOM, storage and transport fakes.
const threadUnitHarness = `
const fs = require('node:fs'), vm = require('node:vm');
class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.attributes = {}; this.handlers = {}; this.value = ''; this._text = ''; this.scrollHeight = 0; this.scrollTop = 0; this.clientHeight = 0; }
  set textContent(value) { this._text = String(value); this.replaceChildren(); }
  get textContent() { return this._text; }
  set innerHTML(value) { throw new Error('HTML rendering is forbidden'); }
  append(...children) { for (const child of children) { if (child.parent) child.parent.children = child.parent.children.filter(item => item !== child); child.parent = this; this.children.push(child); } }
  replaceChildren(...children) { for (const child of this.children) child.parent = null; this.children = []; this.append(...children); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, handler) { this.handlers[name] = handler; }
  focus() { this.focused = true; }
}
const store = new Map(), timers = new Map(); let timerID = 0;
const doc = {hidden: false, handlers: {}, createElement: tag => new Element(tag), getElementById: () => null, addEventListener(name, fn) {this.handlers[name] = fn;}, removeEventListener(name) {delete this.handlers[name];}};
const sandbox = {URL, Date, TextEncoder, AbortController, Map, Set, console, document: doc, crypto: {randomUUID: () => '11111111-1111-4111-8111-111111111111'}, sessionStorage: {getItem: key => store.get(key) ?? null, setItem: (key, value) => store.set(key, value), removeItem: key => store.delete(key)}, window: {setTimeout: (fn, delay) => {timers.set(++timerID, {fn, delay}); return timerID;}, clearTimeout: id => timers.delete(id)}};
const context = vm.createContext(sandbox); context.store = store; context.timers = timers;
vm.runInContext(fs.readFileSync('web/threads.js', 'utf8'), context);
vm.runInContext(` + "`" + `
globalThis.root = document.createElement('div');
globalThis.calls = []; globalThis.queue = []; globalThis.authErrors = 0; globalThis.confirmed = false;
globalThis.key = user => 'ops-alert-dashboard.thread.pending.v1.' + user;
globalThis.requestID = '11111111-1111-4111-8111-111111111111';
globalThis.stamp = n => '1790000000.' + String(n).padStart(6, '0');
globalThis.message = (n, extra = {}) => ({timestamp: stamp(n), text: 'Reply ' + n, author: {id: 'U1', name: 'Alice', avatar_url: 'https://avatars.slack-edge.com/alice.png'}, ...extra});
globalThis.thread = (id = 'one', messages = [], extra = {}) => ({occurrence_id: id, channel_id: 'C123', root_ts: stamp(1), slack_url: 'https://workspace.slack.com/archives/C123/p1790000000000001', fetched_at: '2026-11-05T10:00:00Z', root: message(1, {text: 'Root ' + id}), messages, next_cursor: '', total_replies: messages.length, people: {}, emojis: {}, warnings: [], ...extra});
globalThis.reply = (value = thread(), status = 200, user = 'alice') => ({ok: status < 400, status, body: status < 400 ? {user, csrf_token: 'token', thread: value} : value});
globalThis.transport = async (method, body, signal) => {calls.push({method, body: JSON.parse(JSON.stringify(body)), retained: Array.from(store.values()), signal}); const value = queue.shift(); if (value instanceof Error) throw value; return value || reply(thread(body.occurrence_id));};
globalThis.setup = () => {globalThis.controller = window.OpsSlackThread.create({root, request: transport, botName: 'Dashboard bot', channelName: 'ops', usergroups: {S123: 'operators'}, formatDate: value => 'Local ' + value, confirm: () => confirmed, onAuthError: () => authErrors++}); controller.update({user: 'alice', status: {phase: 'ready'}}); return controller;};
globalThis.walk = (node = root) => [node, ...node.children.flatMap(child => walk(child))];
globalThis.findAll = (className, parent = root) => walk(parent).filter(node => (node.className || '').split(' ').includes(className));
globalThis.find = className => findAll(className)[0];
globalThis.tags = tag => walk().filter(node => node.tag === tag);
globalThis.content = node => node.textContent + node.children.map(content).join('');
globalThis.input = value => {find('thread-draft').value = value; find('thread-draft').handlers.input();};
globalThis.click = className => find(className).handlers.click();
globalThis.settle = async () => {for (let i = 0; i < 12; i++) await Promise.resolve();};
globalThis.fireTimers = async () => {const pending = [...timers.values()]; timers.clear(); for (const timer of pending) timer.fn(); await settle();};
globalThis.open = async (id = 'one') => {controller.open({occurrenceID: id, title: 'Alert ' + id, slackURL: 'https://workspace.slack.com/archives/C123/p1790000000000001'}); await settle();};
globalThis.posts = () => calls.filter(call => call.method === 'POST');
globalThis.messages = () => findAll('thread-message-body').map(content);
globalThis.confirmation = (id = 'one', actor = 'alice') => reply(thread(id, [message(9, {request_id: requestID, actor})]));
globalThis.receipt = (extra = {}) => ({occurrence_id: 'one', request_id: requestID, actor: 'alice', ...extra});
globalThis.setupRetained = () => {store.set(key('alice'), JSON.stringify({request: {occurrence_id: 'one', request_id: requestID, text: 'Old reply'}, uncertain: true})); setup();};
globalThis.checkReceipt = async (proof, method = 'POST', user = 'alice', target = 'one') => {setupRetained(); await open(); queue.push(reply(thread(target, [], {confirmation: proof}), 200, user)); if (method === 'GET') await controller.refresh(); else await click('thread-retry'); return [store.size, posts().length, find('thread-retry').hidden, authErrors];};
` + "`" + `, context);
Promise.resolve(vm.runInContext('(async () => {' + process.argv[1] + '})()', context)).then(value => process.stdout.write(JSON.stringify(value)), error => {console.error(error); process.exitCode = 1;});
`

// TestSlackThreadController covers original-thread rendering and explicit read/write lifecycles.
func TestSlackThreadController(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable for thread controller unit tests")
	}
	for _, tc := range []struct {
		name       string
		expression string
		want       string
	}{
		{
			name:       "mount nonmodal panel and read fresh data on every explicit open",
			expression: `setup(); const initial = [root.children.length, calls.length, find('thread-panel').hidden, find('thread-panel').attributes.role]; await open(); controller.close(); await open(); return [initial, calls.map(call => call.method), calls.map(call => call.body), timers.size, find('thread-panel').hidden];`,
			want:       `[[1,0,true,"region"],["GET","GET"],[{"occurrence_id":"one","refresh":true},{"occurrence_id":"one","refresh":true}],1,false]`,
		},
		{
			name:       "snapshot updates and draft edits never refresh or post",
			expression: `setup(); await open(); input('Private draft'); const textarea = find('thread-draft'); textarea.focus(); for (let i = 0; i < 10; i++) controller.update({user: 'alice', status: {phase: 'ready'}}); controller.close(); await open(); return [calls.length, posts().length, find('thread-draft') === textarea, textarea.focused, textarea.value, timers.size];`,
			want:       `[2,0,true,true,"Private draft",1]`,
		},
		{
			name:       "automatic timer fires one fresh read updates the view and reschedules without posting",
			expression: `setup(); await open(); const body = find('thread-body'); body.scrollTop = 0; body.scrollHeight = 200; body.clientHeight = 100; queue.push(reply(thread('one', [message(2, {text: 'Late reply'})]))); await fireTimers(); return [calls.length, calls[1].body, messages(), body.scrollTop, posts().length, timers.size, [...timers.values()][0].delay];`,
			want:       `[2,{"occurrence_id":"one","refresh":true},["Root one","Late reply"],0,0,1,15000]`,
		},
		{
			name:       "closing the panel stops the automatic refresh timer",
			expression: `setup(); await open(); controller.close(); return [timers.size, find('thread-panel').hidden];`,
			want:       `[0,true]`,
		},
		{
			name:       "hidden tab suspends the timer and returning reads fresh data immediately",
			expression: `setup(); await open(); document.hidden = true; document.handlers.visibilitychange(); const hidden = [timers.size, calls.length]; document.hidden = false; document.handlers.visibilitychange(); await settle(); return [hidden, calls.length, calls.at(-1).body, timers.size];`,
			want:       `[[0,1],2,{"occurrence_id":"one","refresh":true},1]`,
		},
		{
			name:       "auth pause stops the timer until a new explicit open",
			expression: `setup(); await open(); controller.update({user: 'alice', authPaused: true}); const paused = timers.size; controller.update({user: 'alice', status: {phase: 'ready'}}); return [paused, timers.size, find('thread-refresh').disabled];`,
			want:       `[0,0,true]`,
		},
		{
			name:       "degraded snapshot reads once without arming the automatic timer",
			expression: `setup(); controller.update({user: 'alice', status: {phase: 'degraded'}}); await open(); return [calls.length, calls[0].body, timers.size];`,
			want:       `[1,{"occurrence_id":"one","refresh":true},0]`,
		},
		{
			name:       "recovered readiness restarts the timer without resetting a pending one",
			expression: `setup(); controller.update({user: 'alice', status: {phase: 'degraded'}}); await open(); const degraded = timers.size; controller.update({user: 'alice', status: {phase: 'ready'}}); const scheduled = [...timers.keys()]; controller.update({user: 'alice', status: {phase: 'ready'}}); return [degraded, timers.size, [...timers.keys()][0] === scheduled[0], [...timers.values()][0].delay];`,
			want:       `[0,1,true,15000]`,
		},
		{
			name:       "periodic snapshot updates never reset or defer the armed timer",
			expression: `setup(); await open(); const before = [...timers.entries()].map(([id, timer]) => [id, timer.delay]); for (let i = 0; i < 10; i++) controller.update({user: 'alice', status: {phase: 'ready'}}); return [timers.size, JSON.stringify(before) === JSON.stringify([...timers.entries()].map(([id, timer]) => [id, timer.delay])), calls.length];`,
			want:       `[1,true,1]`,
		},
		{
			name:       "manual refresh requests fresh Slack data for the opened occurrence",
			expression: `setup(); await open(); await controller.refresh(); return [calls.length, calls[1].body];`,
			want:       `[2,{"occurrence_id":"one","refresh":true}]`,
		},
		{
			name:       "failed refresh keeps the conversation draft and last synced time while delaying the retry",
			expression: `setup(); queue.push(reply(thread('one', [message(2)], {fetched_at: '2026-11-05T09:00:00Z'}))); await open(); input('Keep'); const synced = find('thread-status').textContent; queue.push(reply({error: 'Slack discussion could not be loaded (not_ready); displayed messages may be stale'}, 503)); await controller.refresh(); return [[...timers.values()][0].delay, messages(), find('thread-draft').value, find('thread-error').textContent, find('thread-status').textContent === synced];`,
			want:       `[15000,["Root one","Reply 2"],"Keep","Slack discussion could not be loaded (not_ready); displayed messages may be stale",true]`,
		},
		{
			name:       "short server Retry-After is honored so shared budget retries are not starved",
			expression: `setup(); await open(); queue.push({...reply({error: 'Busy'}, 503), retryAfter: 1}); await controller.refresh(); return [[...timers.values()][0].delay, messages(), find('thread-error').hidden];`,
			want:       `[1000,["Root one"],false]`,
		},
		{
			name:       "long or invalid Retry-After values stay within safe timer bounds",
			expression: `setup(); await open(); queue.push({...reply({error: 'Busy'}, 503), retryAfter: '999999999999'}); await controller.refresh(); const huge = [...timers.values()][0].delay; queue.push({...reply({error: 'Busy'}, 503), retryAfter: Infinity}); await controller.refresh(); const infinite = [...timers.values()][0].delay; queue.push({...reply({error: 'Busy'}, 503), retryAfter: 'later'}); await controller.refresh(); const invalid = [...timers.values()][0].delay; queue.push({...reply({error: 'Busy'}, 503), retryAfter: '-5'}); await controller.refresh(); return [huge, infinite, invalid, [...timers.values()][0].delay];`,
			want:       `[2147483647,15000,15000,15000]`,
		},
		{
			name:       "retry after a throttled refresh recovers the normal cadence",
			expression: `setup(); await open(); queue.push({...reply({error: 'Busy'}, 503), retryAfter: 1}); await fireTimers(); const retry = [...timers.values()][0].delay; await fireTimers(); return [retry, calls.length, calls.at(-1).body, [...timers.values()][0].delay];`,
			want:       `[1000,3,{"occurrence_id":"one","refresh":true},15000]`,
		},
		{
			name:       "late automatic read cannot re-arm polling after close hide auth failure or destroy",
			expression: `setup(); await open(); const results = []; let resolve; queue.push(new Promise(done => {resolve = done;})); await fireTimers(); controller.close(); resolve(reply(thread('one', [message(2)]))); await settle(); results.push(['close', timers.size]); await open(); queue.push(new Promise(done => {resolve = done;})); await fireTimers(); document.hidden = true; document.handlers.visibilitychange(); resolve(reply(thread('one'))); await settle(); results.push(['hidden', timers.size]); document.hidden = false; document.handlers.visibilitychange(); await settle(); results.push(['visible', calls.length, timers.size]); queue.push(new Promise(done => {resolve = done;})); await fireTimers(); controller.update({user: 'alice', authPaused: true}); resolve(reply(thread('one'))); await settle(); results.push(['auth', timers.size]); controller.update({user: 'alice', status: {phase: 'ready'}}); await open(); queue.push(new Promise(done => {resolve = done;})); await fireTimers(); controller.destroy(); resolve(reply(thread('one'))); await settle(); results.push(['destroy', timers.size, root.children.length]); return results;`,
			want:       `[["close",0],["hidden",0],["visible",5,1],["auth",0],["destroy",0,0]]`,
		},
		{
			name:       "late auto refresh response cannot restore a previous occurrence view",
			expression: `setup(); await open(); let resolve; queue.push(new Promise(done => {resolve = done;})); await fireTimers(); await open('two'); input('Second'); resolve(reply(thread('one', [message(2, {text: 'Stale secret'})]))); await settle(); return [calls.map(call => call.body.occurrence_id), messages(), find('thread-draft').value];`,
			want:       `[["one","one","two"],["Root two"],"Second"]`,
		},
		{
			name:       "reopening a cached occurrence replaces the view with fresh data",
			expression: `setup(); await open(); controller.close(); queue.push(reply(thread('one', [message(2, {text: 'New reply'})]))); await open(); return [calls.length, messages()];`,
			want:       `[2,["Root one","New reply"]]`,
		},
		{
			name:       "automatic refresh reconciles a retained reply without any post",
			expression: `setupRetained(); await open(); queue.push(confirmation()); await fireTimers(); return [posts().length, store.size, find('thread-retry').hidden, calls.length, calls[1].body];`,
			want:       `[0,0,true,2,{"occurrence_id":"one","refresh":true}]`,
		},
		{
			name:       "original root deduped and replies sorted chronologically",
			expression: `setup(); queue.push(reply(thread('one', [message(4), message(1), message(2), message(2, {text: 'Edited two'})]))); await open(); return [messages(), findAll('thread-root').length];`,
			want:       `[["Root one","Edited two","Reply 4"],1]`,
		},
		{
			name:       "older pagination appends without erasing recent page",
			expression: `setup(); queue.push(reply(thread('one', [message(5), message(6)], {next_cursor: stamp(5)}))); await open(); queue.push(reply(thread('one', [message(2), message(3)], {next_cursor: stamp(2)}))); await click('thread-load-older'); await settle(); return [messages(), calls.at(-1).body, find('thread-load-older').hidden];`,
			want:       `[["Root one","Reply 2","Reply 3","Reply 5","Reply 6"],{"occurrence_id":"one","before":"1790000000.000005"},false]`,
		},
		{
			name:       "refresh replaces authoritative recent edits and deletes but keeps older pages",
			expression: `setup(); queue.push(reply(thread('one', [message(5), message(6)], {next_cursor: stamp(5)}))); await open(); queue.push(reply(thread('one', [message(2), message(3)]))); await click('thread-load-older'); await settle(); queue.push(reply(thread('one', [message(6, {text: 'Six edited'}), message(7)], {next_cursor: stamp(6)}))); await controller.refresh(); return [messages(), find('thread-load-older').hidden];`,
			want:       `[["Root one","Reply 2","Reply 3","Six edited","Reply 7"],false]`,
		},
		{
			name:       "refresh moving past a previously complete window exposes pagination to fill the gap",
			expression: `setup(); queue.push(reply(thread('one', [message(5), message(6)], {next_cursor: stamp(5)}))); await open(); queue.push(reply(thread('one', [message(2), message(3)]))); await click('thread-load-older'); queue.push(reply(thread('one', [message(100), message(101)], {next_cursor: stamp(100)}))); await controller.refresh(); const hidden = find('thread-load-older').hidden; queue.push(reply(thread('one', [message(6), message(7)], {next_cursor: stamp(6)}))); await click('thread-load-older'); return [hidden, calls.at(-1).body.before, messages()];`,
			want:       `[false,"1790000000.000100",["Root one","Reply 2","Reply 3","Reply 6","Reply 7","Reply 100","Reply 101"]]`,
		},
		{
			name:       "complete latest page removes deleted older history",
			expression: `setup(); queue.push(reply(thread('one', [message(5)], {next_cursor: stamp(5)}))); await open(); queue.push(reply(thread('one', [message(2)]))); await click('thread-load-older'); await settle(); queue.push(reply(thread('one'))); await controller.refresh(); return messages();`,
			want:       `["Root one"]`,
		},
		{
			name:       "switch while loading starts new read and ignores stale first response",
			expression: `setup(); let resolve; queue.push(new Promise(done => {resolve = done;})); controller.open({occurrenceID: 'one'}); await open('two'); input('Second draft'); resolve(reply(thread('one', [message(2, {text: 'Stale secret'})]))); await settle(); return [calls.map(call => call.body.occurrence_id), messages(), find('thread-draft').value, find('thread-refresh').disabled];`,
			want:       `[["one","two"],["Root two"],"Second draft",false]`,
		},
		{
			name:       "per occurrence drafts and cached conversation survive switching",
			expression: `setup(); await open(); input('First draft'); await open('two'); input('Second draft'); await open('one'); return [find('thread-draft').value, messages(), calls.length];`,
			want:       `["First draft",["Root one"],3]`,
		},
		{
			name:       "thread and draft caches evict oldest after eight occurrences",
			expression: `setup(); for (let i = 0; i < 9; i++) {await open('item' + i); input('Draft ' + i);} await open('item1'); const kept = find('thread-draft').value; await open('item0'); return [calls.length, kept, find('thread-draft').value];`,
			want:       `[11,"Draft 1",""]`,
		},
		{
			name:       "loading errors recover refresh controls and preserve draft",
			expression: `setup(); queue.push(new Error('Offline')); await open(); input('Keep'); await controller.refresh(); return [find('thread-refresh').disabled, find('thread-draft').value, messages(), find('thread-error').hidden];`,
			want:       `[false,"Keep",["Root one"],true]`,
		},
		{
			name:       "loading older errors preserve page and restore pagination control",
			expression: `setup(); queue.push(reply(thread('one', [message(5)], {next_cursor: stamp(5)}))); await open(); queue.push(reply({error: 'Unavailable'}, 503)); await click('thread-load-older'); await settle(); return [messages(), find('thread-load-older').hidden, find('thread-load-older').disabled, find('thread-error').textContent];`,
			want:       `[["Root one","Reply 5"],false,false,"Unavailable"]`,
		},
		{
			name:       "actual bot author and separately verified operator attribution",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {author: {id: 'B1', name: 'Real Slack Bot', is_bot: true, avatar_url: 'https://secure.gravatar.com/avatar/bot'}, actor: 'alice', edited: true})]))); await open(); return [findAll('thread-message-meta').map(content), tags('img').map(node => node.src), find('thread-composer-label').textContent];`,
			want:       `[["AliceLocal 2026-09-21T14:13:20.000Z","Real Slack BotAPPalice via dashboardLocal 2026-09-21T14:13:20.000Zedited"],["https://avatars.slack-edge.com/alice.png","https://secure.gravatar.com/avatar/bot"],"Reply as Dashboard bot on behalf of alice"]`,
		},
		{
			name:       "mention metadata custom emoji aliases and read only reactions",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: '<@U2> :custom: :alias: :eyes:', reactions: [{name: 'alias', count: 2, users: ['U2']}]})], {people: {U2: {name: 'Bob'}}, emojis: {custom: 'https://emoji.slack-edge.com/team/custom.png', alias: 'alias:custom'}}))); await open(); return [messages()[1], tags('img').filter(node => node.className === 'thread-emoji').map(node => [node.src, node.alt]), find('thread-reaction').attributes, find('thread-reaction').tag];`,
			want:       `["@Bob   👀",[["https://emoji.slack-edge.com/team/custom.png",":custom:"],["https://emoji.slack-edge.com/team/custom.png",":custom:"],["https://emoji.slack-edge.com/team/custom.png",":custom:"]],{"title":"Bob","aria-label":":alias: reaction, 2 by Bob"},"span"]`,
		},
		{
			name:       "metadata persists across pages and hostile prototype keys remain data",
			expression: `setup(); const people = JSON.parse('{"__proto__":{"name":"Not a prototype"},"constructor":{"name":"Literal constructor"}}'); const emojis = JSON.parse('{"__proto__":"alias:eyes","constructor":"alias:eyes"}'); queue.push(reply(thread('one', [], {people, emojis}))); await open(); queue.push(reply(thread('one', [message(2, {author: {id: 'constructor'}, text: ':constructor: :__proto__: :toString:'})]))); await controller.refresh(); return [messages()[1], content(findAll('thread-message-meta')[1]), ({}).name || null];`,
			want:       `["👀 👀 :toString:","LLiteral constructorLocal 2026-09-21T14:13:20.000Z",null]`,
		},
		{
			name:       "alias cycles terminate at visible shortcode",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: ':a:'})], {emojis: {a: 'alias:b', b: 'alias:a'}}))); await open(); return messages()[1];`,
			want:       `":a:"`,
		},
		{
			name:       "safe rich text root sections lists quotes preformatted and styles",
			expression: `setup(); const t = value => ({type: 'text', text: value}); const blocks = [{type: 'rich_text', elements: [{type: 'rich_text_section', elements: [{...t('Bold'), style: {bold: true}}, {type: 'user', user_id: 'U2'}, {type: 'emoji', name: 'eyes'}]}, {type: 'rich_text_list', style: 'ordered', elements: [{type: 'rich_text_section', elements: [t('List item')]}]}, {type: 'rich_text_quote', elements: [t('Quote')]}, {type: 'rich_text_preformatted', elements: [t('<script> code')]}]}]; queue.push(reply(thread('one', [message(2, {blocks})], {people: {U2: {name: 'Bob'}}}))); await open(); return [messages()[1], tags('strong').map(content).includes('Bold'), tags('ol').length, tags('blockquote').map(content), tags('code').map(content)];`,
			want:       `["Bold@Bob👀List itemQuote<script> code",true,1,["Quote"],["<script> code"]]`,
		},
		{
			name:       "mrkdwn ordered starts explicit numbers and nested mixed lists retain structure",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: '3. Third\n4. Fourth\n  - Nested bullet\n    7) Nested ordered\n    9) Explicit nine\n5. Fifth\n- Unordered'})]))); await open(); return [tags('ol').map(node => [node.attributes.start, node.parent.tag, node.children.map(item => item.attributes.value)]), tags('ul').map(node => node.parent.tag), tags('ol')[0].children.map(content)];`,
			want:       `[[["3","div",["3","4","5"]],["7","li",["7","9"]]],["li","div"],["Third","FourthNested bulletNested orderedExplicit nine","Fifth"]]`,
		},
		{
			name:       "mrkdwn deeply nested list fallback preserves literal markers without unbounded nesting",
			expression: `setup(); const lines = Array.from({length: 10}, (_, index) => ' '.repeat(index * 2) + '- Item ' + index); queue.push(reply(thread('one', [message(2, {text: lines.join('\n')})]))); await open(); return [messages()[1].includes('                - Item 8'), tags('ul').length <= 9, tags('li').length < 10];`,
			want:       `[true,true,true]`,
		},
		{
			name:       "rich ordered offsets start at offset plus one and nested sibling indents retain hierarchy",
			expression: `setup(); const section = text => ({type: 'rich_text_section', elements: [{type: 'text', text}]}); const list = (style, indent, offset, labels) => ({type: 'rich_text_list', style, indent, offset, elements: labels.map(section)}); const blocks = [{type: 'rich_text', elements: [list('ordered', 0, 4, ['A', 'B']), list('bullet', 1, 0, ['Nested']), list('ordered', 2, 6, ['Deep']), list('ordered', 0, 6, ['C', 'D']), list('ordered', 0, 0, ['Restart'])]}]; queue.push(reply(thread('one', [message(2, {blocks})]))); await open(); return [tags('ol').map(node => [node.attributes.start, node.parent.tag, node.children.map(item => item.attributes.value || null)]), tags('ul').map(node => node.parent.tag), tags('ol')[0].children.map(content), findAll('thread-unsupported').length];`,
			want:       `[[["5","div",["5",null,"7",null,"1"]],["7","li",["7"]]],["li"],["A","BNestedDeep","C","D","Restart"],0]`,
		},
		{
			name:       "documented rich bullet lists resume the parent after a nested list",
			expression: `setup(); const list = (indent, labels) => ({type: 'rich_text_list', style: 'bullet', indent, elements: labels.map(text => ({type: 'rich_text_section', elements: [{type: 'text', text}]}))}); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'rich_text', elements: [list(0, ['Hashbrowns', 'Eggs']), list(1, ['Scrambled', 'Over easy']), list(0, ['Pancakes'])]}]})]))); await open(); return [tags('ul').map(node => [node.parent.tag, node.children.length]), tags('ul')[0].children.map(content)];`,
			want:       `[[["div",3],["li",2]],["Hashbrowns","EggsScrambledOver easy","Pancakes"]]`,
		},
		{
			name:       "invalid rich list offsets and orphaned or excessive indent are visibly unsupported",
			expression: `setup(); const list = (offset, indent) => ({type: 'rich_text_list', style: 'ordered', offset, indent, elements: [{type: 'rich_text_section', elements: [{type: 'text', text: 'Readable'}]}]}); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'rich_text', elements: [list(4, 3), list(-1, 0), list('4', 0), list(0, 999), list(0, 1.5)]}]})]))); await open(); return [content(find('thread-unsupported')).includes('indentation without a parent'), content(find('thread-unsupported')).includes('unsupported rich text list offset'), content(find('thread-unsupported')).includes('unsupported rich text list indentation'), tags('ol').length < 10, messages()[1].includes('Readable')];`,
			want:       `[true,true,true,true,true]`,
		},
		{
			name:       "image block standard plain text title becomes safe visible caption and fallback alt",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'image', image_url: 'https://files.slack.com/image.png', title: {type: 'plain_text', text: 'Caption &lt;script&gt;'}}, {type: 'image', image_url: 'https://files.slack.com/other.png', title: {type: 'plain_text', text: 'Other caption'}, alt_text: 'Explicit description'}]})]))); await open(); return [findAll('thread-attachment-caption').map(content), findAll('thread-attachment-image').map(node => node.alt), tags('script').length];`,
			want:       `[["Caption <script>","Other caption"],["Caption <script>","Explicit description"],0]`,
		},
		{
			name:       "section fields and context use Slack text objects not attachment shape",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'section', text: {type: 'mrkdwn', text: '*Heading*'}, fields: [{type: 'plain_text', text: 'Plain field'}, {type: 'mrkdwn', text: '*Rich field*'}]}, {type: 'context', elements: [{type: 'plain_text', text: 'Plain context'}, {type: 'mrkdwn', text: '_Rich context_'}]}, {type: 'header', text: {type: 'plain_text', text: 'Header'}}, {type: 'divider'}]})]))); await open(); return [messages()[1], tags('strong').map(content).includes('Rich field'), tags('em').map(content), tags('hr').length];`,
			want:       `["HeadingPlain fieldRich fieldPlain contextRich contextHeader",true,["Rich context"],1]`,
		},
		{
			name:       "ordinary attachment fields footer images and fallback are readable",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: '', attachments: [{author_name: 'Sender', author_link: 'https://example.org/author', pretext: 'Before', title: 'Title', title_link: 'https://example.org/title', text: 'Body', fields: [{title: 'Field', value: 'Value', short: true}], footer: 'Footer', image_url: 'https://files.slack.com/image.png'}, {fallback: 'Fallback only'}]})]))); await open(); return [messages()[1], tags('a').map(node => node.href).includes('https://example.org/title'), find('thread-attachment-image').src];`,
			want:       `["SenderBeforeTitleBodyFieldValueFooterFallback only",true,"https://files.slack.com/image.png"]`,
		},
		{
			name:       "unsupported blocks attachments and leaves show original Slack fallback",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: 'Fallback text', blocks: [{type: 'actions'}], attachments: [{actions: [{url: 'javascript:evil()'}]}], unsupported: ['video']})]))); await open(); return [messages()[1], content(find('thread-unsupported')), tags('button').filter(node => content(node).includes('evil')).length, tags('a').every(node => node.href.startsWith('https:'))];`,
			want:       `["Fallback text","Not shown here: video, actions block, attachment content (actions). Open the original thread in Slack for the full message. Open in Slack",0,true]`,
		},
		{
			name:       "rich node budget stops oversized untrusted trees visibly",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'rich_text', elements: [{type: 'rich_text_section', elements: Array.from({length: 1000}, () => ({type: 'text', text: 'x'}))}]}]})]))); await open(); return [messages()[1].length < 500, content(find('thread-unsupported')).includes('truncated rich text')];`,
			want:       `[true,true]`,
		},
		{
			name:       "hostile HTML and URLs never create executable DOM or unapproved images",
			expression: `setup(); const bad = ['javascript:alert(1)', 'http://example.org/a', 'https://user:pass@example.org/a', 'https://example.org/\npath', 'https://evil.example/a.png', 'https://slack-edge.com.evil.example/a.png', 'data:image/svg+xml;base64,PHN2Zz4=']; queue.push(reply(thread('one', [message(2, {text: '<img src=x onerror=evil()> <javascript:evil()|bad> <https://user:pass@example.org/|credentials>', author: {id: 'U1', avatar_url: bad[4]}, blocks: [], attachments: bad.map(image_url => ({image_url}))})], {slack_url: bad[0]}))); await open(); return [tags('script').length, tags('img').map(node => node.src), tags('a').map(node => node.href), messages()[1].includes('img src=x onerror=evil()')];`,
			want:       `[0,["https://avatars.slack-edge.com/alice.png"],["https://workspace.slack.com/archives/C123/p1790000000000001","https://workspace.slack.com/archives/C123/p1790000000000001"],true]`,
		},
		{
			name:       "safe raster data fixture and explicit Slack CDN images are allowed",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {blocks: ['data:image/png;base64,aGVsbG8=', 'https://a.slack-imgs.com/a.png', 'https://secure.gravatar.com/avatar/a'].map(image_url => ({type: 'image', image_url, alt_text: 'Fixture'}))})]))); await open(); return findAll('thread-attachment-image').map(node => [node.src, node.alt]);`,
			want:       `[["data:image/png;base64,aGVsbG8=","Fixture"],["https://a.slack-imgs.com/a.png","Fixture"],["https://secure.gravatar.com/avatar/a","Fixture"]]`,
		},
		{
			name:       "explicit post stores exact UUID target and text with uncertainty before transport",
			expression: `setup(); await open(); input('Plain reply'); queue.push(confirmation()); await click('thread-send'); return [posts()[0].body, JSON.parse(posts()[0].retained[0]), store.size, find('thread-draft').value, posts().length];`,
			want:       `[{"occurrence_id":"one","request_id":"11111111-1111-4111-8111-111111111111","text":"Plain reply"},{"request":{"occurrence_id":"one","request_id":"11111111-1111-4111-8111-111111111111","text":"Plain reply"},"uncertain":true},0,"",1]`,
		},
		{
			name:       "edited draft during delivery survives matching confirmation",
			expression: `setup(); await open(); input('Original'); let resolve; queue.push(new Promise(done => {resolve = done;})); const sending = click('thread-send'); input('Next reply'); resolve(confirmation()); await sending; return [find('thread-draft').value, store.size, find('thread-send').disabled];`,
			want:       `["Next reply",0,false]`,
		},
		{
			name:       "uncertain retry reuses immutable request with reconcile only despite edited draft",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Connection lost')); await click('thread-send'); input('Edited'); queue.push(confirmation()); await click('thread-retry'); return [posts().map(call => call.body), store.size, find('thread-draft').value];`,
			want:       `[[{"occurrence_id":"one","request_id":"11111111-1111-4111-8111-111111111111","text":"Original"},{"occurrence_id":"one","request_id":"11111111-1111-4111-8111-111111111111","text":"Original","reconcile_only":true}],0,"Edited"]`,
		},
		{
			name:       "initial definitive rejection releases guard but retains editable draft",
			expression: `setup(); await open(); input('Original'); queue.push(reply({code: 'validation', error: 'Invalid reply'}, 400)); await click('thread-send'); return [store.size, find('thread-draft').value, find('thread-send').disabled, find('thread-error').textContent];`,
			want:       `[0,"Original",false,"Invalid reply"]`,
		},
		{
			name:       "later definitive rejection never releases prior uncertain guard",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); queue.push(reply({code: 'delivery_failed', error: 'Not found'}, 502)); await click('thread-retry'); return [store.size, find('thread-send').disabled, posts()[1].body.reconcile_only];`,
			want:       `[1,true,true]`,
		},
		{
			name:       "restored even known unsent request only reconciles and never auto sends",
			expression: `store.set(key('alice'), JSON.stringify({request: {occurrence_id: 'one', request_id: requestID, text: 'Retained'}, uncertain: false})); setup(); await open(); const before = posts().length; queue.push(reply({code: 'delivery_uncertain'}, 503)); await click('thread-retry'); return [before, posts()[0].body.reconcile_only, store.size, find('thread-error').textContent.includes('will not repost')];`,
			want:       `[0,true,1,true]`,
		},
		{
			name:       "pending never retargets when operator switches occurrences",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); await open('two'); input('Second draft'); queue.push(confirmation()); await click('thread-retry'); return [posts().map(call => call.body.occurrence_id), messages(), find('thread-draft').value, store.size];`,
			want:       `[["one","one"],["Root two"],"Second draft",0]`,
		},
		{
			name:       "late post after switching cannot confirm or repopulate stale view",
			expression: `setup(); await open(); input('Original'); let resolve; queue.push(new Promise(done => {resolve = done;})); const sending = click('thread-send'); await open('two'); resolve(confirmation()); await sending; return [messages(), store.size, find('thread-retry').disabled, find('thread-refresh').disabled];`,
			want:       `[["Root two"],1,false,false]`,
		},
		{
			name:       "restored old receipt reconciles without adding a message to the latest page or changing pagination",
			expression: `crypto.randomUUID = () => {throw new Error('Unexpected new request ID');}; setupRetained(); const current = Array.from({length: 30}, (_, index) => message(index + 100)); const page = () => thread('one', current, {next_cursor: stamp(100), total_replies: 100}); queue.push(reply(page())); await open(); const beforePosts = posts().length, before = JSON.stringify(messages()); queue.push(reply({...page(), confirmation: receipt()})); await click('thread-retry'); const result = [beforePosts, posts()[0].body, store.size, findAll('thread-message').length, JSON.stringify(messages()) === before, find('thread-load-older').hidden, posts().length]; queue.push(reply(thread('one', [message(99)], {next_cursor: stamp(99)}))); await click('thread-load-older'); return [...result, calls.at(-1).body.before];`,
			want:       `[0,{"occurrence_id":"one","request_id":"11111111-1111-4111-8111-111111111111","text":"Old reply","reconcile_only":true},0,31,true,false,1,"1790000000.000100"]`,
		},
		{
			name:       "optional GET exact receipt reconciles without any POST",
			expression: `return await checkReceipt(receipt(), 'GET');`,
			want:       `[0,0,true,0]`,
		},
		{
			name:       "POST receipt missing actor retains guard",
			expression: `return await checkReceipt({occurrence_id: 'one', request_id: requestID});`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt missing actor retains guard",
			expression: `return await checkReceipt({occurrence_id: 'one', request_id: requestID}, 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "POST receipt empty actor retains guard",
			expression: `return await checkReceipt(receipt({actor: ''}));`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt empty actor retains guard",
			expression: `return await checkReceipt(receipt({actor: ''}), 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "POST receipt wrong actor retains guard",
			expression: `return await checkReceipt(receipt({actor: 'bob'}));`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt wrong actor retains guard",
			expression: `return await checkReceipt(receipt({actor: 'bob'}), 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "POST receipt missing target retains guard",
			expression: `return await checkReceipt({request_id: requestID, actor: 'alice'});`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt missing target retains guard",
			expression: `return await checkReceipt({request_id: requestID, actor: 'alice'}, 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "POST receipt wrong target retains guard",
			expression: `return await checkReceipt(receipt({occurrence_id: 'two'}));`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt wrong target retains guard",
			expression: `return await checkReceipt(receipt({occurrence_id: 'two'}), 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "POST receipt missing request ID retains guard",
			expression: `return await checkReceipt({occurrence_id: 'one', actor: 'alice'});`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt missing request ID retains guard",
			expression: `return await checkReceipt({occurrence_id: 'one', actor: 'alice'}, 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "POST receipt wrong request ID retains guard",
			expression: `return await checkReceipt(receipt({request_id: '22222222-2222-4222-8222-222222222222'}));`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "GET receipt wrong request ID retains guard",
			expression: `return await checkReceipt(receipt({request_id: '22222222-2222-4222-8222-222222222222'}), 'GET');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "exact receipt still requires matching POST response user",
			expression: `return await checkReceipt(receipt(), 'POST', 'bob');`,
			want:       `[1,1,true,1]`,
		},
		{
			name:       "exact receipt still requires matching GET response user",
			expression: `return await checkReceipt(receipt(), 'GET', 'bob');`,
			want:       `[1,0,true,1]`,
		},
		{
			name:       "exact receipt cannot confirm a different POST thread envelope",
			expression: `return await checkReceipt(receipt(), 'POST', 'alice', 'two');`,
			want:       `[1,1,false,0]`,
		},
		{
			name:       "exact receipt cannot confirm a different GET thread envelope",
			expression: `return await checkReceipt(receipt(), 'GET', 'alice', 'two');`,
			want:       `[1,0,false,0]`,
		},
		{
			name:       "confirmation must match occurrence request ID and actor",
			expression: `setup(); await open(); input('Original'); queue.push(confirmation('one', 'bob')); await click('thread-send'); return [store.size, messages(), find('thread-error').textContent.includes('did not confirm')];`,
			want:       `[1,["Root one"],true]`,
		},
		{
			name:       "POST matching request without actor cannot clear retained guard",
			expression: `setup(); await open(); input('Original'); queue.push(reply(thread('one', [message(9, {request_id: requestID})]))); await click('thread-send'); return [store.size, find('thread-send').disabled, find('thread-error').textContent.includes('did not confirm')];`,
			want:       `[1,true,true]`,
		},
		{
			name:       "POST matching request with empty actor cannot clear retained guard",
			expression: `setup(); await open(); input('Original'); queue.push(reply(thread('one', [message(9, {request_id: requestID, actor: ''})]))); await click('thread-send'); return [store.size, find('thread-send').disabled, find('thread-error').textContent.includes('did not confirm')];`,
			want:       `[1,true,true]`,
		},
		{
			name:       "GET matching request without actor cannot reconcile retained guard",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); queue.push(reply(thread('one', [message(9, {request_id: requestID})]))); await controller.refresh(); return [store.size, find('thread-draft').value, find('thread-send').disabled];`,
			want:       `[1,"Original",true]`,
		},
		{
			name:       "GET matching request with empty actor cannot reconcile retained guard",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); queue.push(reply(thread('one', [message(9, {request_id: requestID, actor: ''})]))); await controller.refresh(); return [store.size, find('thread-draft').value, find('thread-send').disabled];`,
			want:       `[1,"Original",true]`,
		},
		{
			name:       "GET confirmation cannot clear another occurrences pending request",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); queue.push(confirmation('two')); await open('two'); return [store.size, find('thread-send').disabled];`,
			want:       `[1,true]`,
		},
		{
			name:       "matching GET reconciles only trusted original actor request",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); queue.push(confirmation()); await controller.refresh(); return [store.size, find('thread-draft').value, posts().length];`,
			want:       `[0,"",1]`,
		},
		{
			name:       "late pre confirmation GET cannot overwrite confirmed post",
			expression: `setup(); await open(); let resolve; queue.push(new Promise(done => {resolve = done;})); const reading = controller.refresh(); input('Reply'); queue.push(confirmation()); await click('thread-send'); resolve(reply(thread('one'))); await reading; return [messages(), store.size, find('thread-refresh').disabled];`,
			want:       `[["Root one","Reply 9"],0,false]`,
		},
		{
			name:       "auth pause immediately clears metadata drafts pending preview and ignores late GET",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: 'Secret'})], {people: {U2: {name: 'Private'}}}))); await open(); input('Secret draft'); let resolve; queue.push(new Promise(done => {resolve = done;})); const reading = controller.refresh(); controller.update({user: 'alice', authPaused: true, status: {phase: 'ready'}}); const cleared = [messages(), find('thread-draft').value, content(find('thread-pending-preview'))]; resolve(confirmation()); await reading; return [cleared, messages(), find('thread-send').disabled, calls.length];`,
			want:       `[[[],"",""],[],true,2]`,
		},
		{
			name:       "auth pause ignores late POST leaves guard only in original actors storage",
			expression: `setup(); await open(); input('Secret'); let resolve; queue.push(new Promise(done => {resolve = done;})); const sending = click('thread-send'); controller.update(null); resolve(confirmation()); await sending; return [messages(), find('thread-draft').value, content(find('thread-pending-preview')), find('thread-retry').hidden, Array.from(store.keys())];`,
			want:       `[[],"","",true,["ops-alert-dashboard.thread.pending.v1.alice"]]`,
		},
		{
			name:       "operator change isolates durable guard and clears private state",
			expression: `setup(); await open(); input('Secret'); queue.push(new Error('Lost')); await click('thread-send'); controller.update({user: 'bob', status: {phase: 'ready'}}); queue.push(reply(thread('two'), 200, 'bob')); await open('two'); return [messages(), find('thread-draft').value, find('thread-retry').hidden, Array.from(store.keys()), posts().length];`,
			want:       `[["Root two"],"",true,["ops-alert-dashboard.thread.pending.v1.alice"],1]`,
		},
		{
			name:       "server identity mismatch clears private state and pauses",
			expression: `setup(); await open(); input('Secret'); queue.push(reply(thread('one', [message(2)]), 200, 'bob')); await controller.refresh(); return [authErrors, messages(), find('thread-draft').value, find('thread-send').disabled];`,
			want:       `[1,[],"",true]`,
		},
		{
			name:       "authentication failure during POST preserves actor guard without private preview",
			expression: `setup(); await open(); input('Secret'); queue.push(reply({error: 'Unauthorized'}, 401)); await click('thread-send'); return [authErrors, messages(), find('thread-draft').value, find('thread-retry').hidden, store.size];`,
			want:       `[1,[],"",true,1]`,
		},
		{
			name:       "reauthentication restores guard without fetching or automatically posting",
			expression: `setup(); await open(); input('Secret'); queue.push(new Error('Lost')); await click('thread-send'); controller.update(null); controller.update({user: 'alice', status: {phase: 'ready'}}); const before = calls.length; await open(); return [before, calls.length, posts().length, find('thread-retry').hidden, find('thread-draft').value];`,
			want:       `[2,3,1,false,""]`,
		},
		{
			name:       "explicit discard requires confirmation after checking Slack and sends nothing",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); click('thread-discard'); const retained = store.size; confirmed = true; click('thread-discard'); return [retained, store.size, posts().length, find('thread-draft').value, find('thread-error').textContent.includes('nothing was sent or undone')];`,
			want:       `[1,0,1,"Original",true]`,
		},
		{
			name:       "storage failure blocks transport and warns",
			expression: `setup(); await open(); input('Original'); sessionStorage.setItem = () => {throw new Error('Unavailable');}; await click('thread-send'); return [posts().length, find('thread-send').disabled, find('thread-error').textContent.includes('session storage')];`,
			want:       `[0,true,true]`,
		},
		{
			name:       "invalid restored payload cannot introduce extra POST fields",
			expression: `store.set(key('alice'), JSON.stringify({request: {occurrence_id: 'one', request_id: requestID, text: 'Retained', actor: 'other'}, uncertain: true})); setup(); await open(); input('New'); await click('thread-send'); return [posts().length, find('thread-send').disabled, find('thread-error').textContent.includes('could not be read safely')];`,
			want:       `[0,true,true]`,
		},
		{
			name:       "UTF8 maximum is bytes not JS characters",
			expression: `setup(); await open(); input('😀'.repeat(1001)); await click('thread-send'); return [posts().length, find('thread-counter').textContent, find('thread-draft').attributes['aria-invalid'], find('thread-send').disabled];`,
			want:       `[0,"4004 / 4000 bytes","true",true]`,
		},
		{
			name:       "mrkdwn preserves newlines quotes lists inline and fenced code without parsing code",
			expression: "setup(); queue.push(reply(thread('one', [message(2, {text: '*Bold* _Italic_ ~Strike~\\n&gt; Quote\\n- List item\\n`<@U2> :eyes:`\\n```\\n<img src=x> :eyes:\\n```'})]))); await open(); return [tags('strong').map(content).includes('Bold'), tags('em').map(content), tags('s').map(content), tags('blockquote').map(content), tags('li').map(content), tags('code').map(content), tags('img').length];",
			want:       `[true,["Italic"],["Strike"],["Quote"],["List item"],["<@U2> :eyes:","<img src=x> :eyes:\n"],2]`,
		},
		{
			name:       "unresolved mentions stay IDs rather than invented channel or group identities",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: '<@U2> <#C123> <#COTHER> <!subteam^S123> <!subteam^SOTHER> <!here>'})]))); await open(); return messages()[1];`,
			want:       `"@U2 #ops #COTHER @operators @SOTHER @here"`,
		},
		{
			name:       "unknown rich leaves and excessive depth have visible bounded fallback",
			expression: `setup(); let nested = {type: 'rich_text_section', elements: [{type: 'video'}]}; for (let i = 0; i < 12; i++) nested = {type: 'rich_text', elements: [nested]}; queue.push(reply(thread('one', [message(2, {blocks: [{type: 'rich_text', elements: [{type: 'rich_text_section', elements: [{type: 'video'}]}, nested]}]})]))); await open(); return [content(find('thread-unsupported')).includes('video rich text element'), content(find('thread-unsupported')).includes('deeply nested rich text'), walk().length < 100];`,
			want:       `[true,true,true]`,
		},
		{
			name:       "safe HTTPS links use noopener and reject credentials controls and backslashes",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'rich_text', elements: [{type: 'rich_text_section', elements: ['https://example.org/good', 'https://user:pass@example.org/', 'https://@example.org/', 'https://example.org/\u007f', 'https://example.org/\\path', 'javascript:evil()'].map(url => ({type: 'link', url, text: 'Link'}))}]}]})]))); await open(); return tags('a').filter(node => node.href.includes('example.org')).map(node => [node.href, node.target, node.rel]);`,
			want:       `[["https://example.org/good","_blank","noopener noreferrer"]]`,
		},
		{
			name:       "whitespace edit during delivery is a new draft and remains intact",
			expression: `setup(); await open(); input(' Original '); let resolve; queue.push(new Promise(done => {resolve = done;})); const sending = click('thread-send'); input('Original  '); resolve(confirmation()); await sending; return [posts()[0].body.text, find('thread-draft').value];`,
			want:       `["Original","Original  "]`,
		},
		{
			name:       "all initial definite rejection codes release only fresh guards",
			expression: `const results = []; for (const code of ['validation', 'not_found', 'not_ready', 'conflict', 'delivery_failed']) {setup(); await open(); input('Original'); queue.push(reply({code}, 400)); await click('thread-send'); results.push(store.size); controller.destroy();} return results;`,
			want:       `[0,0,0,0,0]`,
		},
		{
			name:       "wrong occurrence or request confirmations keep durable uncertainty",
			expression: `setup(); await open(); input('Original'); queue.push(confirmation('wrong')); await click('thread-send'); queue.push(reply(thread('one', [message(9, {request_id: '22222222-2222-4222-8222-222222222222', actor: 'alice'})]))); await click('thread-retry'); return [store.size, find('thread-send').disabled, posts()[1].body.reconcile_only];`,
			want:       `[1,true,true]`,
		},
		{
			name:       "late GET after identity swap cannot restore prior operator metadata",
			expression: `setup(); let resolve; queue.push(new Promise(done => {resolve = done;})); controller.open({occurrenceID: 'one'}); controller.update({user: 'bob', status: {phase: 'ready'}}); resolve(reply(thread('one', [message(2, {text: 'Private Alice'})]))); await settle(); return [messages(), find('thread-draft').value, find('thread-refresh').disabled];`,
			want:       `[[],"",true]`,
		},
		{
			name:       "POST identity mismatch leaves original actor guard and hides private display",
			expression: `setup(); await open(); input('Original'); queue.push({...confirmation(), body: {...confirmation().body, user: 'bob'}}); await click('thread-send'); return [store.size, authErrors, messages(), content(find('thread-pending-preview')), find('thread-send').disabled];`,
			want:       `[1,1,[],"",true]`,
		},
		{
			name:       "missing response identity never treated as verified",
			expression: `setup(); queue.push({ok: true, status: 200, body: {thread: thread()}}); await open(); return [authErrors, messages(), find('thread-send').disabled];`,
			want:       `[1,[],true]`,
		},
		{
			name:       "retained request Slack fallback still points to original target after switch",
			expression: `setup(); queue.push(reply(thread('one', [], {slack_url: 'https://workspace.slack.com/archives/C123/original'}))); await open(); input('Original'); queue.push(new Error('Lost')); await click('thread-send'); await open('two'); return findAll('thread-link', find('thread-pending-preview')).map(node => node.href);`,
			want:       `["https://workspace.slack.com/archives/C123/original"]`,
		},
		{
			name:       "retained original thread fallback can use snapshot occurrence metadata after reload",
			expression: `store.set(key('alice'), JSON.stringify({request: {occurrence_id: 'one', request_id: requestID, text: 'Original'}, uncertain: true})); setup(); controller.update({user: 'alice', status: {phase: 'ready'}, occurrences: [{identity: {id: 'one'}, message: {slack_url: 'https://workspace.slack.com/archives/C123/original'}}]}); await open('two'); return [posts().length, findAll('thread-link', find('thread-pending-preview')).map(node => node.href)];`,
			want:       `[0,["https://workspace.slack.com/archives/C123/original"]]`,
		},
		{
			name:       "degraded readiness permits cached reads but never writes",
			expression: `setup(); controller.update({user: 'alice', status: {phase: 'degraded'}}); await open(); input('Draft'); await click('thread-send'); return [calls.length, posts().length, find('thread-send').disabled, find('thread-draft').value];`,
			want:       `[1,0,true,"Draft"]`,
		},
		{
			name:       "exact 4000 UTF8 byte boundary is accepted",
			expression: `setup(); await open(); input('😀'.repeat(1000)); queue.push(confirmation()); await click('thread-send'); return [posts().length, new TextEncoder().encode(posts()[0].body.text).length, store.size];`,
			want:       `[1,4000,0]`,
		},
		{
			name:       "nonstandard ports and oversized raster fixtures are rejected",
			expression: `setup(); const urls = ['https://avatars.slack-edge.com:443/a.png', 'https://avatars.slack-edge.com:8443/a.png', 'data:image/png;base64,' + 'a'.repeat(65536)]; queue.push(reply(thread('one', [message(2, {blocks: urls.map(image_url => ({type: 'image', image_url}))})]))); await open(); return [findAll('thread-attachment-image').length, content(find('thread-unsupported')).includes('untrusted URL')];`,
			want:       `[0,true]`,
		},
		{
			name:       "paused or unselected composer cannot accept a new reply",
			expression: `setup(); const before = find('thread-draft').disabled; await open(); const opened = find('thread-draft').disabled; controller.update({user: 'alice', authPaused: true}); return [before, opened, find('thread-draft').disabled, find('thread-send').disabled];`,
			want:       `[true,false,true,true]`,
		},
		{
			name:       "user and channel references retain Slack actions",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: '<@U_CHAI> <#C123> <#G_OTHER|private>'})], {people: {U_CHAI: {name: 'Chai'}}}))); await open(); return findAll('thread-mention').map(node => [node.tag, content(node), node.href]);`,
			want:       `[["a","@Chai","https://workspace.slack.com/team/U_CHAI"],["a","#ops","https://workspace.slack.com/archives/C123"],["a","#private","https://workspace.slack.com/archives/G_OTHER"]]`,
		},
		{
			name:       "rich text mentions retain profile and channel links",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {blocks: [{type: 'rich_text', elements: [{type: 'rich_text_section', elements: [{type: 'user', user_id: 'U2'}, {type: 'channel', channel_id: 'C123'}]}]}]})], {people: {U2: {name: 'Bob'}}}))); await open(); return findAll('thread-mention').map(node => [content(node), node.href, node.rel]);`,
			want:       `[["@Bob","https://workspace.slack.com/team/U2","noopener noreferrer"],["#ops","https://workspace.slack.com/archives/C123","noopener noreferrer"]]`,
		},
		{
			name:       "derived mention actions never trust a non Slack workspace",
			expression: `setup(); queue.push(reply(thread('one', [message(2, {text: '<@U2> <#C123>'})], {slack_url: 'https://evil.example/thread'}))); await open(); return findAll('thread-mention').map(node => [node.tag, node.href || null]);`,
			want:       `[["span",null],["span",null]]`,
		},
		{
			name:       "unresolved references retain an editable draft without a delivery guard",
			expression: `setup(); await open(); input('Ask @missing'); queue.push(reply({code: 'validation', error: 'Cannot resolve Slack reference @missing'}, 400)); await click('thread-send'); return [find('thread-draft').value, store.size, find('thread-error').textContent, posts()[0].body.text];`,
			want:       `["Ask @missing",0,"Cannot resolve Slack reference @missing","Ask @missing"]`,
		},
		{
			name:       "workspace loading preserves draft and clears guard without automatically posting",
			expression: `setup(); await open(); input('Ask @alice'); queue.push(reply({code: 'not_ready', error: 'Slack user directory is loading; no reply was posted. Try again shortly.'}, 503)); await click('thread-send'); return [find('thread-draft').value, store.size, find('thread-send').disabled, find('thread-error').textContent, posts().length];`,
			want:       `["Ask @alice",0,false,"Slack user directory is loading; no reply was posted. Try again shortly.",1]`,
		},
		{
			name:       "destroy invalidates pending reads and removes isolated panel",
			expression: `setup(); let resolve; queue.push(new Promise(done => {resolve = done;})); controller.open({occurrenceID: 'one'}); controller.destroy(); resolve(reply()); await settle(); return [root.children.length, posts().length, timers.size];`,
			want:       `[0,0,0]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(node, "-e", threadUnitHarness, tc.expression)
			command.Env = append(command.Environ(), "NODE_OPTIONS=")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run thread unit fixture: %v\n%s", err, output)
			}
			if diff := cmp.Diff(tc.want, string(output)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSlackThreadAppWiring checks the existing app's narrow controller boundary without a browser or network.
func TestSlackThreadAppWiring(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable for thread app wiring unit tests")
	}
	harness := strings.Replace(uiUnitHarness, "vm.runInContext(source, context);", `context.window.OpsSlackThread = {create: options => {context.threadOptions = options; return {open: value => {context.openedThread = value;}, update: value => {context.threadUpdate = value;}};}}; vm.runInContext(source, context);`, 1)
	harness = strings.Replace(harness, "globalThis.UI = {", "globalThis.UI = {threadRequest,", 1)
	for _, tc := range []struct {
		name       string
		expression string
		want       string
	}{
		{
			name:       "thread GET encodes occurrence cursor and never targets Chai",
			expression: `(async () => {UI.state.snapshot = {...snapshot([]), csrf_token: 'token'}; globalThis.sent = []; globalThis.fetch = async (url, options) => {sent.push({url, method: options.method, headers: options.headers, credentials: options.credentials, cache: options.cache}); return respond({user: 'operator', thread: {}});}; await UI.threadRequest('GET', {occurrence_id: 'C1:1.2', before: '1.1'}); return sent;})()`,
			want:       `[{"url":"/api/alert-thread?occurrence_id=C1%3A1.2&before=1.1","method":"GET","headers":{"Accept":"application/json"},"credentials":"same-origin","cache":"no-store"}]`,
		},
		{
			name:       "thread GET adds refresh true only for an explicit true flag",
			expression: `(async () => {UI.state.snapshot = {...snapshot([]), csrf_token: 'token'}; globalThis.sent = []; globalThis.fetch = async url => {sent.push(url); return respond({user: 'operator', thread: {}});}; await UI.threadRequest('GET', {occurrence_id: 'C1:1.2', refresh: true}); await UI.threadRequest('GET', {occurrence_id: 'C1:1.2', refresh: false}); await UI.threadRequest('GET', {occurrence_id: 'C1:1.2'}); return sent;})()`,
			want:       `["/api/alert-thread?occurrence_id=C1%3A1.2&refresh=true","/api/alert-thread?occurrence_id=C1%3A1.2","/api/alert-thread?occurrence_id=C1%3A1.2"]`,
		},
		{
			name:       "thread POST sends strict caller payload with actor bound CSRF",
			expression: `(async () => {UI.state.snapshot = {...snapshot([]), csrf_token: 'actor-token'}; globalThis.sent = []; globalThis.fetch = async (url, options) => {sent.push({url, method: options.method, headers: options.headers, body: JSON.parse(options.body)}); return respond({user: 'operator', thread: {}});}; await UI.threadRequest('POST', {occurrence_id: 'one', request_id: 'id', text: 'Plain reply'}); return sent;})()`,
			want:       `[{"url":"/api/alert-thread","method":"POST","headers":{"Accept":"application/json","Content-Type":"application/json","X-CSRF-Token":"actor-token"},"body":{"occurrence_id":"one","request_id":"id","text":"Plain reply"}}]`,
		},
		{
			name:       "successful main snapshot only updates thread identity and readiness",
			expression: `(async () => {globalThis.urls = []; globalThis.fetch = async url => {urls.push(url); return respond(snapshot([]));}; await UI.refresh(); return [urls, threadUpdate.user, threadUpdate.status.phase];})()`,
			want:       `[["/api/snapshot"],"operator","ready"]`,
		},
		{
			name:       "failed main snapshot downgrades thread posting readiness without a thread read",
			expression: `(async () => {UI.state.snapshot = snapshot([]); globalThis.urls = []; globalThis.fetch = async url => {urls.push(url); throw new Error('Offline');}; await UI.refresh(); return [urls, threadUpdate.user, threadUpdate.status.phase];})()`,
			want:       `[["/api/snapshot"],"operator","degraded"]`,
		},
		{
			name:       "auth pause notifies isolated controller immediately",
			expression: `UI.pauseAuthentication('Expired'); [threadUpdate, UI.state.authPaused]`,
			want:       `[null,true]`,
		},
		{
			name:       "inspector discussion open leaves original workflow and read state unchanged",
			expression: `let items = setupQueue(['one']); let before = JSON.stringify([UI.state.selected, UI.state.personal, UI.state.pending]); let buttons = tags(queueRoot, 'button'); buttons.find(button => content(button).includes('Load Slack conversation')).handlers.click(); [openedThread, before === JSON.stringify([UI.state.selected, UI.state.personal, UI.state.pending])]`,
			want:       `[{"occurrenceID":"one","title":"ParsedAlert"},true]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(node, "-e", harness, tc.expression)
			command.Env = append(command.Environ(), "NODE_OPTIONS=", "TZ=Europe/Warsaw")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run thread app unit fixture: %v\n%s", err, output)
			}
			if diff := cmp.Diff(tc.want, string(output)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}
