/*
 * Mailschleuse single page UI.
 *
 * Everything the instance holds is reachable from this one surface: pick a
 * mailbox on the left, read what landed there in the middle, inspect it on the
 * right, and use "New message" to put a mail into any mailbox by hand.
 */

const api = (path, options) => fetch(`api${path}`, options);

const state = {
  config: null,
  mailboxes: [],
  mailbox: null,
  messages: [],
  total: 0,
  selectedId: null,
  detail: null,
  search: '',
  tab: 'preview',
};

const el = (id) => document.getElementById(id);
const dom = {
  version: el('version'),
  mailboxList: el('mailbox-list'),
  messageList: el('message-list'),
  detailPane: el('detail-pane'),
  listTitle: el('list-title'),
  listCount: el('list-count'),
  search: el('search'),
  toasts: el('toasts'),
  composeDialog: el('compose-dialog'),
  endpointsDialog: el('endpoints-dialog'),
  endpointsBody: el('endpoints-body'),
};

/* --- helpers -------------------------------------------------------------- */

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function formatBytes(bytes) {
  if (!bytes) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  const exp = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / Math.pow(1024, exp);
  return `${value >= 10 || exp === 0 ? Math.round(value) : value.toFixed(1)} ${units[exp]}`;
}

function formatTime(iso) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const today = new Date();
  const sameDay = date.toDateString() === today.toDateString();
  return sameDay
    ? date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : date.toLocaleDateString([], { day: '2-digit', month: 'short' });
}

function formatAddresses(list) {
  if (!list || !list.length) return '';
  return list.map((a) => (a.name ? `${a.name} <${a.address}>` : a.address)).join(', ');
}

function toast(message, kind) {
  const node = document.createElement('div');
  node.className = `toast${kind === 'error' ? ' error' : ''}`;
  node.textContent = message;
  dom.toasts.appendChild(node);
  setTimeout(() => node.remove(), kind === 'error' ? 6000 : 3000);
}

async function request(path, options) {
  const response = await api(path, options);
  const text = await response.text();
  const payload = text ? JSON.parse(text) : {};
  if (!response.ok) throw new Error(payload.error || `request failed with ${response.status}`);
  return payload;
}

/* --- mailboxes ------------------------------------------------------------ */

// Mailboxes named "in"/"out" get a direction colour; everything else stays neutral.
function directionOf(name) {
  if (/^(in|inbox|incoming|received)/.test(name)) return 'in';
  if (/^(out|outbox|outgoing|sent)/.test(name)) return 'out';
  return '';
}

function renderMailboxes() {
  dom.mailboxList.innerHTML = state.mailboxes.map((box) => `
    <button class="mailbox" type="button" data-mailbox="${escapeHTML(box.name)}"
            data-direction="${directionOf(box.name)}"
            aria-current="${box.name === state.mailbox}">
      <span class="dot"></span>
      <span class="name">${escapeHTML(box.name)}</span>
      ${box.unread ? `<span class="unread">${box.unread}</span>` : `<span class="count">${box.total}</span>`}
    </button>
  `).join('');

  dom.mailboxList.querySelectorAll('[data-mailbox]').forEach((button) => {
    button.addEventListener('click', () => selectMailbox(button.dataset.mailbox));
  });
}

async function refreshMailboxes() {
  const payload = await request('/mailboxes');
  state.mailboxes = payload.mailboxes;
  renderMailboxes();
  const current = state.mailboxes.find((b) => b.name === state.mailbox);
  dom.listCount.textContent = current
    ? `${current.total} message${current.total === 1 ? '' : 's'} · ${formatBytes(current.bytes)}`
    : '';
}

function selectMailbox(name) {
  state.mailbox = name;
  state.selectedId = null;
  state.detail = null;
  localStorage.setItem('mailschleuse.mailbox', name);
  location.hash = `#${encodeURIComponent(name)}`;
  dom.listTitle.textContent = name;
  document.body.classList.remove('detail-open');
  renderMailboxes();
  renderDetail();
  loadMessages();
}

/* --- message list --------------------------------------------------------- */

async function loadMessages() {
  if (!state.mailbox) return;
  const params = new URLSearchParams({ mailbox: state.mailbox, limit: '200' });
  if (state.search) params.set('search', state.search);
  const payload = await request(`/messages?${params}`);
  state.messages = payload.messages;
  state.total = payload.total;
  renderMessages();
  refreshMailboxes();
}

