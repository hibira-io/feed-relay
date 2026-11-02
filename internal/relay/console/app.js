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
    sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
    moon: '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>',
    monitor: '<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8M12 17v4"/>',
    globe: '<circle cx="12" cy="12" r="10"/><path d="M2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    search: '<circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/>',
    book: '<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20V3H6.5A2.5 2.5 0 0 0 4 5.5z"/><path d="M4 19.5A2.5 2.5 0 0 0 6.5 22H20v-5"/>',
    chevron: '<path d="m9 18 6-6-6-6"/>',
  };
  const icon = (name) => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICON[name]}</svg>`;
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
  const $ = (sel, root = document) => root.querySelector(sel);
  const app = $('#app');

  let state = null;
  let timer = null;
  let live = null; // { ws, status, reason }

  // --- language ---
  const I18N = window.FEED_RELAY_I18N;
  const store = {
    get: (k) => { try { return localStorage.getItem(k); } catch { return null; } },
    set: (k, v) => { try { if (v) localStorage.setItem(k, v); else localStorage.removeItem(k); } catch { /* not saved */ } },
  };
  function pickLang() {
    const saved = store.get('feed-relay-lang');
    if (saved && I18N.dict[saved]) return saved;
    for (const l of navigator.languages || [navigator.language || 'en']) {
      const x = l.toLowerCase();
      if (x.startsWith('zh')) return /tw|hk|mo|hant/.test(x) ? 'zh-TW' : 'zh-CN';
      const base = x.split('-')[0];
      if (I18N.dict[base]) return base;
    }
    return 'en';
  }
  let lang = pickLang();
  document.documentElement.lang = lang;
  function t(key, vars) {
    const d = I18N.dict[lang] || {}, en = I18N.dict.en;
    const one = vars && vars.n === 1 ? key + '_one' : null;
    let s = (one && (d[one] ?? (lang === 'en' ? en[one] : undefined))) ?? d[key] ?? en[key] ?? key;
    if (vars) s = s.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? vars[k] : m));
    return s;
  }
  // tHtml escapes the text and puts in vars that are already HTML.
  function tHtml(key, html) {
    return esc(t(key)).replace(/\{(\w+)\}/g, (m, k) => (k in html ? html[k] : m));
  }
  document.title = t('appName');

  // --- theme ---
  const THEMES = ['system', 'light', 'dark'];
  const THEME_ICON = { system: 'monitor', light: 'sun', dark: 'moon' };
  const THEME_TEXT = { system: 'themeSystem', light: 'themeLight', dark: 'themeDark' };
  let theme = document.documentElement.dataset.theme || 'system';
  function setTheme(next) {
    theme = next;
    if (next === 'system') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = next;
    store.set('feed-relay-theme', next === 'system' ? '' : next);
    document.querySelectorAll('[data-pref="theme"]').forEach(paintThemeBtn);
  }
  function paintThemeBtn(b) {
    b.innerHTML = icon(THEME_ICON[theme]);
    b.dataset.tip = t(THEME_TEXT[theme]);
    b.setAttribute('aria-label', t(THEME_TEXT[theme]));
  }

  // prefs is the theme and language buttons, on the sign-in page and the top bar.
  function prefs() {
    return `<button class="icon-btn" data-pref="theme"></button>
      <span class="menu-wrap"><button class="icon-btn" data-pref="lang" data-tip="${esc(t('language'))}" aria-label="${esc(t('language'))}" aria-haspopup="menu" aria-expanded="false">${icon('globe')}</button></span>`;
  }
  function wirePrefs(root) {
    root.querySelectorAll('[data-pref="theme"]').forEach((b) => {
      paintThemeBtn(b);
      b.onclick = () => setTheme(THEMES[(THEMES.indexOf(theme) + 1) % THEMES.length]);
    });
    root.querySelectorAll('[data-pref="lang"]').forEach((b) => (b.onclick = (e) => { e.stopPropagation(); openLangMenu(b); }));
  }
  function openLangMenu(btn) {
    const open = $('.menu');
    if (open) { open.remove(); btn.setAttribute('aria-expanded', 'false'); return; }
    const m = document.createElement('div');
    m.className = 'menu';
    m.setAttribute('role', 'menu');
    m.innerHTML = I18N.langs.map(([code, label]) => `<button type="button" role="menuitemradio" aria-checked="${code === lang}" data-lang="${code}" lang="${code}">${icon('check')}<span>${esc(label)}</span></button>`).join('');
    btn.parentElement.append(m);
    btn.setAttribute('aria-expanded', 'true');
    const close = () => { m.remove(); btn.setAttribute('aria-expanded', 'false'); document.removeEventListener('click', close); document.removeEventListener('keydown', onKey); };
    const onKey = (e) => { if (e.key === 'Escape') { close(); btn.focus(); } };
    document.addEventListener('click', close);
    document.addEventListener('keydown', onKey);
    m.onclick = (e) => {
      const b = e.target.closest('[data-lang]');
      if (!b) return;
      lang = b.dataset.lang;
      store.set('feed-relay-lang', lang);
      document.documentElement.lang = lang;
      document.title = t('appName');
      close();
      if (state) { renderMain(); } else showLogin();
    };
    (m.querySelector('[aria-checked="true"]') || m.querySelector('button')).focus();
  }

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
      throw new Error(t('signInAgain'));
    }
    const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
    if (!res.ok) {
      const err = new Error(data.error || t('requestFailed', { status: res.status }));
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
    if (s < 45) return t('justNow');
    if (s < 3600) return t('minAgo', { n: Math.round(s / 60) });
    if (s < 86400) return t('hAgo', { n: Math.round(s / 3600) });
    return t('dAgo', { n: Math.round(s / 86400) });
  }

  // --- login ---
  function showLogin() {
    clearInterval(timer);
    state = null;
    app.innerHTML = `
      <div class="corner">${prefs()}</div>
      <div class="login"><form class="card" id="login">
        <img src="/console/icon.svg" width="40" height="40" alt="">
        <h1>${esc(t('appName'))}</h1>
        <p>${esc(t('loginHint'))}</p>
        <div class="field"><label for="pw">${esc(t('password'))}</label>
          <input class="input" id="pw" type="password" autocomplete="current-password" required></div>
        <button class="btn primary block" type="submit">${esc(t('signIn'))}</button>
      </form></div>`;
    wirePrefs(app);
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
          <div class="brand"><img src="/console/icon.svg" alt="">${esc(t('appName'))}</div>
          <span class="chip hide-sm">${esc(state.version)}</span>
          <div class="grow"></div>
          <span id="live-chip"></span>
          <span class="chip ok hide-sm" data-tip="${esc(h.url)}" tabindex="0"><span class="dot"></span>${esc(t('app', { id: h.app_id }))}</span>
          ${prefs()}
          <button class="icon-btn" id="logout" data-tip="${esc(t('signOut'))}" data-tip-end aria-label="${esc(t('signOut'))}">${icon('logout')}</button>
        </div>
        <div class="head">
          <div><h1>${esc(t('feeds'))}</h1><p>${esc(t('feedsSub'))}</p></div>
          <div class="grow"></div>
          <div class="stats" id="stats"></div>
          <button class="btn primary" id="add">${icon('plus')}${esc(t('addFeed'))}</button>
        </div>
        <div class="list" id="list"></div>
      </div>`;
    $('#logout').onclick = async () => { await api('POST', '/api/logout'); stopLive(); showLogin(); };
    $('#add').onclick = () => openEditor(null);
    wirePrefs(app);
    renderList();
    if (live) setLive(live.status, live.reason);
    startLive();
  }

  function targetsOf(f) {
    const out = [];
    for (const c of f.channels || []) out.push(`<span class="chip">#${esc(c)}</span>`);
    if (f.push) out.push(`<span class="chip" data-tip="${esc([...(f.push.interests || []), ...(f.push.users || [])].join(', '))}" tabindex="0">${esc(t('push'))}</span>`);
    if (f.inbox) out.push(`<span class="chip" data-tip="${esc(t('nUsers', { n: f.inbox.users.length }))}" tabindex="0">${esc(t('inbox'))}</span>`);
    return out.join('');
  }

  function statusOf(v) {
    const s = v.status, f = v.feed;
    if (f.disabled) return { text: t('paused'), cls: '' };
    if (s.last_error) return { text: s.last_error, cls: 'err' };
    const parts = [];
    parts.push(s.last_fetch ? t('checkedAgo', { ago: ago(s.last_fetch) }) : t('waitingFirst'));
    parts.push(t('every', { interval: f.interval }));
    if (s.sent) parts.push(t('nSent', { n: s.sent }) + (s.last_sent ? ', ' + t('lastAgo', { ago: ago(s.last_sent) }) : ''));
    if (s.websub === 'active') parts.push(t('websubOn'));
    return { text: parts.join(' · '), cls: '' };
  }

  function renderList() {
    const list = $('#list');
    if (!list) return;
    const feeds = state.feeds;
    const sent = feeds.reduce((n, v) => n + (v.status.sent || 0), 0);
    const errs = feeds.filter((v) => v.status.last_error && !v.feed.disabled).length;
    $('#stats').innerHTML = `
      <div class="stat"><b>${feeds.length}</b><span>${esc(t('statFeeds', { n: feeds.length }))}</span></div>
      <div class="stat"><b>${sent}</b><span>${esc(t('statSent'))}</span></div>
      ${errs ? `<div class="stat"><b class="err-text">${errs}</b><span>${esc(t('statFailing'))}</span></div>` : ''}`;
    if (!feeds.length) {
      list.innerHTML = `<div class="empty">
        <div class="empty-mark"><div class="mark">${icon('rss')}</div></div>
        <h2>${esc(t('noFeeds'))}</h2>
        <p>${state.rsshub_url ? tHtml('emptyTextRsshub', { route: '<code>/github/release/owner/repo</code>' }) : esc(t('emptyText'))}</p>
        <button class="btn primary" id="add2">${icon('plus')}${esc(t('addFirst'))}</button></div>`;
      $('#add2').onclick = () => openEditor(null);
      return;
    }
    list.innerHTML = feeds.map((v) => {
      const f = v.feed, st = statusOf(v), file = v.source === 'file';
      return `<div class="feed${f.disabled ? ' paused' : ''}" data-id="${v.id}">
        <div class="mark">${icon('rss')}</div>
        <div class="body">
          <div class="name">${esc(f.name)}${file ? `<span class="chip" data-tip="${esc(t('setInConfig'))}" tabindex="0">${icon('lock').replace('<svg', '<svg width="11" height="11"')}${esc(t('configChip'))}</span>` : ''}</div>
          <div class="url" title="${esc(f.url)}">${esc(f.url)}</div>
          <div class="meta">${targetsOf(f)}<span class="status ${st.cls}">${esc(st.text)}</span></div>
        </div>
        <div class="actions">
          <button class="switch" role="switch" aria-checked="${!f.disabled}" data-act="pause" ${file ? 'disabled' : ''}
            data-tip="${esc(file ? t('changeInConfig') : f.disabled ? t('pausedClickRun') : t('runningClickPause'))}" aria-label="${esc(t('running'))}"></button>
          <button class="icon-btn" data-act="poll" data-tip="${esc(t('checkNow'))}" aria-label="${esc(t('checkNow'))}" ${f.disabled ? 'disabled' : ''}>${icon('refresh')}</button>
          <button class="icon-btn" data-act="test" data-tip="${esc(t('sendTestTip'))}" aria-label="${esc(t('sendTest'))}">${icon('send')}</button>
          ${file ? '' : `<button class="icon-btn" data-act="edit" data-tip="${esc(t('edit'))}" aria-label="${esc(t('edit'))}">${icon('edit')}</button>
          <button class="icon-btn danger" data-act="delete" data-tip="${esc(t('delete'))}" data-tip-end aria-label="${esc(t('delete'))}">${icon('trash')}</button>`}
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
        toast(t(v.feed.disabled ? 'toastRunning' : 'toastPaused', { name: v.feed.name }));
      } else if (what === 'poll') {
        const r = await api('POST', `/api/feeds/${id}/poll`);
        toast(r.sent ? t('sentItems', { n: r.sent }) : t('nothingNew'));
      } else if (what === 'test') {
        const r = await api('POST', `/api/feeds/${id}/test`);
        const title = r.item.title || r.item.link;
        toast(r.channels?.length ? t('testSentTo', { title, channels: '#' + r.channels.join(', #') }) : t('testSent', { title }));
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
    m.innerHTML = `<h2>${esc(t('deleteTitle', { name: v.feed.name }))}</h2>
      <p>${esc(t('deleteText'))}</p>
      <div class="row"><button class="btn" data-x>${esc(t('keep'))}</button><button class="btn danger" data-del>${esc(t('delete'))}</button></div>`;
    const close = () => { scrim.remove(); m.remove(); document.removeEventListener('keydown', onKey); };
    const onKey = (e) => { if (e.key === 'Escape') close(); };
    document.addEventListener('keydown', onKey);
    scrim.onclick = close;
    m.querySelector('[data-x]').onclick = close;
    m.querySelector('[data-del]').onclick = async () => {
      try {
        await api('DELETE', `/api/feeds/${v.id}`);
        toast(t('deleted', { name: v.feed.name }));
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
  function mcpText(e) {
    const key = e.action === 'updated' ? (e.was && e.was !== e.feed ? 'mcpRenamed' : 'mcpChanged')
      : { added: 'mcpAdded', deleted: 'mcpDeleted', paused: 'mcpPaused', resumed: 'mcpResumed' }[e.action] || 'mcpChanged';
    return t(key, { feed: e.feed, was: e.was });
  }

  function setLive(status, reason) {
    live = { ...(live || {}), status, reason };
    const el = $('#live-chip');
    if (!el) return;
    const tip = status === 'on' ? t('liveOnTip') : status === 'connecting' ? t('liveConnectingTip') : reasonText(reason);
    el.innerHTML = `<span class="chip ${status === 'on' ? 'ok' : status === 'connecting' ? '' : 'warn'}" data-tip="${esc(tip)}" data-tip-wrap tabindex="0"><span class="dot"></span>${esc(t(status === 'on' ? 'liveOn' : status === 'connecting' ? 'liveConnecting' : 'liveOff'))}</span>`;
  }

  // reasons are kept as [key, vars] so they follow a language change.
  function reasonText(r) {
    if (!r) return '';
    return Array.isArray(r) ? t(r[0], r[1]) : r;
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
          ? ['originRefused', { origin: location.origin }]
          : ['hibiraError', { message: data?.message || 'error' }]);
      } else if (msg.event === 'pusher:subscription_error') {
        setLive('off', ['subscriptionRefused']);
      } else if (msg.event === 'feeds.changed' && msg.channel === cfg.channel) {
        onLiveChange(data);
      }
    };
    ws.onclose = () => {
      if (live?.closing) return;
      if (!opened && (!live || live.status !== 'off')) {
        setLive('off', ['unreachable', { origin: location.origin }]);
      }
      const wait = Math.min(30000, 1000 * 2 ** attempt);
      live.retry = setTimeout(() => startLive(attempt + 1), wait);
    };
  }

  let liveRefresh = null;
  function onLiveChange(e) {
    if (e.by === 'mcp') toast(mcpText(e), 'live');
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
    return `<span class="tag" data-v="${esc(v)}">${esc(v)}<button type="button" aria-label="${esc(t('removeValue', { value: v }))}">${icon('x')}</button></span>`;
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
      <header><h2>${esc(t(v ? 'editFeed' : 'addFeed'))}</h2>
        <button type="button" class="icon-btn" data-close data-tip="${esc(t('close'))}" data-tip-end aria-label="${esc(t('close'))}">${icon('x')}</button></header>
      <div class="scroll">
        <div id="problems"></div>
        ${finderHtml(!v)}
        <div class="field"><label for="f-url">${esc(t('feedUrl'))}</label>
          <div class="row"><input class="input mono" id="f-url" value="${esc(f.url)}" placeholder="${esc(state.rsshub_url ? t('urlPhRoute') : 'https://example.com/feed.xml')}" autocomplete="off" spellcheck="false">
          <button type="button" class="icon-btn" id="f-find" data-tip="${esc(t('findFeed'))}" data-tip-end aria-label="${esc(t('findFeed'))}" aria-expanded="${!v}">${icon('search')}</button>
          <button type="button" class="btn" id="f-preview">${icon('eye')}${esc(t('preview'))}</button></div>
          <span class="hint">${esc(state.rsshub_url ? t('urlHintRsshub', { url: state.rsshub_url }) : t('urlHint'))}</span></div>
        <div id="f-preview-out"></div>
        <div class="field"><label for="f-name">${esc(t('name'))}</label>
          <input class="input" id="f-name" value="${esc(f.name && f.name !== f.url ? f.name : '')}" placeholder="${esc(t('namePh'))}"></div>
        <div class="field"><span class="label">${esc(t('channels'))}</span>
          ${chips('channels', f.channels, t('channelsPh'))}
          <span class="hint">${esc(t('channelsHint'))}</span></div>
        <div class="grid2">
          <div class="field"><label for="f-event">${esc(t('eventName'))}</label>
            <input class="input mono" id="f-event" value="${esc(f.event || '')}" placeholder="${esc(state.defaults.event)}"></div>
          <div class="field"><span class="label">${esc(t('firstItems'))}</span>
            <div><div class="stepper"><button type="button" data-step="-1" aria-label="${esc(t('fewer'))}">${icon('minus')}</button>
            <output id="f-backfill">${f.backfill || 0}</output>
            <button type="button" data-step="1" aria-label="${esc(t('more'))}">${icon('plus')}</button></div></div>
            <span class="hint">${esc(t('backfillHint'))}</span></div>
        </div>
        <div class="field"><span class="label">${esc(t('checkEvery'))}</span>
          <div class="row wrap-row">${seg('interval', [...intervals.map((i) => [i, i]), ['custom', t('other')]], custom ? 'custom' : f.interval)}
          <input class="input mono small ${custom ? '' : 'hidden'}" id="f-interval" value="${esc(custom ? f.interval : '')}" placeholder="10m"></div>
          <span class="hint">${esc(t('intervalHint'))}</span></div>
        ${state.websub ? `<div class="field"><span class="label">${esc(t('websub'))}</span>${seg('websub', [['auto', t('websubAuto')], ['off', t('websubOff')]], f.websub === false ? 'off' : 'auto')}</div>` : ''}
        ${section('push', t('pushTitle'), state.hibira.push ? t('pushSub') : t('pushNeedsKeys'), !!f.push, `
          <div class="field"><span class="label">${esc(t('interests'))}</span>${chips('interests', f.push?.interests, t('interestsPh'))}</div>
          <div class="field"><span class="label">${esc(t('users'))}</span>${chips('push-users', f.push?.users, t('userIdsPh'))}</div>
          <div class="field"><label for="f-ptitle">${esc(t('title'))}</label><input class="input" id="f-ptitle" value="${esc(f.push?.title || '')}" placeholder="${esc(t('titlePh'))}"></div>`, !state.hibira.push)}
        ${section('inbox', t('inboxTitle'), t('inboxSub'), !!f.inbox, `
          <div class="field"><span class="label">${esc(t('users'))}</span>${chips('inbox-users', f.inbox?.users, t('userIdsPh'))}</div>
          <div class="grid2"><div class="field"><label for="f-cat">${esc(t('category'))}</label><input class="input mono" id="f-cat" value="${esc(f.inbox?.category || '')}" placeholder="${esc(t('optional'))}"></div>
          <div class="field"><span class="label">${esc(t('alsoPush'))}</span>${seg('inbox-push', [['offline', t('ifOffline')], ['always', t('always')], ['never', t('never')]], f.inbox?.push || 'offline')}</div></div>`)}
        ${section('headers', t('headersTitle'), t('headersSub'), !!(f.headers && Object.keys(f.headers).length), `
          <div id="hdrs">${Object.entries(f.headers || {}).map(([k, val]) => hdrRow(k, val)).join('')}</div>
          <button type="button" class="btn spaced" id="hdr-add">${icon('plus')}${esc(t('addHeader'))}</button>`)}
      </div>
      <footer><button type="button" class="btn" data-close>${esc(t('cancel'))}</button><button class="btn primary" type="submit">${esc(t(v ? 'save' : 'addFeed'))}</button></footer>`;

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
    wireFinder(d);

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
        toast(v ? t('saved') : t('added', { name: body.name || body.url }));
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
    ($('#find-q', d) && !v ? $('#find-q', d) : $('#f-url', d)).focus();
  }

  function section(id, title, sub, on, body, locked) {
    return `<div class="section"><div class="sec-head"><div class="grow"><b>${esc(title)}</b><span>${esc(sub)}</span></div>
      <button type="button" class="switch" role="switch" aria-checked="${on}" data-section="${id}" aria-label="${esc(title)}" ${locked && !on ? 'disabled' : ''}></button></div>
      <div class="sec-body ${on ? '' : 'hidden'}" data-sec-body="${id}">${body}</div></div>`;
  }
  function hdrRow(k, val) {
    return `<div class="row hdr-row"><input class="input mono" value="${esc(k)}" placeholder="${esc(t('header'))}" aria-label="${esc(t('headerName'))}">
      <input class="input mono" value="${esc(val)}" placeholder="${esc(t('value'))}" aria-label="${esc(t('headerValue'))}">
      <button type="button" class="icon-btn danger" data-hdr-rm data-tip="${esc(t('remove'))}" data-tip-end aria-label="${esc(t('removeHeader'))}">${icon('trash')}</button></div>`;
  }

  async function preview(d) {
    const out = $('#f-preview-out', d);
    const btn = $('#f-preview', d);
    const url = $('#f-url', d).value.trim();
    if (!url) { $('#f-url', d).focus(); return; }
    btn.disabled = true;
    out.innerHTML = `<div class="preview"><span class="hint">${esc(t('readingFeed'))}</span></div>`;
    const headers = {};
    d.querySelectorAll('#hdrs .hdr-row').forEach((r) => {
      const [k, val] = r.querySelectorAll('input');
      if (k.value.trim()) headers[k.value.trim()] = val.value;
    });
    try {
      const p = await api('POST', '/api/preview', { url, headers });
      out.innerHTML = `<div class="preview">
        <div class="ptitle">${esc(p.title || t('untitled'))}<span class="chip">${esc(t('nItems', { n: p.count }))}</span>${p.hub ? `<span class="chip ok" data-tip="${esc(t('websubHubTip'))}" tabindex="0">${esc(t('websubHub'))}</span>` : ''}</div>
        <ol>${p.items.map((x) => `<li><span class="t" title="${esc(x.item.title)}">${esc(x.item.title || x.item.link || x.item.id)}</span>
          <span class="b" data-tip="${esc(t('sizeTip', { max: p.max_bytes }))}" data-tip-end tabindex="0">${(x.bytes / 1024).toFixed(1)} KB</span></li>`).join('')}</ol></div>`;
      if (!$('#f-name', d).value && p.title) $('#f-name', d).value = p.title;
    } catch (err) {
      out.innerHTML = `<div class="problems">${esc(err.problems ? err.problems.join(' ') : err.message)}</div>`;
    }
    btn.disabled = false;
  }

  // --- finding a feed ---
  // Two ways to pick a feed instead of typing it: read a web page for the
  // feeds it offers (and the RSSHub routes that match it), or browse the
  // RSSHub route catalog and fill in a route's parameters.
  function finderHtml(open) {
    const tabs = state.rsshub_url ? seg('find-mode', [['site', t('fromWebsite')], ['routes', t('rsshubRoutes')]], 'site') : '';
    return `<div class="finder ${open ? '' : 'hidden'}" id="finder">
      <div class="finder-head"><b>${esc(t('findFeed'))}</b>${tabs}</div>
      <div class="row"><input class="input" id="find-q" placeholder="${esc(t('websitePh'))}" autocomplete="off" spellcheck="false" enterkeyhint="search">
        <button type="button" class="btn" id="find-go">${icon('search')}<span class="hide-xs">${esc(t('find'))}</span></button></div>
      <div class="finder-out" id="find-out"><span class="hint">${esc(t('pickHint'))}</span></div>
    </div>`;
  }

  function wireFinder(d) {
    const box = $('#finder', d), q = $('#find-q', d), out = $('#find-out', d), go = $('#find-go', d);
    const toggle = $('#f-find', d);
    let mode = 'site', routes = [], seq = 0, debounce = null;
    toggle.onclick = () => {
      const open = box.classList.toggle('hidden') === false;
      toggle.setAttribute('aria-expanded', String(open));
      if (open) q.focus();
    };
    const modeSeg = d.querySelector('[data-seg="find-mode"]');
    if (modeSeg) {
      modeSeg.addEventListener('change', () => {
        mode = segValue(d, 'find-mode');
        q.value = '';
        q.placeholder = t(mode === 'site' ? 'websitePh' : 'routesPh');
        go.classList.toggle('hidden', mode === 'routes');
        out.innerHTML = `<span class="hint">${esc(t('pickHint'))}</span>`;
        if (mode === 'routes') searchRoutes();
        q.focus();
      });
    }
    q.onkeydown = (e) => {
      if (e.key === 'Enter') { e.preventDefault(); if (mode === 'site') discover(); else searchRoutes(); }
    };
    q.oninput = () => {
      if (mode !== 'routes') return;
      clearTimeout(debounce);
      debounce = setTimeout(searchRoutes, 250);
    };
    go.onclick = discover;

    const pick = (url, name) => {
      $('#f-url', d).value = url;
      if (name && !$('#f-name', d).value) $('#f-name', d).value = name;
      preview(d);
      $('#f-url', d).scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    };

    async function discover() {
      const url = q.value.trim();
      if (!url) { q.focus(); return; }
      const n = ++seq;
      go.disabled = true;
      out.innerHTML = `<span class="hint">${esc(t('finding'))}</span>`;
      try {
        const r = await api('POST', '/api/discover', { url });
        if (n !== seq) return;
        const kinds = { feed: ['kindFeed', 'ok'], link: ['kindLink', ''], rsshub: ['kindRsshub', 'accent'] };
        out.innerHTML = (r.feeds.length ? `<ul class="pick">${r.feeds.map((f, i) => `
          <li><button type="button" class="pick-row" data-i="${i}">
            <span class="pick-main"><span class="pick-title">${esc(f.title)}</span><span class="pick-sub mono">${esc(f.url)}</span></span>
            <span class="chip ${kinds[f.kind]?.[1] || ''}">${esc(t(kinds[f.kind]?.[0] || 'kindFeed'))}</span>
            <span class="pick-go">${icon('chevron')}</span></button></li>`).join('')}</ul>`
          : `<span class="hint">${esc(t('noneFound'))}</span>`)
          + (r.page_error ? `<div class="hint note">${esc(t('pageUnreadable', { error: r.page_error }))}</div>` : '')
          + (r.rsshub_error ? `<div class="hint note">${esc(t('rsshubUnavailable', { error: r.rsshub_error }))}</div>` : '');
        out.querySelectorAll('[data-i]').forEach((b) => (b.onclick = () => {
          const f = r.feeds[Number(b.dataset.i)];
          out.querySelectorAll('.pick-row').forEach((x) => x.classList.toggle('on', x === b));
          pick(f.url, f.title);
        }));
      } catch (err) {
        if (n === seq) out.innerHTML = `<div class="problems">${esc(err.problems ? err.problems.join(' ') : err.message)}</div>`;
      }
      go.disabled = false;
    }

    async function searchRoutes() {
      const n = ++seq;
      if (!routes.length) out.innerHTML = `<span class="hint">${esc(t('loadingRoutes'))}</span>`;
      try {
        const r = await api('GET', `/api/routes?q=${encodeURIComponent(q.value.trim())}&lang=${encodeURIComponent(lang)}`);
        if (n !== seq) return;
        routes = r.routes;
        if (!routes.length) { out.innerHTML = `<span class="hint">${esc(t('noRoutes'))}</span>`; return; }
        out.innerHTML = `<ul class="pick">${routes.map((rt, i) => `
          <li data-i="${i}"><button type="button" class="pick-row" aria-expanded="false">
            <span class="pick-main"><span class="pick-title">${esc(rt.site)} · ${esc(rt.name)}</span><span class="pick-sub mono">${esc(rt.path)}</span></span>
            ${rt.needs_config ? `<span class="chip warn" data-tip="${esc(t('needsConfigTip'))}" data-tip-end>${esc(t('needsConfig'))}</span>` : ''}
            <span class="pick-go">${icon('chevron')}</span></button></li>`).join('')}</ul>`;
        out.querySelectorAll('li[data-i] > .pick-row').forEach((b) => (b.onclick = () => openRoute(b.parentElement)));
      } catch (err) {
        if (n === seq) out.innerHTML = `<div class="problems">${esc(err.message)}</div>`;
      }
    }

    function openRoute(li) {
      const rt = routes[Number(li.dataset.i)];
      const was = li.querySelector('.route-form');
      out.querySelectorAll('.route-form').forEach((x) => x.remove());
      out.querySelectorAll('.pick-row').forEach((x) => { x.classList.remove('on'); x.setAttribute('aria-expanded', 'false'); });
      if (was) return;
      const row = li.querySelector('.pick-row');
      row.classList.add('on');
      row.setAttribute('aria-expanded', 'true');
      const form = document.createElement('div');
      form.className = 'route-form';
      form.innerHTML = `${rt.description ? `<p class="hint">${esc(rt.description)}</p>` : ''}
        ${(rt.params || []).map((p, i) => `<div class="field"><label for="rp-${i}">${esc(p.name)}${p.optional ? ` <span class="hint">(${esc(t('optional'))})</span>` : ''}</label>
          ${p.options?.length
            ? `<div class="seg" role="radiogroup" data-param="${i}">${[...(p.optional ? [['', t('any')]] : []), ...p.options.map((o) => [o.value, o.label || o.value])].map(([val, label]) =>
              `<button type="button" role="radio" data-v="${esc(val)}" aria-checked="${val === (p.default || (p.optional ? '' : p.options[0].value))}">${esc(label)}</button>`).join('')}</div>`
            : `<input class="input mono" id="rp-${i}" data-param="${i}" placeholder="${esc(p.default ? t('defaultIs', { value: p.default }) : exampleFor(rt, i) || p.name)}" autocomplete="off" spellcheck="false">`}
          ${p.description ? `<span class="hint">${esc(p.description)}</span>` : ''}</div>`).join('')}
        <div class="route-url mono" id="rp-url"></div>
        <div class="row end"><a class="icon-btn" href="${esc(rt.docs)}" target="_blank" rel="noopener" data-tip="${esc(t('docs'))}" aria-label="${esc(t('docs'))}">${icon('book')}</a>
          <button type="button" class="btn primary" id="rp-use">${esc(t('useRoute'))}</button></div>`;
      li.append(form);
      form.querySelectorAll('.seg[data-param]').forEach((g) => (g.onclick = (e) => {
        const b = e.target.closest('button');
        if (!b) return;
        g.querySelectorAll('button').forEach((x) => x.setAttribute('aria-checked', String(x === b)));
        update();
      }));
      form.querySelectorAll('input[data-param]').forEach((inp) => (inp.oninput = update));
      const values = () => (rt.params || []).map((p, i) => {
        const el = form.querySelector(`[data-param="${i}"]`);
        if (el.tagName === 'INPUT') return el.value.trim() || p.default || '';
        return el.querySelector('[aria-checked="true"]')?.dataset.v || '';
      });
      function build() {
        const vals = values();
        const missing = [];
        let i = 0, gap = false;
        // After an empty optional parameter, later values would land in its
        // place, so they are left out.
        const segs = rt.path.split('/').filter(Boolean).flatMap((s) => {
          if (!s.startsWith(':')) return gap ? [] : [s];
          const p = rt.params[i], v = vals[i];
          i++;
          if (!v || gap) {
            if (!p.optional) missing.push(p.name);
            else gap = true;
            return [];
          }
          return [encodeURIComponent(v)];
        });
        return { url: '/' + segs.join('/'), missing };
      }
      function update() {
        const { url, missing } = build();
        $('#rp-url', form).textContent = url;
        const use = $('#rp-use', form);
        use.disabled = missing.length > 0;
        use.dataset.tip = missing.length ? t('fillIn', { names: missing.join(', ') }) : '';
        if (!missing.length) delete use.dataset.tip;
      }
      $('#rp-use', form).onclick = () => {
        const { url, missing } = build();
        if (missing.length) return;
        pick(url, `${rt.site} · ${rt.name}`);
      };
      update();
      const first = form.querySelector('input[data-param]');
      if (first) first.focus();
    }
  }

  // exampleFor reads a parameter's value out of the route's example path.
  function exampleFor(rt, i) {
    if (!rt.example) return '';
    const segs = rt.path.split('/').filter(Boolean), ex = rt.example.split('/').filter(Boolean);
    let k = -1;
    for (let j = 0; j < segs.length; j++) {
      if (segs[j].startsWith(':')) k++;
      if (k === i) { try { return ex[j] ? decodeURIComponent(ex[j]) : ''; } catch { return ex[j]; } }
    }
    return '';
  }

  load();
})();
