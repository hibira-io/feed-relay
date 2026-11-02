'use strict';
(() => {
  const ICON = {
    rss: '<path d="M5 19a1 1 0 1 0 0-2 1 1 0 0 0 0 2zM4 11a9 9 0 0 1 9 9M4 4a16 16 0 0 1 16 16"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    minus: '<path d="M5 12h14"/>',
    edit: '<path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"/>',
    trash: '<path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v6M14 11v6"/>',
    refresh: '<path d="M21 12a9 9 0 1 1-2.6-6.4L21 8"/><path d="M21 3v5h-5"/>',
    send: '<path d="M22 2 11 13"/><path d="M22 2 15 22l-4-9-9-4z"/>',
    x: '<path d="M18 6 6 18M6 6l12 12"/>',
    lock: '<rect x="4" y="11" width="16" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
    logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9"/>',
    eye: '<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8S1 12 1 12z"/><circle cx="12" cy="12" r="3"/>',
  };
  const icon = (name) => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICON[name]}</svg>`;
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
  const $ = (sel, root = document) => root.querySelector(sel);
  const app = $('#app');

  let state = null;
  let timer = null;
  let live = null; // { ws, status, reason }

  // --- api ---
  async function api(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: { 'Content-Type': 'application/json', 'X-Feed-Relay': '1' },
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
    });
    if (res.status === 401 && path !== '/api/login') {
      showLogin();
      throw new Error('Sign in again.');
    }
    const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
    if (!res.ok) {
      const err = new Error(data.error || `Request failed (${res.status})`);
      err.problems = data.problems;
      throw err;
    }
    return data;
  }

  function toast(msg, kind) {
    const el = document.createElement('div');
    el.className = 'toast' + (kind === 'err' ? ' err' : kind === 'live' ? ' live' : '');
    if (kind === 'live') el.innerHTML = `<img src="/console/hibira.svg" alt="" width="18" height="18"><span>${esc(msg)}</span>`;
    else
      el.textContent = msg;
    $('#toasts').append(el);
    setTimeout(() => el.remove(), kind === "err" ? 6000 : kind === "live" ? 6000 : 3500);
  }

  // --- time ---
  function ago(iso) {
    if (!iso) return '';
    const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 45) return 'just now';
    if (s < 3600) return `${Math.round(s / 60)} min ago`;
    if (s < 86400) return `${Math.round(s / 3600)} h ago`;
    return `${Math.round(s / 86400)} d ago`;
  }

  // --- login ---
  function showLogin() {
    clearInterval(timer);
    app.innerHTML = `
      <div class="login"><form class="card" id="login">
        <img src="/console/icon.svg" width="40" height="40" alt="">
        <h1>Feed relay</h1>
        <p>Enter the admin password set in ADMIN_PASSWORD.</p>
        <div class="field"><label for="pw">Password</label>
          <input class="input" id="pw" type="password" autocomplete="current-password" required></div>
        <button class="btn primary block" type="submit">Sign in</button>
      </form></div>`;
    $('#pw').focus();
    $('#login').addEventListener('submit', async (e) => {
      e.preventDefault();
      const btn = e.target.querySelector('button');
      btn.disabled = true;
      try {
        await api('POST', '/api/login', { password: $('#pw').value });
        await load();
      } catch (err) {
        toast(err.message, 'err');
        btn.disabled = false;
        $('#pw').select();
      }
    });
  }

  // --- main ---
  async function load() {
    try {
      state = await api('GET', '/api/state');
    } catch {
      return;
    }
    renderMain();
    clearInterval(timer);
    timer = setInterval(refresh, 10000);
  }

  async function refresh() {
    if (document.hidden || $('.drawer') || $('.modal')) return;
    try {
      state = await api('GET', '/api/state');
      renderList();
    } catch { /* shown by api() */ }
  }

  function renderMain() {
    const h = state.hibira;
    app.innerHTML = `
      <div class="wrap">
        <div class="top">
          <div class="brand"><img src="/console/icon.svg" alt="">Feed relay</div>
          <span class="chip hide-sm">${esc(state.version)}</span>
          <div class="grow"></div>
          <span id="live-chip"></span>
          <span class="chip ok" data-tip="${esc(h.url)}" tabindex="0"><span class="dot"></span>App ${esc(h.app_id)}</span>
          <button class="icon-btn" id="logout" data-tip="Sign out" data-tip-end aria-label="Sign out">${icon('logout')}</button>
        </div>
        <div class="head">
          <div><h1>Feeds</h1><p>New items are sent to Hibira as soon as the relay sees them.</p></div>
          <div class="grow"></div>
          <div class="stats" id="stats"></div>
          <button class="btn primary" id="add">${icon('plus')}Add feed</button>
        </div>
        <div class="list" id="list"></div>
      </div>`;
    $('#logout').onclick = async () => { await api('POST', '/api/logout'); stopLive(); showLogin(); };
    $('#add').onclick = () => openEditor(null);
    renderList();
    startLive();
  }

  function targetsOf(f) {
    const out = [];
    for (const c of f.channels || []) out.push(`<span class="chip">#${esc(c)}</span>`);
    if (f.push) out.push(`<span class="chip" data-tip="${esc([...(f.push.interests || []), ...(f.push.users || [])].join(', '))}" tabindex="0">Push</span>`);
    if (f.inbox) out.push(`<span class="chip" data-tip="${esc(f.inbox.users.length + ' users')}" tabindex="0">Inbox</span>`);
    return out.join('');
  }

  function statusOf(v) {
    const s = v.status, f = v.feed;
    if (f.disabled) return { text: 'Paused', cls: '' };
    if (s.last_error) return { text: s.last_error, cls: 'err' };
    const parts = [];
    parts.push(s.last_fetch ? `Checked ${ago(s.last_fetch)}` : 'Waiting for the first check');
    parts.push(`every ${f.interval}`);
    if (s.sent) parts.push(`${s.sent} sent${s.last_sent ? `, last ${ago(s.last_sent)}` : ''}`);
    if (s.websub === 'active') parts.push('WebSub on');
    return { text: parts.join(' · '), cls: '' };
  }

  function renderList() {
    const list = $('#list');
    if (!list) return;
    const feeds = state.feeds;
    const sent = feeds.reduce((n, v) => n + (v.status.sent || 0), 0);
    const errs = feeds.filter((v) => v.status.last_error && !v.feed.disabled).length;
    $('#stats').innerHTML = `
      <div class="stat"><b>${feeds.length}</b><span>feeds</span></div>
      <div class="stat"><b>${sent}</b><span>sent since start</span></div>
      ${errs ? `<div class="stat"><b class="err-text">${errs}</b><span>failing</span></div>` : ''}`;
    if (!feeds.length) {
      list.innerHTML = `<div class="empty">
        <div class="empty-mark"><div class="mark">${icon('rss')}</div></div>
        <h2>No feeds yet</h2>
        <p>Add an RSS, Atom or JSON feed${state.rsshub_url ? ', or just an RSSHub route like <code>/github/release/owner/repo</code>' : ''}. Each new item goes to your Hibira channels, push or inbox.</p>
        <button class="btn primary" id="add2">${icon('plus')}Add your first feed</button></div>`;
      $('#add2').onclick = () => openEditor(null);
      return;
    }
    list.innerHTML = feeds.map((v) => {
      const f = v.feed, st = statusOf(v), file = v.source === 'file';
      return `<div class="feed${f.disabled ? ' paused' : ''}" data-id="${v.id}">
        <div class="mark">${icon('rss')}</div>
        <div class="body">
          <div class="name">${esc(f.name)}${file ? `<span class="chip" data-tip="Set in the config file" tabindex="0">${icon('lock').replace('<svg', '<svg width="11" height="11"')}config</span>` : ''}</div>
          <div class="url" title="${esc(f.url)}">${esc(f.url)}</div>
          <div class="meta">${targetsOf(f)}<span class="status ${st.cls}">${esc(st.text)}</span></div>
        </div>
        <div class="actions">
          <button class="switch" role="switch" aria-checked="${!f.disabled}" data-act="pause" ${file ? 'disabled' : ''}
            data-tip="${file ? 'Change it in the config file' : f.disabled ? 'Paused: click to run' : 'Running: click to pause'}" aria-label="Running"></button>
          <button class="icon-btn" data-act="poll" data-tip="Check now" aria-label="Check now" ${f.disabled ? 'disabled' : ''}>${icon('refresh')}</button>
          <button class="icon-btn" data-act="test" data-tip="Send the newest item as a test" aria-label="Send test">${icon('send')}</button>
          ${file ? '' : `<button class="icon-btn" data-act="edit" data-tip="Edit" aria-label="Edit">${icon('edit')}</button>
          <button class="icon-btn danger" data-act="delete" data-tip="Delete" data-tip-end aria-label="Delete">${icon('trash')}</button>`}
        </div></div>`;
    }).join('');
    list.querySelectorAll('[data-act]').forEach((b) => {
      b.onclick = () => act(b.dataset.act, b.closest('.feed').dataset.id, b);
    });
  }

  async function act(what, id, btn) {
    const v = state.feeds.find((x) => x.id === id);
    if (!v) return;
    if (what === 'edit') return openEditor(v);
    if (what === 'delete') return confirmDelete(v);
    btn.disabled = true;
    try {
      if (what === 'pause') {
        await api('POST', `/api/feeds/${id}/pause`, { paused: !v.feed.disabled });
        toast(v.feed.disabled ? `${v.feed.name} is running` : `${v.feed.name} is paused`);
      } else if (what === 'poll') {
        const r = await api('POST', `/api/feeds/${id}/poll`);
        toast(r.sent ? `Sent ${r.sent} new item${r.sent > 1 ? 's' : ''}` : 'Checked: nothing new');
      } else if (what === 'test') {
        const r = await api('POST', `/api/feeds/${id}/test`);
        toast(`Sent “${r.item.title || r.item.link}”${r.channels?.length ? ` to #${r.channels.join(', #')}` : ''}`);
      }
    } catch (err) {
      toast(err.problems ? err.problems.join(' ') : err.message, 'err');
    }
    btn.disabled = false;
    state = await api('GET', '/api/state');
    renderList();
  }

  function confirmDelete(v) {
    const scrim = document.createElement('div');
    scrim.className = 'scrim';
    const m = document.createElement('div');
    m.className = 'modal';
    m.setAttribute('role', 'dialog');
    m.setAttribute('aria-modal', 'true');
    m.innerHTML = `<h2>Delete ${esc(v.feed.name)}?</h2>
      <p>The relay stops reading it and forgets which items it sent. Events already sent stay where they are.</p>
      <div class="row"><button class="btn" data-x>Keep it</button><button class="btn danger" data-del>Delete</button></div>`;
    const close = () => { scrim.remove(); m.remove(); document.removeEventListener('keydown', onKey); };
    const onKey = (e) => { if (e.key === 'Escape') close(); };
    document.addEventListener('keydown', onKey);
    scrim.onclick = close;
    m.querySelector('[data-x]').onclick = close;
    m.querySelector('[data-del]').onclick = async () => {
      try {
        await api('DELETE', `/api/feeds/${v.id}`);
        toast(`Deleted ${v.feed.name}`);
        close();
        state = await api('GET', '/api/state');
        renderList();
      } catch (err) { toast(err.message, 'err'); }
    };
    document.body.append(scrim, m);
    m.querySelector('[data-x]').focus();
  }

  // --- live updates through Hibira ---
  // A tiny Pusher-protocol client: the relay publishes feeds.changed on a
  // private channel whenever a feed changes, and this page listens.
  const LIVE_TEXT = {
    added: (e) => `added “${e.feed}”`,
    updated: (e) => (e.was ? `renamed “${e.was}” to “${e.feed}”` : `changed “${e.feed}”`),
    deleted: (e) => `deleted “${e.feed}”`,
    paused: (e) => `paused “${e.feed}”`,
    resumed: (e) => `resumed “${e.feed}”`,
  };

  function setLive(status, reason) {
    live = { ...(live || {}), status, reason };
    const el = $('#live-chip');
    if (!el) return;
    const tip = status === 'on' ? 'Changes made by AI agents over MCP show up here at once'
      : status === 'connecting' ? 'Connecting to Hibira…' : reason;
    el.innerHTML = `<span class="chip ${status === 'on' ? 'ok' : status === 'connecting' ? '' : 'warn'}" data-tip="${esc(tip)}" tabindex="0"><span class="dot"></span>${status === 'on' ? 'Live' : status === 'connecting' ? 'Connecting' : 'Live off'}</span>`;
  }

  function stopLive() {
    if (live?.ws) { live.closing = true; live.ws.close(); }
    clearTimeout(live?.retry);
    live = null;
  }

  function startLive(attempt = 0) {
    const cfg = state.live;
    if (!cfg || (live?.ws && live.ws.readyState <= 1)) { if (live) setLive(live.status, live.reason); return; }
    setLive('connecting');
    const port = (cfg.tls && cfg.port === 443) || (!cfg.tls && cfg.port === 80) ? '' : `:${cfg.port}`;
    const ws = new WebSocket(`${cfg.tls ? 'wss' : 'ws'}://${cfg.host}${port}/app/${encodeURIComponent(cfg.key)}?protocol=7&client=feed-relay-console&version=1.0`);
    live = { ws, status: 'connecting' };
    let opened = false;
    ws.onmessage = async (m) => {
      let msg;
      try { msg = JSON.parse(m.data); } catch { return; }
      const data = typeof msg.data === 'string' ? (() => { try { return JSON.parse(msg.data); } catch { return msg.data; } })() : msg.data;
      if (msg.event === 'pusher:connection_established') {
        opened = true;
        try {
          const { auth } = await api('POST', '/api/live/auth', { socket_id: data.socket_id, channel_name: cfg.channel });
          ws.send(JSON.stringify({ event: 'pusher:subscribe', data: { channel: cfg.channel, auth } }));
        } catch (err) { setLive('off', err.message); }
      } else if (msg.event === 'pusher_internal:subscription_succeeded') {
        setLive('on');
        attempt = 0;
      } else if (msg.event === 'pusher:ping') {
        ws.send(JSON.stringify({ event: 'pusher:pong', data: {} }));
      } else if (msg.event === 'pusher:error') {
        const code = data?.code;
        setLive('off', code === 4009 || code === 4003 || /origin/i.test(data?.message || '')
          ? `Hibira refused this page’s origin (${location.origin}). Add it to the app’s allowed origins.`
          : `Hibira: ${data?.message || 'error'}`);
      } else if (msg.event === 'pusher:subscription_error') {
        setLive('off', 'Hibira did not accept the subscription.');
      } else if (msg.event === 'feeds.changed' && msg.channel === cfg.channel) {
        onLiveChange(data);
      }
    };
    ws.onclose = () => {
      if (live?.closing) return;
      if (!opened && (!live || live.status !== 'off')) {
        setLive('off', `Could not reach Hibira from this page. If the app is on the Free plan, add ${location.origin} to its allowed origins.`);
      }
      const wait = Math.min(30000, 1000 * 2 ** attempt);
      live.retry = setTimeout(() => startLive(attempt + 1), wait);
    };
  }

  let liveRefresh = null;
  function onLiveChange(e) {
    if (e.by === 'mcp') toast(`AI agent ${(LIVE_TEXT[e.action] || ((x) => `changed “${x.feed}”`))(e)} over MCP`, 'live');
    clearTimeout(liveRefresh);
    liveRefresh = setTimeout(async () => {
      if ($('.drawer') || $('.modal')) return;
      try { state = await api('GET', '/api/state'); renderList(); flash(e.feed); } catch { /* shown by api() */ }
    }, 250);
  }

  function flash(name) {
    const v = state.feeds.find((x) => x.feed.name === name);
    const row = v && document.querySelector(`.feed[data-id="${v.id}"]`);
    if (!row) return;
    row.classList.remove('flash');
    void row.offsetWidth;
    row.classList.add('flash');
  }

  // --- editor ---
  function chips(id, values, placeholder) {
    return `<div class="chips" data-chips="${id}">${(values || []).map(tag).join('')}<input aria-label="${esc(placeholder)}" placeholder="${esc(placeholder)}"></div>`;
  }
  function tag(v) {
    return `<span class="tag" data-v="${esc(v)}">${esc(v)}<button type="button" aria-label="Remove ${esc(v)}">${icon('x')}</button></span>`;
  }
  function wireChips(root) {
    root.querySelectorAll('[data-chips]').forEach((box) => {
      const input = box.querySelector('input');
      const add = () => {
        for (const v of input.value.split(/[,\s]+/).map((s) => s.trim()).filter(Boolean)) {
          if (![...box.querySelectorAll('.tag')].some((t) => t.dataset.v === v)) input.insertAdjacentHTML('beforebegin', tag(v));
        }
        input.value = '';
      };
      box.onclick = (e) => {
        const rm = e.target.closest('.tag button');
        if (rm) rm.parentElement.remove();
        else input.focus();
      };
      input.onkeydown = (e) => {
        if (e.key === 'Enter' || e.key === ',' || e.key === ' ') { e.preventDefault(); add(); }
        if (e.key === 'Backspace' && !input.value) box.querySelector('.tag:last-of-type')?.remove();
      };
      input.onblur = add;
    });
  }
  const chipValues = (root, id) => [...root.querySelectorAll(`[data-chips="${id}"] .tag`)].map((t) => t.dataset.v);

  function seg(id, options, value) {
    return `<div class="seg" role="radiogroup" data-seg="${id}">${options.map(([v, label]) =>
      `<button type="button" role="radio" data-v="${esc(v)}" aria-checked="${v === value}">${esc(label)}</button>`).join('')}</div>`;
  }
  function wireSeg(root) {
    root.querySelectorAll('[data-seg]').forEach((g) => {
      g.onclick = (e) => {
        const b = e.target.closest('button');
        if (!b) return;
        g.querySelectorAll('button').forEach((x) => x.setAttribute('aria-checked', String(x === b)));
        g.dispatchEvent(new Event('change'));
      };
    });
  }
  const segValue = (root, id) => root.querySelector(`[data-seg="${id}"] [aria-checked="true"]`)?.dataset.v;

  function openEditor(v) {
    const f = v ? JSON.parse(JSON.stringify(v.feed)) : { interval: state.defaults.interval };
    const intervals = ['1m', '5m', '15m', '1h'];
    const custom = !intervals.includes(f.interval);
    const scrim = document.createElement('div');
    scrim.className = 'scrim';
    const d = document.createElement('form');
    d.className = 'drawer';
    d.setAttribute('role', 'dialog');
    d.setAttribute('aria-modal', 'true');
    d.innerHTML = `
      <header><h2>${v ? 'Edit feed' : 'Add feed'}</h2>
        <button type="button" class="icon-btn" data-close data-tip="Close" data-tip-end aria-label="Close">${icon('x')}</button></header>
      <div class="scroll">
        <div id="problems"></div>
        <div class="field"><label for="f-url">Feed URL</label>
          <div class="row"><input class="input mono" id="f-url" value="${esc(f.url)}" placeholder="${state.rsshub_url ? '/github/release/owner/repo or https://…' : 'https://example.com/feed.xml'}" autocomplete="off" spellcheck="false">
          <button type="button" class="btn" id="f-preview">${icon('eye')}Preview</button></div>
          <span class="hint">${state.rsshub_url ? `RSS, Atom or JSON Feed, or an RSSHub route starting with / (read from ${esc(state.rsshub_url)}).` : 'RSS, Atom or JSON Feed, including any RSSHub URL.'}</span></div>
        <div id="f-preview-out"></div>
        <div class="field"><label for="f-name">Name</label>
          <input class="input" id="f-name" value="${esc(f.name && f.name !== f.url ? f.name : '')}" placeholder="Shown here and sent as feed.name"></div>
        <div class="field"><span class="label">Channels</span>
          ${chips('channels', f.channels, 'releases, news… press Enter')}
          <span class="hint">Every open page subscribed to these channels gets the item.</span></div>
        <div class="grid2">
          <div class="field"><label for="f-event">Event name</label>
            <input class="input mono" id="f-event" value="${esc(f.event || '')}" placeholder="${esc(state.defaults.event)}"></div>
          <div class="field"><span class="label">Items on the first check</span>
            <div><div class="stepper"><button type="button" data-step="-1" aria-label="Fewer">${icon('minus')}</button>
            <output id="f-backfill">${f.backfill || 0}</output>
            <button type="button" data-step="1" aria-label="More">${icon('plus')}</button></div></div>
            <span class="hint">0 sends only items that appear later.</span></div>
        </div>
        <div class="field"><span class="label">Check every</span>
          <div class="row wrap-row">${seg('interval', [...intervals.map((i) => [i, i]), ['custom', 'Other']], custom ? 'custom' : f.interval)}
          <input class="input mono small ${custom ? '' : 'hidden'}" id="f-interval" value="${esc(custom ? f.interval : '')}" placeholder="10m"></div>
          <span class="hint">At least 30s. Be kind to the sites you read.</span></div>
        ${state.websub ? `<div class="field"><span class="label">WebSub</span>${seg('websub', [['auto', 'When the feed has a hub'], ['off', 'Poll only']], f.websub === false ? 'off' : 'auto')}</div>` : ''}
        ${section('push', 'Push notification', state.hibira.push ? 'To iOS, Android and web devices by interest or user' : 'Set HIBIRA_PUSH_INSTANCE_ID and HIBIRA_PUSH_SECRET_KEY to use push', !!f.push, `
          <div class="field"><span class="label">Interests</span>${chips('interests', f.push?.interests, 'interest names')}</div>
          <div class="field"><span class="label">Users</span>${chips('push-users', f.push?.users, 'user IDs')}</div>
          <div class="field"><label for="f-ptitle">Title</label><input class="input" id="f-ptitle" value="${esc(f.push?.title || '')}" placeholder="The feed's title"></div>`, !state.hibira.push)}
        ${section('inbox', 'Inbox notification', 'Stored in each user’s notification inbox', !!f.inbox, `
          <div class="field"><span class="label">Users</span>${chips('inbox-users', f.inbox?.users, 'user IDs')}</div>
          <div class="grid2"><div class="field"><label for="f-cat">Category</label><input class="input mono" id="f-cat" value="${esc(f.inbox?.category || '')}" placeholder="optional"></div>
          <div class="field"><span class="label">Also push</span>${seg('inbox-push', [['offline', 'If offline'], ['always', 'Always'], ['never', 'Never']], f.inbox?.push || 'offline')}</div></div>`)}
        ${section('headers', 'Request headers', 'For feeds behind a key, such as a private RSSHub', !!(f.headers && Object.keys(f.headers).length), `
          <div id="hdrs">${Object.entries(f.headers || {}).map(([k, val]) => hdrRow(k, val)).join('')}</div>
          <button type="button" class="btn spaced" id="hdr-add">${icon('plus')}Add header</button>`)}
      </div>
      <footer><button type="button" class="btn" data-close>Cancel</button><button class="btn primary" type="submit">${v ? 'Save' : 'Add feed'}</button></footer>`;

    const close = () => { scrim.remove(); d.remove(); document.removeEventListener('keydown', onKey); };
    const onKey = (e) => { if (e.key === 'Escape') close(); };
    document.addEventListener('keydown', onKey);
    scrim.onclick = close;
    d.querySelectorAll('[data-close]').forEach((b) => (b.onclick = close));
    wireChips(d);
    wireSeg(d);
    d.querySelector('[data-seg="interval"]').addEventListener('change', () => {
      const other = segValue(d, 'interval') === 'custom';
      $('#f-interval', d).classList.toggle('hidden', !other);
      if (other) $('#f-interval', d).focus();
    });
    d.querySelectorAll('[data-step]').forEach((b) => (b.onclick = () => {
      const o = $('#f-backfill', d);
      o.textContent = Math.max(0, Math.min(20, Number(o.textContent) + Number(b.dataset.step)));
    }));
    d.querySelectorAll('[data-section]').forEach((sw) => (sw.onclick = () => {
      const on = sw.getAttribute('aria-checked') !== 'true';
      sw.setAttribute('aria-checked', String(on));
      d.querySelector(`[data-sec-body="${sw.dataset.section}"]`).classList.toggle('hidden', !on);
    }));
    const wireHdrRows = () => d.querySelectorAll('[data-hdr-rm]').forEach((b) => (b.onclick = () => b.closest('.row').remove()));
    $('#hdr-add', d).onclick = () => { $('#hdrs', d).insertAdjacentHTML('beforeend', hdrRow('', '')); wireHdrRows(); };
    wireHdrRows();
    $('#f-preview', d).onclick = () => preview(d);

    d.onsubmit = async (e) => {
      e.preventDefault();
      const on = (s) => d.querySelector(`[data-section="${s}"]`).getAttribute('aria-checked') === 'true';
      const iv = segValue(d, 'interval');
      const body = {
        name: $('#f-name', d).value.trim(),
        url: $('#f-url', d).value.trim(),
        channels: chipValues(d, 'channels'),
        event: $('#f-event', d).value.trim(),
        interval: iv === 'custom' ? $('#f-interval', d).value.trim() : iv,
        backfill: Number($('#f-backfill', d).textContent),
        disabled: v ? v.feed.disabled : false,
      };
      if (state.websub && segValue(d, 'websub') === 'off') body.websub = false;
      if (on('push')) body.push = { interests: chipValues(d, 'interests'), users: chipValues(d, 'push-users'), title: $('#f-ptitle', d).value.trim() };
      if (on('inbox')) body.inbox = { users: chipValues(d, 'inbox-users'), category: $('#f-cat', d).value.trim(), push: segValue(d, 'inbox-push') };
      if (on('headers')) {
        body.headers = {};
        d.querySelectorAll('#hdrs .hdr-row').forEach((r) => {
          const [k, val] = r.querySelectorAll('input');
          if (k.value.trim()) body.headers[k.value.trim()] = val.value;
        });
      }
      const btn = d.querySelector('[type=submit]');
      btn.disabled = true;
      try {
        await api('POST', v ? `/api/feeds/${v.id}` : '/api/feeds', body);
        toast(v ? 'Saved' : `Added ${body.name || body.url}. Its current items are remembered, new ones will be sent.`);
        close();
        state = await api('GET', '/api/state');
        renderList();
      } catch (err) {
        $('#problems', d).innerHTML = `<div class="problems"><b>${esc(err.message)}</b>${err.problems ? `<ul>${err.problems.map((p) => `<li>${esc(p)}</li>`).join('')}</ul>` : ''}</div>`;
        d.querySelector('.scroll').scrollTop = 0;
        btn.disabled = false;
      }
    };
    document.body.append(scrim, d);
    $('#f-url', d).focus();
  }

  function section(id, title, sub, on, body, locked) {
    return `<div class="section"><div class="sec-head"><div class="grow"><b>${esc(title)}</b><span>${esc(sub)}</span></div>
      <button type="button" class="switch" role="switch" aria-checked="${on}" data-section="${id}" aria-label="${esc(title)}" ${locked && !on ? 'disabled' : ''}></button></div>
      <div class="sec-body ${on ? '' : 'hidden'}" data-sec-body="${id}">${body}</div></div>`;
  }
  function hdrRow(k, val) {
    return `<div class="row hdr-row"><input class="input mono" value="${esc(k)}" placeholder="Header" aria-label="Header name">
      <input class="input mono" value="${esc(val)}" placeholder="Value" aria-label="Header value">
      <button type="button" class="icon-btn danger" data-hdr-rm data-tip="Remove" data-tip-end aria-label="Remove header">${icon('trash')}</button></div>`;
  }

  async function preview(d) {
    const out = $('#f-preview-out', d);
    const btn = $('#f-preview', d);
    const url = $('#f-url', d).value.trim();
    if (!url) { $('#f-url', d).focus(); return; }
    btn.disabled = true;
    out.innerHTML = '<div class="preview"><span class="hint">Reading the feed…</span></div>';
    const headers = {};
    d.querySelectorAll('#hdrs .hdr-row').forEach((r) => {
      const [k, val] = r.querySelectorAll('input');
      if (k.value.trim()) headers[k.value.trim()] = val.value;
    });
    try {
      const p = await api('POST', '/api/preview', { url, headers });
      out.innerHTML = `<div class="preview">
        <div class="ptitle">${esc(p.title || 'Untitled feed')}<span class="chip">${p.count} items</span>${p.hub ? '<span class="chip ok" data-tip="New items can arrive within seconds" tabindex="0">WebSub hub</span>' : ''}</div>
        <ol>${p.items.map((x) => `<li><span class="t" title="${esc(x.item.title)}">${esc(x.item.title || x.item.link || x.item.id)}</span>
          <span class="b" data-tip="Size of the event (limit ${p.max_bytes})" data-tip-end tabindex="0">${(x.bytes / 1024).toFixed(1)} KB</span></li>`).join('')}</ol></div>`;
      if (!$('#f-name', d).value && p.title) $('#f-name', d).value = p.title;
    } catch (err) {
      out.innerHTML = `<div class="problems">${esc(err.problems ? err.problems.join(' ') : err.message)}</div>`;
    }
    btn.disabled = false;
  }

  load();
})();
