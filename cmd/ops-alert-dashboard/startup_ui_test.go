package main

import (
	"os/exec"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// startupHarness boots the unmodified production frontend in a Node VM: the
// browser scripts run in the order index.html serves them (reports.js, chat.js,
// threads.js, app.js) against an inert DOM, storage and timer fake. The initial
// /api/snapshot response stays pending until a case releases it, so each case
// observes the real bootstrap, the first fetch and both channel-name states
// deterministically, without a browser, network access or timers firing. An
// app.js load failure is captured as loadError instead of aborting the process,
// which is what makes an initialization regression visible in the case result.
const startupHarness = `
const fs = require('node:fs'), vm = require('node:vm');
class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.attributes = {}; this.handlers = {}; this.dataset = {}; this.style = {}; this.parent = null; this.textContent = ''; this.value = ''; this.className = ''; this.id = ''; this.hidden = false; this.disabled = false; this.scrollTop = 0; this.scrollHeight = 0; this.clientHeight = 0; }
  append(...children) { for (const child of children) if (child instanceof Element) { child.parent = this; this.children.push(child); } }
  replaceChildren(...children) { this.children = []; this.append(...children); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, handler) { this.handlers[name] = handler; }
  removeEventListener(name) { delete this.handlers[name]; }
  focus() { this.focused = true; }
  showModal() { this.open = true; }
  close() { this.open = false; }
  closest(selector) { let current = this; while (current) { if (current.tag === String(selector).toLowerCase()) return current; current = current.parent; } return null; }
  querySelectorAll(selector) { const cls = String(selector).slice(1); const found = []; if (!String(selector).startsWith('.')) return found; const descend = element => { for (const child of element.children) { if ((child.className || '').split(' ').includes(cls)) found.push(child); descend(child); } }; descend(this); return found; }
}
const storage = new Map(), session = new Map(), elements = new Map(), timers = new Map(), windowHandlers = new Map(), documentHandlers = new Map(); let timerID = 0;
const document = {title: '', activeElement: null, handlers: {},
  createElement: tag => new Element(tag),
  querySelectorAll: () => [],
  getElementById: id => { if (!elements.has(id)) elements.set(id, new Element('div')); return elements.get(id); },
  addEventListener(name, handler) { this.handlers[name] = handler; documentHandlers.set(name, handler); },
  removeEventListener(name) { delete this.handlers[name]; documentHandlers.delete(name); }};
Object.defineProperty(document, 'cookie', {configurable: true, get: () => '', set: () => {}});
const window = {location: {protocol: 'https:'},
  setTimeout: (fn, delay) => { timers.set(++timerID, fn); return timerID; },
  clearTimeout: id => { timers.delete(id); },
  setInterval: (fn, delay) => { timers.set(++timerID, fn); return timerID; },
  clearInterval: id => { timers.delete(id); },
  addEventListener: (name, handler) => { windowHandlers.set(name, handler); },
  removeEventListener: () => {}};
const searchLabel = new Element('label'); searchLabel.className = 'search';
const searchInput = new Element('input'); searchLabel.append(searchInput);
elements.set('search', searchInput);
const context = vm.createContext({URL, TextEncoder, Intl, Date, Map, Set, console, AbortController, crypto: require('node:crypto'), document, window, navigator: {},
  localStorage: {getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
  sessionStorage: {getItem: key => session.get(key) ?? null, setItem: (key, value) => session.set(key, value), removeItem: key => session.delete(key)}});
context.timers = timers; context.windowHandlers = windowHandlers; context.documentHandlers = documentHandlers;
vm.runInContext("globalThis.requests = []; globalThis.responses = []; globalThis.snapshotChannel = 'ops-testplatform'; globalThis.respond = (value, status = 200) => ({ok: status < 400, status, redirected: false, headers: {get: () => 'application/json'}, json: async () => value}); globalThis.fixture = (name, at = '2026-11-09T09:00:00Z') => ({identity: {id: name, revision: '1', channel: 'C123'}, message: {title: '[FIRING] Raw title [ops_color=red]', text: '', identifiers: ['ParsedAlert'], reported_at: at, source: 'alerts', slack_url: 'https://workspace.slack.com/archives/C123/p1'}, classification: {category: 'ci_platform'}, view: 'feed'}); globalThis.snapshot = () => ({user: 'operator', channel_name: globalThis.snapshotChannel, csrf_token: 'token', usergroups: {}, status: {phase: 'ready', last_sync: '2026-11-09T10:00:00Z', cutover: '2026-11-02T06:00:00Z'}, occurrences: [fixture('one'), fixture('two')]}); globalThis.fetch = async (url, options = {}) => {requests.push({url, method: options.method || 'GET', credentials: options.credentials, cache: options.cache}); if (url !== '/api/snapshot') throw new Error('unexpected request during startup: ' + url); return responses.length ? await responses.shift() : respond(snapshot());}; globalThis.holdSnapshot = () => {let done; responses.push(new Promise(resolve => {done = resolve;})); return () => done(respond(snapshot()));}; globalThis.release = holdSnapshot();", context);
vm.runInContext("globalThis.walk = node => [node, ...node.children.flatMap(child => walk(child))]; globalThis.findNode = id => [document.getElementById('chai-chat-root'), document.getElementById('alert-thread-root')].flatMap(walk).find(node => node.id === id); globalThis.content = node => node ? node.textContent + node.children.map(content).join('') : ''; globalThis.hint = () => findNode('alert-thread-mentions')?.textContent || ''; globalThis.alertRows = () => walk(document.getElementById('content')).filter(node => (node.className || '').split(' ').includes('alert-row')).length; globalThis.settle = async () => { for (let i = 0; i < 24; i++) await Promise.resolve(); };", context);
vm.runInContext(fs.readFileSync('web/reports.js', 'utf8'), context);
vm.runInContext(fs.readFileSync('web/chat.js', 'utf8'), context);
vm.runInContext(fs.readFileSync('web/threads.js', 'utf8'), context);
context.loadError = null;
try { vm.runInContext(fs.readFileSync('web/app.js', 'utf8'), context); }
catch (err) { context.loadError = {name: err.name, message: err.message}; }
Promise.resolve(vm.runInContext('(async () => {' + process.argv[1] + '})()', context)).then(value => process.stdout.write(JSON.stringify(value)), error => {console.error(error); process.exitCode = 1;});
`

// TestOpsDashboardStartup guards the full frontend bootstrap: app.js must finish
// initializing with the real chat and thread controllers, install its refresh
// handling, request the first /api/snapshot and keep the thread mention hint on
// the fallback channel until a snapshot supplies the configured one.
func TestOpsDashboardStartup(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable for dashboard startup unit tests")
	}
	for _, tc := range []struct {
		name       string
		expression string
		want       string
	}{
		{
			name: "bootstrap mounts real controllers, keeps the fallback channel and requests one snapshot",
			expression: `const initial = [loadError ? [loadError.name, loadError.message] : null, !!findNode('alert-thread-panel'), findNode('alert-thread-panel')?.hidden ?? null, !!findNode('chai-chat-panel'), content(findNode('chai-chat-panel')).includes('Waiting for your session'), hint().includes('#Operations channel'), requests, timers.size, [...windowHandlers.keys()], [...documentHandlers.keys()]];
release(); await settle();
return [initial, requests.length, typeof document.getElementById('refresh').handlers.click === 'function', alertRows(), document.getElementById('operator').textContent];`,
			want: `[[null,true,true,true,true,true,[{"url":"/api/snapshot","method":"GET","credentials":"same-origin","cache":"no-store"}],2,["focus","storage"],["visibilitychange"]],1,true,2,"Operator · operator"]`,
		},
		{
			name: "released snapshot renders the workspace with the configured channel",
			expression: `release(); await settle();
return [loadError ? [loadError.name, loadError.message] : null, hint().includes('#ops-testplatform'), hint().includes('#Operations channel'), document.title, document.getElementById('operator').textContent, document.getElementById('coverage').textContent.startsWith('Coverage ready'), alertRows(), requests.map(request => request.url), timers.size];`,
			want: `[null,true,false,"Test Platform · Operations","Operator · operator",true,2,["/api/snapshot"],1]`,
		},
		{
			name: "missing channel name keeps the fallback hint after the snapshot",
			expression: `globalThis.snapshotChannel = undefined; release(); await settle();
return [hint().includes('#Operations channel'), hint().includes('#ops-testplatform'), alertRows(), requests.length];`,
			want: `[true,false,2,1]`,
		},
		{
			name: "empty channel name keeps the fallback hint after the snapshot",
			expression: `globalThis.snapshotChannel = ''; release(); await settle();
return [hint().includes('#Operations channel'), hint().includes('#ops-testplatform'), alertRows(), requests.length];`,
			want: `[true,false,2,1]`,
		},
		{
			name: "registered refresh control reads a second snapshot and keeps the channel callback dynamic",
			expression: `release(); await settle();
const first = hint();
globalThis.snapshotChannel = 'ops-renamed';
await document.getElementById('refresh').handlers.click();
await settle();
return [first.includes('#ops-testplatform'), hint().includes('#ops-renamed'), hint().includes('#Operations channel'), requests.map(request => request.url), requests.length, timers.size];`,
			want: `[true,true,false,["/api/snapshot","/api/snapshot"],2,1]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(node, "-e", startupHarness, tc.expression)
			command.Env = append(command.Environ(), "NODE_OPTIONS=", "TZ=UTC")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run dashboard startup fixture: %v\n%s", err, output)
			}
			if diff := cmp.Diff(tc.want, string(output)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}