function renderMessages() {
  if (!state.messages.length) {
    dom.messageList.innerHTML = `
      <li class="empty">
        <h3>${state.search ? 'Nothing matches your search' : 'No messages yet'}</h3>
        <p>${state.search
          ? 'Try a different term, or clear the search field.'
          : 'Mail delivered over SMTP shows up here instantly. Use "New message" to place one into this mailbox by hand.'}</p>
      </li>`;
    return;
  }

  dom.messageList.innerHTML = state.messages.map((msg) => `
    <li>
      <button class="message-item${msg.read ? '' : ' unread'}" type="button"
              data-id="${escapeHTML(msg.id)}" aria-current="${msg.id === state.selectedId}">
        <div class="row">
          <span class="from">${escapeHTML(formatAddresses(msg.from) || msg.envelope.from || 'unknown sender')}</span>
          <span class="time">${formatTime(msg.receivedAt)}</span>
        </div>
        <div class="subject">${escapeHTML(msg.subject || '(no subject)')}</div>
        <div class="row">
          <span class="snippet">${escapeHTML(msg.snippet || '')}</span>
        </div>
        <div class="row" style="margin-top:6px;gap:6px">
          <span class="tag tag-${escapeHTML(msg.source)}">${escapeHTML(msg.source)}</span>
          ${msg.hasHtml ? '<span class="tag">html</span>' : ''}
          ${msg.attachments ? `<span class="tag">${msg.attachments} file${msg.attachments === 1 ? '' : 's'}</span>` : ''}
          <span class="spacer" style="flex:1"></span>
          <span class="time">${formatBytes(msg.size)}</span>
        </div>
      </button>
    </li>
  `).join('');

  dom.messageList.querySelectorAll('[data-id]').forEach((button) => {
    button.addEventListener('click', () => selectMessage(button.dataset.id));
  });
}

/* --- detail --------------------------------------------------------------- */

async function selectMessage(id) {
  state.selectedId = id;
  state.tab = 'preview';
  document.body.classList.add('detail-open');
  renderMessages();
  state.detail = await request(`/messages/${encodeURIComponent(id)}`);
  renderDetail();
  if (!state.detail.read) {
    await request(`/messages/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ read: true }),
    });
  }
}

function renderDetail() {
  const detail = state.detail;
  if (!detail) {
    dom.detailPane.innerHTML = `
      <div class="empty">
        <h3>Nothing selected</h3>
        <p>Pick a message to see its rendered body, its headers and the raw source.</p>
      </div>`;
    return;
  }

  const msg = detail.message;
  const rows = [
    ['From', formatAddresses(msg.from) || detail.envelope.from],
    ['To', formatAddresses(msg.to) || (detail.envelope.to || []).join(', ')],
    ['Cc', formatAddresses(msg.cc)],
    ['Reply-To', formatAddresses(msg.replyTo)],
    ['Date', msg.date && !msg.date.startsWith('0001') ? new Date(msg.date).toLocaleString() : new Date(detail.receivedAt).toLocaleString()],
    ['Envelope', `${detail.envelope.from || '<>'} → ${(detail.envelope.to || []).join(', ') || '(none)'}`],
  ].filter(([, value]) => value);

  const tabs = [
    ['preview', 'Preview'],
    ['text', 'Plain text'],
    ['headers', `Headers (${msg.headers.length})`],
    ['raw', 'Raw source'],
    ['attachments', `Attachments (${msg.attachments.length})`],
  ];

  dom.detailPane.innerHTML = `
    <div class="detail-head">
      <h2>${escapeHTML(msg.subject || '(no subject)')}</h2>
      <dl class="addr-grid">
        ${rows.map(([label, value]) => `<dt>${label}</dt><dd>${escapeHTML(value)}</dd>`).join('')}
      </dl>
      <div class="detail-actions">
        <button class="btn back-btn" type="button" data-action="back">← Back</button>
        <a class="btn" href="api/messages/${encodeURIComponent(detail.id)}/raw?download=true" download>Download .eml</a>
        <button class="btn" type="button" data-action="unread">Mark unread</button>
        <button class="btn btn-danger" type="button" data-action="delete">Delete</button>
        <span class="spacer" style="flex:1"></span>
        <span class="tag tag-${escapeHTML(detail.source)}">${escapeHTML(detail.source)} · ${escapeHTML(detail.mailbox)}</span>
      </div>
    </div>
    <div class="tabs" role="tablist">
      ${tabs.map(([key, label]) => `
        <button class="tab" role="tab" type="button" data-tab="${key}"
                aria-selected="${state.tab === key}">${label}</button>`).join('')}
    </div>
    <div class="tab-body" id="tab-body"></div>
  `;

  dom.detailPane.querySelectorAll('[data-tab]').forEach((button) => {
    button.addEventListener('click', () => {
      state.tab = button.dataset.tab;
      renderDetail();
    });
  });
  dom.detailPane.querySelectorAll('[data-action]').forEach((button) => {
    button.addEventListener('click', () => detailAction(button.dataset.action));
  });

  renderTabBody(detail, msg);
}

function renderTabBody(detail, msg) {
  const body = el('tab-body');
  switch (state.tab) {
    case 'preview':
      if (msg.html) {
        body.innerHTML = `<iframe sandbox="" title="HTML preview"
          src="api/messages/${encodeURIComponent(detail.id)}/html"></iframe>`;
      } else {
        body.innerHTML = `<pre>${escapeHTML(msg.text || '(empty message)')}</pre>`;
      }
      break;
    case 'text':
      body.innerHTML = `<pre>${escapeHTML(msg.text || '(this message has no plain text part)')}</pre>`;
      break;
    case 'headers':
      body.innerHTML = `<table class="headers-table"><tbody>${msg.headers.map((h) => `
        <tr><td>${escapeHTML(h.name)}</td><td>${escapeHTML(h.value)}</td></tr>`).join('')}</tbody></table>`;
      break;
    case 'raw':
      body.innerHTML = '<pre>loading...</pre>';
      api(`/messages/${encodeURIComponent(detail.id)}/raw`)
        .then((r) => r.text())
        .then((text) => { body.innerHTML = `<pre>${escapeHTML(text)}</pre>`; });
      break;
    case 'attachments':
      body.innerHTML = msg.attachments.length
        ? `<ul class="attachment-list">${msg.attachments.map((a) => `
            <li class="attachment">
              <span class="name">${escapeHTML(a.filename)}</span>
              <span class="tag">${escapeHTML(a.contentType)}</span>
              <span class="size">${formatBytes(a.size)}</span>
              <a class="btn" download
                 href="api/messages/${encodeURIComponent(detail.id)}/attachments/${encodeURIComponent(a.partId)}?download=true">Download</a>
            </li>`).join('')}</ul>`
        : '<div class="empty"><p>This message has no attachments.</p></div>';
      break;
  }
}

async function detailAction(action) {
  const detail = state.detail;
  if (!detail) return;
  try {
    if (action === 'back') {
      document.body.classList.remove('detail-open');
      return;
    }
    if (action === 'delete') {
      await request(`/messages/${encodeURIComponent(detail.id)}`, { method: 'DELETE' });
      state.selectedId = null;
      state.detail = null;
      document.body.classList.remove('detail-open');
      renderDetail();
      await loadMessages();
      toast('Message deleted');
      return;
    }
    if (action === 'unread') {
      await request(`/messages/${encodeURIComponent(detail.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ read: false }),
      });
      await loadMessages();
      toast('Marked as unread');
    }
  } catch (error) {
    toast(error.message, 'error');
  }
}

/* --- compose -------------------------------------------------------------- */

function parseHeaderLines(raw) {
  const headers = {};
  raw.split('\n').forEach((line) => {
    const index = line.indexOf(':');
    if (index > 0) headers[line.slice(0, index).trim()] = line.slice(index + 1).trim();
  });
  return headers;
}

function readFileAsBase64(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error(`cannot read ${file.name}`));
    reader.onload = () => resolve({
      filename: file.name,
      contentType: file.type || 'application/octet-stream',
      content: String(reader.result).split(',')[1] || '',
    });
    reader.readAsDataURL(file);
  });
}

function openCompose() {
  const select = el('c-mailbox');
  select.innerHTML = state.mailboxes
    .map((box) => `<option value="${escapeHTML(box.name)}"${box.name === state.mailbox ? ' selected' : ''}>${escapeHTML(box.name)}</option>`)
    .join('');
  dom.composeDialog.showModal();
}

async function submitCompose(event) {
  event.preventDefault();
  const submit = el('c-submit');
  submit.disabled = true;
  try {
    const files = Array.from(el('c-files').files || []);
    const payload = {
      mailbox: el('c-mailbox').value,
      from: el('c-from').value,
      to: el('c-to').value.split(',').map((s) => s.trim()).filter(Boolean),
      cc: el('c-cc').value.split(',').map((s) => s.trim()).filter(Boolean),
      subject: el('c-subject').value,
      text: el('c-text').value,
      html: el('c-html').value,
      headers: parseHeaderLines(el('c-headers').value),
      attachments: await Promise.all(files.map(readFileAsBase64)),
    };
    const result = await request('/messages', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    dom.composeDialog.close();
    toast(`Delivered into ${result.mailbox}`);
    if (state.mailbox !== result.mailbox) selectMailbox(result.mailbox);
    else await loadMessages();
  } catch (error) {
    toast(error.message, 'error');
  } finally {
    submit.disabled = false;
  }
}

/* --- endpoints ------------------------------------------------------------ */

function openEndpoints() {
  const endpoints = state.config?.endpoints || [];
  dom.endpointsBody.innerHTML = endpoints.length ? endpoints.map((endpoint) => `
    <div class="endpoint">
      <h3><span class="tag">${escapeHTML(endpoint.protocol)}</span> ${escapeHTML(endpoint.note || '')}</h3>
      <dl>
        <dt>Host</dt><dd>${escapeHTML(endpoint.host)}</dd>
        <dt>Port</dt><dd>${escapeHTML(endpoint.port)}</dd>
        <dt>Mailbox</dt><dd>${escapeHTML(endpoint.mailbox)}</dd>
        <dt>Username</dt><dd>${escapeHTML(endpoint.username || 'any value accepted')}</dd>
        <dt>Password</dt><dd>${endpoint.username ? 'as configured in your .env' : 'any value accepted'}</dd>
        <dt>Auth</dt><dd>${endpoint.authRequired ? 'required' : 'optional'}</dd>
        <dt>Encryption</dt><dd>${escapeHTML(endpoint.tls)}</dd>
      </dl>
    </div>`).join('') : '<p>No protocol listeners are enabled.</p>';
  dom.endpointsDialog.showModal();
}

/* --- live updates --------------------------------------------------------- */

function connectEvents() {
  const source = new EventSource('api/events');
  const refresh = (event) => {
    const payload = JSON.parse(event.data);
    if (payload.mailbox === state.mailbox || payload.type === 'mailbox.cleared') {
      loadMessages().catch(() => {});
    } else {
      refreshMailboxes().catch(() => {});
    }
    if (payload.type === 'message.deleted' && payload.id === state.selectedId) {
      state.selectedId = null;
      state.detail = null;
      renderDetail();
    }
  };
  ['message.new', 'message.updated', 'message.deleted', 'mailbox.cleared']
    .forEach((type) => source.addEventListener(type, refresh));
  source.onerror = () => { /* EventSource reconnects on its own */ };
}

/* --- boot ----------------------------------------------------------------- */

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem('mailschleuse.theme', theme);
}

function debounce(fn, delay) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), delay);
  };
}

async function boot() {
  applyTheme(localStorage.getItem('mailschleuse.theme')
    || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));

  el('theme-btn').addEventListener('click', () => {
    applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  });
  el('compose-btn').addEventListener('click', openCompose);
  el('endpoints-btn').addEventListener('click', openEndpoints);
  el('refresh-btn').addEventListener('click', () => loadMessages().catch((e) => toast(e.message, 'error')));
  el('compose-form').addEventListener('submit', submitCompose);
  document.querySelectorAll('[data-close]').forEach((button) => {
    button.addEventListener('click', () => button.closest('dialog').close());
  });

  el('mark-all-btn').addEventListener('click', async () => {
    try {
      const result = await request(`/mailboxes/${encodeURIComponent(state.mailbox)}/read`, { method: 'POST' });
      toast(`${result.updated} message(s) marked read`);
      await loadMessages();
    } catch (error) { toast(error.message, 'error'); }
  });

  el('clear-btn').addEventListener('click', async () => {
    if (!confirm(`Delete every message in "${state.mailbox}"?`)) return;
    try {
      const result = await request(`/mailboxes/${encodeURIComponent(state.mailbox)}/messages`, { method: 'DELETE' });
      state.selectedId = null;
      state.detail = null;
      renderDetail();
      toast(`${result.deleted} message(s) deleted`);
      await loadMessages();
    } catch (error) { toast(error.message, 'error'); }
  });

  dom.search.addEventListener('input', debounce(() => {
    state.search = dom.search.value.trim();
    loadMessages().catch((error) => toast(error.message, 'error'));
  }, 200));

  document.addEventListener('keydown', (event) => {
    if (event.key === '/' && document.activeElement !== dom.search) {
      event.preventDefault();
      dom.search.focus();
    }
  });

  try {
    state.config = await request('/config');
    state.mailboxes = state.config.mailboxes;
    const label = /^\d/.test(state.config.version) ? `v${state.config.version}` : state.config.version;
    dom.version.textContent = `${label} · ${state.config.hostname}`;
    if (state.config.readOnly) {
      document.querySelectorAll('#compose-btn, #clear-btn, #mark-all-btn')
        .forEach((button) => { button.disabled = true; });
    }
    const wanted = decodeURIComponent(location.hash.slice(1))
      || localStorage.getItem('mailschleuse.mailbox');
    const initial = state.mailboxes.find((box) => box.name === wanted) || state.mailboxes[0];
    if (initial) selectMailbox(initial.name);
    connectEvents();
  } catch (error) {
    toast(`Cannot reach the API: ${error.message}`, 'error');
  }
}

boot();
