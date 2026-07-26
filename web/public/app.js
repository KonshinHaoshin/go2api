// app.js — go2api 管理控制台
//
// 纯原生 JavaScript,无需构建步骤。基于 hash 路由的单页应用,
// 与 Node 代理 /api/* 通信(代理将请求转发到上游 go2api)。
// 启动时根据 localStorage 中是否存在 JWT 决定显示登录页还是主界面。

'use strict';

// ----- Token 存储 ---------------------------------------------------------

const TOKEN_KEY = 'go2api_token';
const USER_KEY = 'go2api_user';

const tokenStore = {
  get() { return localStorage.getItem(TOKEN_KEY); },
  set(token, user) {
    localStorage.setItem(TOKEN_KEY, token);
    if (user) localStorage.setItem(USER_KEY, user);
  },
  clear() {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(USER_KEY);
  },
};

// ----- API 客户端 ----------------------------------------------------------

const api = {
  async get(path) {
    const res = await fetch('/api' + path, { headers: this._headers() });
    return this._handle(res);
  },
  async post(path, body) {
    const res = await fetch('/api' + path, {
      method: 'POST',
      headers: this._headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify(body || {}),
    });
    return this._handle(res);
  },
  async patch(path, body) {
    const res = await fetch('/api' + path, {
      method: 'PATCH',
      headers: this._headers({ 'Content-Type': 'application/json' }),
      body: JSON.stringify(body || {}),
    });
    return this._handle(res);
  },
  async delete(path) {
    const res = await fetch('/api' + path, { method: 'DELETE', headers: this._headers() });
    return this._handle(res);
  },
  _headers(extra = {}) {
    const h = { Accept: 'application/json', ...extra };
    const t = tokenStore.get();
    if (t) h['Authorization'] = `Bearer ${t}`;
    return h;
  },
  async _handle(res) {
    if (res.status === 401) {
      tokenStore.clear();
      showLogin();
      throw new Error('未登录');
    }
    const text = await res.text();
    let data;
    try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
    if (!res.ok) {
      // 后端返回的 JSON 里有 error.message,优先用它;否则根据 HTTP 状态码
      // 给出中文描述(避免直接显示英文 statusText)。
      const msg = (data && data.error && data.error.message)
        || httpStatusText(res.status)
        || '请求失败';
      const err = new Error(msg);
      err.status = res.status;
      err.data = data;
      throw err;
    }
    return data;
  },
};

// ----- DOM 辅助 ------------------------------------------------------------

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, children = []) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'style') node.setAttribute('style', v);
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null) node.setAttribute(k, v);
  }
  for (const c of [].concat(children)) {
    if (c == null) continue;
    node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  }
  return node;
};

function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

// 把 HTTP 状态码映射为中文描述,避免前端显示英文的 res.statusText
function httpStatusText(status) {
  const map = {
    400: '请求参数错误',
    401: '未授权',
    403: '禁止访问',
    404: '接口不存在',
    405: '方法不被允许',
    408: '请求超时',
    409: '冲突',
    413: '请求体过大',
    429: '请求过于频繁',
    500: '服务器内部错误',
    502: '上游服务不可用',
    503: '服务暂不可用',
    504: '上游超时',
  };
  return map[status] || `请求失败 (HTTP ${status})`;
}

// ----- 提示 (toast) -------------------------------------------------------

function toast(message, kind = 'info', ttlMs = 3200) {
  const stack = $('#toast-stack');
  const t = el('div', { class: `toast ${kind}`, text: message });
  stack.appendChild(t);
  setTimeout(() => {
    t.style.transition = 'opacity 0.25s';
    t.style.opacity = '0';
    setTimeout(() => t.remove(), 260);
  }, ttlMs);
}

// ----- 弹窗 ----------------------------------------------------------------

const modal = {
  show({ title, body, confirmLabel = '保存', onConfirm }) {
    setModalContent(title, body, confirmLabel);
    const close = () => { $('#modal').hidden = true; $('#modal-confirm').onclick = null; };
    $('#modal-close').onclick = close;
    $('#modal-cancel').onclick = close;
    $('#modal-confirm').onclick = async () => {
      try {
        await onConfirm();
        // 不再自动 close:由调用方决定是否关闭,以便支持"创建后展示 token"等
        // 需要保留弹窗继续显示的场景。
      } catch (e) {
        toast(e.message || '操作失败', 'err');
      }
    };
    return close;
  },

  close() {
    $('#modal').hidden = true;
    $('#modal-confirm').onclick = null;
  },
};

// 直接替换弹窗内容(不重新走 confirm 流程),用于"创建后展示 token"等场景
function setModalContent(title, body, confirmLabel) {
  $('#modal-title').textContent = title;
  clear($('#modal-body'));
  $('#modal-body').appendChild(body);
  $('#modal-confirm').textContent = confirmLabel;
  $('#modal').hidden = false;
}

// ----- 登录 / 退出 ---------------------------------------------------------

function showLogin() {
  $('#login-shell').hidden = false;
  $('#app-shell').hidden = true;
  setTimeout(() => $('#login-username').focus(), 50);
}

function showApp(username) {
  $('#login-shell').hidden = true;
  $('#app-shell').hidden = false;
  if (username) {
    $('#user-tag').textContent = `已登录:${username}`;
  }
}

async function login(username, password) {
  const res = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const msg = (data && data.error && data.error.message) || '登录失败';
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  tokenStore.set(data.token, username);
  return data;
}

async function logout() {
  try { await fetch('/api/auth/logout', { method: 'POST' }); } catch {}
  tokenStore.clear();
  showLogin();
  $('#login-username').value = '';
  $('#login-password').value = '';
  $('#login-error').hidden = true;
}

async function bootstrapAuth() {
  const token = tokenStore.get();
  if (!token) { showLogin(); return; }
  try {
    const me = await api.get('/auth/me');
    showApp(me.username || localStorage.getItem(USER_KEY) || 'admin');
  } catch {
    showLogin();
  }
}

// 登录表单提交
$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const username = $('#login-username').value.trim();
  const password = $('#login-password').value;
  const errBox = $('#login-error');
  errBox.hidden = true;
  const btn = $('#login-form button[type=submit]');
  btn.disabled = true;
  btn.textContent = '登录中…';
  try {
    await login(username, password);
    await bootstrapAuth();
    route();
  } catch (e) {
    errBox.textContent = e.message || '登录失败';
    errBox.hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = '登 录';
  }
});

$('#logout-btn').addEventListener('click', logout);

// ----- 健康检查 -----------------------------------------------------------

async function pingHealth() {
  const dot = $('#health-dot');
  const text = $('#health-text');
  try {
    const res = await fetch('/api/healthz', { headers: tokenStore.get() ? { Authorization: `Bearer ${tokenStore.get()}` } : {} });
    if (res.ok) { dot.className = 'status-dot ok'; text.textContent = '健康'; }
    else { dot.className = 'status-dot err'; text.textContent = `上游 ${res.status}`; }
  } catch {
    dot.className = 'status-dot err'; text.textContent = '已断开';
  }
}

// ----- 格式化辅助 ---------------------------------------------------------

function fmtNumber(n) {
  if (n == null) return '—';
  if (Math.abs(n) >= 1_000_000) return (n / 1_000_000).toFixed(2) + 'M';
  if (Math.abs(n) >= 1_000) return (n / 1_000).toFixed(1) + 'k';
  return Number(n).toLocaleString();
}

function fmtLatency(ms) {
  if (ms == null || isNaN(ms)) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}

function fmtTime(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime())) return '—';
  const now = Date.now();
  const diff = d.getTime() - now;
  if (diff > 0 && diff < 24 * 3600 * 1000) {
    const mins = Math.round(diff / 60000);
    return `${mins} 分钟后`;
  }
  return d.toLocaleString('zh-CN');
}

function fmtQuota(quota) {
  if (!quota || (quota.remaining === 0 && quota.limit === 0)) return '—';
  return `${fmtNumber(quota.remaining)} / ${fmtNumber(quota.limit)}`;
}

function fmtUSD(n) {
  if (n == null || n === 0) return '$0.00';
  if (n < 0.01) return '<$0.01';
  return '$' + Number(n).toFixed(2);
}

function fmtPct(used, limit) {
  if (!limit || limit <= 0) return '0%';
  const pct = (used / limit) * 100;
  if (pct < 0.1) return '0%';
  return pct.toFixed(1) + '%';
}

// 额度进度条:used/limit,颜色随使用率变化
function usageBar(label, used, limit) {
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  const kind = pct >= 90 ? 'crit' : pct >= 70 ? 'warn' : 'ok';
  const bar = el('div', { class: 'bar-track' }, [
    el('div', { class: `bar-fill bar-${kind}`, style: `width:${pct}%` }),
  ]);
  return el('div', { class: 'usage-row' }, [
    el('span', { class: 'usage-label', text: label }),
    bar,
    el('span', { class: 'usage-val mono', text: `${fmtUSD(used)} / ${fmtUSD(limit)}` }),
  ]);
}

// 渲染单个 key 的额度消耗卡片
function renderKeyUsageCard(k) {
  const u = k.usage || {};
  const monthlyLimit = u.monthly_limit || 60;
  const card = el('div', { class: 'card key-usage-card' });
  card.appendChild(el('div', { class: 'key-usage-header' }, [
    el('span', { class: 'mono', text: k.label }),
    el('span', { class: 'muted', style: 'font-size:11px', text: `${fmtNumber(u.requests || 0)} 次请求` }),
  ]));
  card.appendChild(usageBar('5 小时', u.five_hour || 0, 12));
  card.appendChild(usageBar('每周', u.weekly || 0, 30));
  card.appendChild(usageBar('每月', u.monthly || 0, monthlyLimit));
  if (u.last_used_at && u.last_used_at !== '0001-01-01T00:00:00Z') {
    card.appendChild(el('div', { class: 'muted', style: 'font-size:11px;text-align:right', text: '最近: ' + fmtTime(u.last_used_at) }));
  }
  return card;
}

// ----- 视图 ----------------------------------------------------------------

const views = {};

views.dashboard = async () => {
  clear($('#content'));
  const root = el('div', { class: 'view' });

  let stats, keysData;
  try {
    [stats, keysData] = await Promise.all([api.get('/admin/stats'), api.get('/admin/keys')]);
  } catch (e) {
    root.appendChild(el('div', { class: 'card', text: '加载失败:' + e.message }));
    $('#content').appendChild(root);
    return;
  }

  const keys = keysData.keys || [];
  const active = keys.filter((k) => !k.disabled).length;
  const disabled = keys.filter((k) => k.disabled).length;
  const inCooldown = keys.filter((k) => {
    return k.cooldown_until && new Date(k.cooldown_until).getTime() > Date.now();
  }).length;

  const total = stats.Last24hTotal || 0;
  const hits = stats.Last24hCacheHits || 0;
  const hitRate = total > 0 ? ((hits / total) * 100).toFixed(1) + '%' : '—';

  const kpis = el('div', { class: 'grid-4' }, [
    statCard('活跃 Go Key', `${active}`, `共 ${keys.length} 个 · ${disabled} 个已停用`),
    statCard('冷却中', `${inCooldown}`, inCooldown > 0 ? '自动恢复中' : '全部正常'),
    statCard('24h 请求', fmtNumber(total), `${fmtNumber(hits)} 次命中缓存`),
    statCard('命中率', hitRate, `平均延迟 ${fmtLatency(stats.Last24hAvgLatencyMs)}`),
  ]);
  root.appendChild(kpis);

  const cacheCard = el('div', { class: 'card section' });
  cacheCard.appendChild(el('h3', { text: '缓存' }));
  cacheCard.appendChild(el('div', { class: 'value', text: fmtNumber(stats.CacheEntries) + ' 条' }));
  cacheCard.appendChild(el('div', { class: 'delta', text: `累计 ${fmtNumber(stats.CacheTotalHits)} 次命中` }));
  root.appendChild(cacheCard);

  // 额度消耗:每个 key 的 5h / 每周 / 每月 USD 用量
  // (已注释:上游 OpenCode Go API 不返回真实额度,本地估算值参考意义有限)
  // if (keys.length > 0) {
  //   const usageSection = el('div', { class: 'section' });
  //   usageSection.appendChild(el('div', { class: 'section-header' }, [
  //     el('div', { class: 'section-title', text: 'Go Key 额度消耗' }),
  //     el('span', { class: 'muted', style: 'font-size:12px', text: '基于请求 token 用量 × 模型单价本地估算' }),
  //   ]));
  //   const usageGrid = el('div', { class: 'usage-grid' });
  //   for (const k of keys) {
  //     usageGrid.appendChild(renderKeyUsageCard(k));
  //   }
  //   usageSection.appendChild(usageGrid);
  //   root.appendChild(usageSection);
  // }

  // 当没有任何 key 时,显示快速开始指引,而不是直接催用户加 key
  if (keys.length === 0) {
    root.appendChild(renderQuickStart());
  } else {
    const recent = el('div', { class: 'section' });
    recent.appendChild(el('div', { class: 'section-header' }, [
      el('div', { class: 'section-title', text: 'Go Key 概览' }),
      el('a', { class: 'btn btn-sm btn-ghost', href: '#/keys', text: '前往管理 →' }),
    ]));
    recent.appendChild(renderKeysTable(keys.slice(0, 6), { compact: true }));
    root.appendChild(recent);
  }

  $('#content').appendChild(root);
};

// 快速开始指引:首次使用、未配置任何 key 时显示
function renderQuickStart() {
  const section = el('div', { class: 'section' });
  section.appendChild(el('div', { class: 'section-header' }, [
    el('div', { class: 'section-title', text: '快速开始' }),
  ]));

  const card = el('div', { class: 'card quickstart' });

  const intro = el('p', { class: 'muted', text: 'go2api 把多个 OpenCode Go 订阅聚合成一个统一的 API 入口。完成下面 3 步即可对外提供 OpenAI / Anthropic 兼容服务:' });
  card.appendChild(intro);

  const steps = el('ol', { class: 'quickstart-steps' });
  steps.appendChild(el('li', {}, [
    el('strong', { text: '准备 OpenCode Go 订阅密钥' }),
    el('span', { class: 'muted', text: '在 opencode.ai 订阅 Go,获取形如 sk-go-xxxxxx 的密钥。' }),
  ]));
  steps.appendChild(el('li', {}, [
    el('strong', { text: '把密钥添加到本服务' }),
    el('span', { class: 'muted', text: '前往左侧「Go Key」页面 → 点击右上角"+ 新增 Go Key"添加你的第一个订阅。' }),
  ]));
  steps.appendChild(el('li', {}, [
    el('strong', { text: '调用接口' }),
    el('span', { class: 'muted', text: '客户端用 Bearer Token 调用本服务的 /v1/chat/completions 或 /v1/messages。' }),
  ]));
  card.appendChild(steps);

  const actions = el('div', { class: 'quickstart-actions' }, [
    el('a', { class: 'btn btn-primary', href: '#/keys', text: '前往 Go Key 页面' }),
    el('a', { class: 'btn btn-ghost', href: '#/models', text: '查看支持的模型' }),
  ]);
  card.appendChild(actions);

  section.appendChild(card);
  return section;
}

function statCard(label, value, sub) {
  return el('div', { class: 'card' }, [
    el('h3', { text: label }),
    el('div', { class: 'value', text: value }),
    el('div', { class: 'delta', text: sub }),
  ]);
}

views.keys = async () => {
  clear($('#content'));
  const root = el('div', { class: 'view' });

  const header = el('div', { class: 'section-header' }, [
    el('div', {}, [
      el('div', { class: 'section-title', text: 'OpenCode Go Key 列表' }),
      el('div', { class: 'section-sub', text: '管理多个 OpenCode Go 订阅密钥,请求会按策略从中选取使用。' }),
    ]),
    el('div', { class: 'row-actions' }, [
      el('button', {
        class: 'btn btn-sm btn-ghost',
        text: '刷新',
        onclick: () => views.keys(),
      }),
      el('button', {
        class: 'btn btn-primary',
        onclick: () => openAddKeyModal(root),
        text: '+ 新增 Go Key',
      }),
    ]),
  ]);
  root.appendChild(header);

  try {
    const data = await api.get('/admin/keys');
    root.appendChild(renderKeysTable(data.keys || [], { compact: false, onChange: () => views.keys() }));
  } catch (e) {
    root.appendChild(el('div', { class: 'card', text: '加载失败:' + e.message }));
  }

  $('#content').appendChild(root);
};

function renderKeysTable(keys, opts = {}) {
  const wrap = el('div', { class: 'table-wrap' });
  if (keys.length === 0) {
    wrap.appendChild(el('div', { class: 'empty', text: '暂无 Go Key。可以点击右上角"+ 新增 Go Key"添加一个。' }));
    return wrap;
  }

  const table = el('table');
  const thead = el('thead', {}, el('tr', {}, [
    el('th', { text: '名称' }),
    el('th', { text: 'Go Key' }),
    el('th', { text: '权重' }),
    el('th', { text: '状态' }),
    // (已注释:上游不返回真实额度,本地估算参考意义有限)
    // el('th', { text: '额度消耗 (5h / 周 / 月)' }),
    el('th', { text: '最近错误' }),
    el('th', { text: '', class: 'right' }),
  ]));
  table.appendChild(thead);

  const tbody = el('tbody');
  for (const k of keys) {
    const cooldownActive = k.cooldown_until && new Date(k.cooldown_until).getTime() > Date.now();
    const status = k.disabled ? '已停用' : cooldownActive ? '冷却中' : '活跃';
    const statusKind = k.disabled ? 'err' : cooldownActive ? 'warn' : 'ok';

    const tr = el('tr', {}, [
      el('td', {}, [
        el('div', { class: 'mono', text: k.label }),
        el('div', { class: 'muted', style: 'font-size:11px', text: k.id }),
      ]),
      el('td', { class: 'mono', text: k.api_key_masked || '—' }),
      el('td', { class: 'mono', text: String(k.weight) }),
      el('td', {}, pill(status, statusKind)),
      // el('td', {}, renderUsageCell(k.usage)),
      el('td', { class: 'muted', style: 'max-width:240px;overflow:hidden;text-overflow:ellipsis', text: k.last_error || '' }),
      el('td', { class: 'right' }, rowActions(k, opts)),
    ]);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  return wrap;
}

// 在 keys 表格里渲染紧凑的额度消耗单元格:三行迷你进度条
function renderUsageCell(u) {
  if (!u) return el('span', { class: 'muted', text: '—' });
  const ml = u.monthly_limit || 60;
  const cell = el('div', { class: 'usage-cell' });
  cell.appendChild(miniBar('5h', u.five_hour || 0, 12));
  cell.appendChild(miniBar('周', u.weekly || 0, 30));
  cell.appendChild(miniBar('月', u.monthly || 0, ml));
  if (u.last_used_at && u.last_used_at !== '0001-01-01T00:00:00Z') {
    cell.appendChild(el('div', { class: 'muted', style: 'font-size:10px;margin-top:2px', text: '最近 ' + fmtTime(u.last_used_at) }));
  }
  return cell;
}

// 紧凑的单行进度条:"5h $0.05/$12.00 [====----]"
function miniBar(label, used, limit) {
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  const kind = pct >= 90 ? 'crit' : pct >= 70 ? 'warn' : 'ok';
  return el('div', { class: 'mini-usage' }, [
    el('span', { class: 'mini-label', text: label }),
    el('div', { class: 'bar-track mini-bar' }, [
      el('div', { class: `bar-fill bar-${kind}`, style: `width:${pct}%` }),
    ]),
    el('span', { class: 'mono mini-val', text: `${fmtUSD(used)}/${fmtUSD(limit)}` }),
  ]);
}

function rowActions(k, opts) {
  const wrap = el('div', { class: 'row-actions' });
  wrap.appendChild(el('button', {
    class: 'btn btn-sm btn-ghost',
    text: k.disabled ? '启用' : '停用',
    onclick: async () => {
      try {
        await api.patch(`/admin/keys/${encodeURIComponent(k.id)}`, { disabled: !k.disabled });
        toast(`${k.disabled ? '已启用' : '已停用'} ${k.label}`, 'ok');
        if (opts.onChange) opts.onChange();
      } catch (e) { toast(e.message, 'err'); }
    },
  }));
  wrap.appendChild(el('button', {
    class: 'btn btn-sm btn-danger',
    text: '删除',
    onclick: async () => {
      if (!confirm(`确定要删除 Go Key "${k.label}" 吗?`)) return;
      try {
        await api.delete(`/admin/keys/${encodeURIComponent(k.id)}`);
        toast(`已删除 ${k.label}`, 'ok');
        if (opts.onChange) opts.onChange();
      } catch (e) { toast(e.message, 'err'); }
    },
  }));
  return wrap;
}

function openAddKeyModal(root) {
  const body = el('div', { class: 'form-grid' });
  const labelInput = el('input', { class: 'field', placeholder: '例如:account-A', value: '' });
  const keyInput = el('input', { class: 'field', placeholder: 'sk-go-xxxxxxxx', value: '' });
  const weightInput = el('input', { class: 'field', type: 'number', min: '1', value: '1' });
  const idInput = el('input', { class: 'field', placeholder: '(自动生成)', value: '' });
  const disabledSelect = el('select', { class: 'field' }, [
    el('option', { value: 'false', text: '启用' }),
    el('option', { value: 'true', text: '停用' }),
  ]);

  body.appendChild(el('label', { class: 'field-label', text: 'ID(可选)' }));
  body.appendChild(idInput);
  body.appendChild(el('label', { class: 'field-label', text: '名称' }));
  body.appendChild(labelInput);
  body.appendChild(el('label', { class: 'field-label', text: 'OpenCode Go Key' }));
  body.appendChild(keyInput);
  body.appendChild(el('p', { class: 'field-hint', text: '在 opencode.ai 订阅 Go 后获得的密钥,格式 sk-go-...' }));
  body.appendChild(el('label', { class: 'field-label', text: '权重' }));
  body.appendChild(weightInput);
  body.appendChild(el('label', { class: 'field-label', text: '状态' }));
  body.appendChild(disabledSelect);

  const close = modal.show({
    title: '新增 OpenCode Go Key',
    body,
    confirmLabel: '添加',
    onConfirm: async () => {
      if (!labelInput.value.trim() || !keyInput.value.trim()) {
        throw new Error('名称和 Go Key 不能为空');
      }
      await api.post('/admin/keys', {
        id: idInput.value.trim() || undefined,
        label: labelInput.value.trim(),
        api_key: keyInput.value.trim(),
        weight: parseInt(weightInput.value, 10) || 1,
        disabled: disabledSelect.value === 'true',
      });
      toast('Go Key 已添加', 'ok');
      views.keys();
      close();
    },
  });
}

views.models = async () => {
  clear($('#content'));
  const root = el('div', { class: 'view' });

  let data;
  try {
    data = await api.get('/v1/models');
  } catch (e) {
    root.appendChild(el('div', { class: 'card', text: '加载失败:' + e.message }));
    $('#content').appendChild(root);
    return;
  }

  const groups = {
    'openai-compatible': [],
    'anthropic-compatible': [],
  };
  for (const m of data.data || []) {
    if (groups[m.family]) groups[m.family].push(m);
  }

  for (const [family, models] of Object.entries(groups)) {
    if (models.length === 0) continue;
    const title = family === 'openai-compatible' ? 'OpenAI 兼容 (chat/completions)' : 'Anthropic 兼容 (messages)';
    const group = el('div', { class: 'model-group' });
    group.appendChild(el('h3', {}, [
      el('span', { text: title }),
      el('span', { class: 'muted', style: 'font-weight:400;text-transform:none;letter-spacing:0', text: `共 ${models.length} 个模型` }),
    ]));
    const grid = el('div', { class: 'model-grid' });
    for (const m of models) {
      grid.appendChild(el('div', { class: 'model-chip' }, [
        el('span', { class: 'id', text: m.id }),
        el('span', { class: 'tag', text: family === 'openai-compatible' ? 'OpenAI' : 'Anthropic' }),
      ]));
    }
    group.appendChild(grid);
    root.appendChild(group);
  }

  $('#content').appendChild(root);
};

views.cache = async () => {
  clear($('#content'));
  const root = el('div', { class: 'view' });

  let stats;
  try {
    stats = await api.get('/admin/stats');
  } catch (e) {
    root.appendChild(el('div', { class: 'card', text: '加载失败:' + e.message }));
    $('#content').appendChild(root);
    return;
  }

  const total = stats.Last24hTotal || 0;
  const hits = stats.Last24hCacheHits || 0;
  const hitRate = total > 0 ? ((hits / total) * 100).toFixed(1) + '%' : '—';

  const cards = el('div', { class: 'grid-4' }, [
    statCard('缓存条目', fmtNumber(stats.CacheEntries), 'SQLite 中的行数'),
    statCard('累计命中', fmtNumber(stats.CacheTotalHits), '所有条目的命中总和'),
    statCard('24h 请求', fmtNumber(total), '所有上游调用'),
    statCard('24h 命中率', hitRate, '由缓存服务的请求比例'),
  ]);
  root.appendChild(cards);

  const actions = el('div', { class: 'card section' }, [
    el('h3', { text: '操作' }),
    el('p', { class: 'muted', text: '清空会删除所有缓存的响应,之后的请求将重新打到上游。' }),
    el('div', { class: 'mt-12' }, el('button', {
      class: 'btn btn-danger',
      text: '清空缓存',
      onclick: async () => {
        if (!confirm('确定要清空所有缓存响应吗?')) return;
        try {
          await api.post('/admin/cache/flush', {});
          toast('缓存已清空', 'ok');
          views.cache();
        } catch (e) { toast(e.message, 'err'); }
      },
    })),
  ]);
  root.appendChild(actions);

  $('#content').appendChild(root);
};

views.tokens = async () => {
  clear($('#content'));
  const root = el('div', { class: 'view' });

  const header = el('div', { class: 'section-header' }, [
    el('div', {}, [
      el('div', { class: 'section-title', text: '访问令牌' }),
      el('div', { class: 'section-sub', text: 'go2api 自己签发的 API key,供客户端调用数据面接口时使用。' }),
    ]),
    el('button', {
      class: 'btn btn-primary',
      onclick: () => openCreateTokenModal(root),
      text: '+ 创建令牌',
    }),
  ]);
  root.appendChild(header);

  try {
    const data = await api.get('/admin/tokens');
    root.appendChild(renderTokensTable(data.tokens || [], { onChange: () => views.tokens() }));
  } catch (e) {
    root.appendChild(el('div', { class: 'card', text: '加载失败:' + e.message }));
  }

  $('#content').appendChild(root);
};

function renderTokensTable(tokens, opts = {}) {
  const wrap = el('div', { class: 'table-wrap' });
  if (tokens.length === 0) {
    wrap.appendChild(el('div', { class: 'empty', text: '暂无令牌。点击右上角"+ 创建令牌"签发第一个。' }));
    return wrap;
  }

  const table = el('table');
  const thead = el('thead', {}, el('tr', {}, [
    el('th', { text: '名称' }),
    el('th', { text: '前缀' }),
    el('th', { text: '用量' }),
    el('th', { text: '状态' }),
    el('th', { text: '上次调用' }),
    el('th', { text: '创建时间' }),
    el('th', { text: '', class: 'right' }),
  ]));
  table.appendChild(thead);

  const tbody = el('tbody');
  for (const t of tokens) {
    const quotaText = t.quota_limit > 0
      ? `${fmtNumber(t.quota_used)} / ${fmtNumber(t.quota_limit)}`
      : `${fmtNumber(t.quota_used)} / ∞`;
    const quotaPct = t.quota_limit > 0 ? (t.quota_used / t.quota_limit) * 100 : 0;
    const quotaKind = t.quota_limit > 0 && t.quota_used >= t.quota_limit ? 'err'
      : t.quota_limit > 0 && quotaPct >= 80 ? 'warn' : 'ok';

    let status, statusKind;
    if (t.revoked) {
      status = '已撤销'; statusKind = 'err';
    } else if (t.quota_limit > 0 && t.quota_used >= t.quota_limit) {
      status = '额度已尽'; statusKind = 'err';
    } else {
      status = '活跃'; statusKind = 'ok';
    }

    const tr = el('tr', {}, [
      el('td', {}, [
        el('div', { class: 'mono', text: t.label }),
        el('div', { class: 'muted', style: 'font-size:11px', text: t.id }),
      ]),
      el('td', { class: 'mono', text: t.prefix + '…' }),
      el('td', {}, [pill(quotaText, quotaKind)]),
      el('td', {}, pill(status, statusKind)),
      el('td', { class: 'muted', text: fmtTime(t.last_used_at) }),
      el('td', { class: 'muted', text: fmtTime(t.created_at) }),
      el('td', { class: 'right' }, tokenRowActions(t, opts)),
    ]);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  return wrap;
}

function tokenRowActions(t, opts) {
  const wrap = el('div', { class: 'row-actions' });
  if (!t.revoked) {
    wrap.appendChild(el('button', {
      class: 'btn btn-sm btn-ghost',
      text: '轮换',
      onclick: async () => {
        if (!confirm(`轮换令牌 "${t.label}"?旧 key 将立即失效,客户端需使用新 key。`)) return;
        try {
          const data = await api.post(`/admin/tokens/${encodeURIComponent(t.id)}/rotate`, {});
          showTokenModal({
            title: '令牌已轮换',
            label: t.label,
            token: data.token,
            prefix: data.prefix,
          });
          if (opts.onChange) opts.onChange();
        } catch (e) { toast(e.message, 'err'); }
      },
    }));
    if (t.quota_used > 0) {
      wrap.appendChild(el('button', {
        class: 'btn btn-sm btn-ghost',
        text: '重置额度',
        onclick: async () => {
          if (!confirm(`重置 "${t.label}" 的用量计数?`)) return;
          try {
            await api.post(`/admin/tokens/${encodeURIComponent(t.id)}/reset-quota`, {});
            toast('已重置用量', 'ok');
            if (opts.onChange) opts.onChange();
          } catch (e) { toast(e.message, 'err'); }
        },
      }));
    }
  }
  wrap.appendChild(el('button', {
    class: 'btn btn-sm btn-danger',
    text: t.revoked ? '删除' : '撤销',
    onclick: async () => {
      const verb = t.revoked ? '删除' : '撤销';
      if (!confirm(`${verb}令牌 "${t.label}"?`)) return;
      try {
        await api.delete(`/admin/tokens/${encodeURIComponent(t.id)}`);
        toast(`已${verb} ${t.label}`, 'ok');
        if (opts.onChange) opts.onChange();
      } catch (e) { toast(e.message, 'err'); }
    },
  }));
  return wrap;
}

function openCreateTokenModal(root) {
  const body = el('div', { class: 'form-grid' });
  const labelInput = el('input', { class: 'field', placeholder: '例如:team-A laptop', value: '' });
  const quotaInput = el('input', { class: 'field', type: 'number', min: '0', value: '0', placeholder: '0 表示无限制' });

  body.appendChild(el('label', { class: 'field-label', text: '令牌名称' }));
  body.appendChild(labelInput);
  body.appendChild(el('label', { class: 'field-label', text: '用量限额(可选)' }));
  body.appendChild(quotaInput);
  body.appendChild(el('p', { class: 'field-hint', text: '允许的最大请求次数。0 表示无限制。达到后令牌自动失效。' }));

  modal.show({
    title: '创建访问令牌',
    body,
    confirmLabel: '签发',
    onConfirm: async () => {
      if (!labelInput.value.trim()) {
        throw new Error('请填写令牌名称');
      }
      const quota = parseInt(quotaInput.value, 10);
      if (isNaN(quota) || quota < 0) {
        throw new Error('限额必须是非负整数(0 表示无限制)');
      }
      const data = await api.post('/admin/tokens', {
        label: labelInput.value.trim(),
        quota_limit: quota,
      });
      // 替换弹窗内容为"展示 token"界面 —— 不关闭,用户必须手动确认
      showTokenModal({
        title: '令牌已创建',
        label: data.label,
        token: data.token,
        prefix: data.prefix,
        quotaLimit: data.quota_limit,
      });
      views.tokens();
    },
  });
}

// 令牌创建/轮换后展示一次,带复制和下载按钮
// 注意:这个函数直接复用 #modal,不调用 modal.show,以避免 close() 副作用
function showTokenModal({ title, label, token, prefix, quotaLimit }) {
  const body = el('div', { class: 'token-reveal' });

  body.appendChild(el('div', { class: 'token-reveal-warn', text: '⚠ 此令牌仅在此处显示一次,请立即复制或下载。关闭后无法再次查看完整值。' }));

  body.appendChild(el('div', { class: 'token-reveal-label', text: '令牌' }));
  const tokenBox = el('div', { class: 'token-reveal-box' });
  tokenBox.appendChild(el('code', { class: 'token-reveal-code', text: token }));
  tokenBox.appendChild(el('button', {
    class: 'btn btn-sm btn-ghost',
    text: '复制',
    onclick: async () => {
      try {
        await navigator.clipboard.writeText(token);
        toast('已复制到剪贴板', 'ok', 1500);
      } catch {
        toast('复制失败,请手动选中', 'err');
      }
    },
  }));
  body.appendChild(tokenBox);

  const quotaText = quotaLimit != null ? (quotaLimit === 0 ? '无限制' : quotaLimit) : '—';
  body.appendChild(el('div', { class: 'token-reveal-meta', text: `前缀:${prefix} · 用量限额:${quotaText} · 标签:${label}` }));

  const usage = el('div', { class: 'token-reveal-usage' });
  usage.appendChild(el('div', { class: 'token-reveal-label', text: '调用示例' }));
  usage.appendChild(el('pre', { class: 'token-reveal-pre', text:
`curl -X POST http://localhost:8080/v1/chat/completions \\
  -H "Authorization: Bearer ${token}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}'` }));
  body.appendChild(usage);

  // 直接替换弹窗内容(不触发 modal.show 的 onConfirm 自动关闭)
  setModalContent(title, body, '我已保存');

  // 重新绑定按钮:这次"我已保存"和"取消"/X 都直接关闭弹窗
  const close = () => { $('#modal').hidden = true; $('#modal-confirm').onclick = null; };
  $('#modal-close').onclick = close;
  $('#modal-cancel').onclick = close;
  $('#modal-confirm').onclick = close;

  // 移除旧下载按钮(如果有),插入新的
  const footer = $('#modal').querySelector('.modal-footer');
  const oldDl = footer.querySelector('.btn-download-token');
  if (oldDl) oldDl.remove();
  const dlBtn = el('button', {
    class: 'btn btn-ghost btn-download-token',
    text: '下载 .txt',
    onclick: () => downloadTokenFile({ label, token, prefix, quotaLimit }),
  });
  footer.insertBefore(dlBtn, $('#modal-cancel'));
}

function downloadTokenFile({ label, token, prefix, quotaLimit }) {
  const quotaText = quotaLimit != null
    ? (quotaLimit === 0 ? 'unlimited (no quota)' : `${quotaLimit} requests`)
    : 'unlimited';
  const content =
`go2api API token
================

Label:       ${label}
Prefix:      ${prefix}...
Created at:  ${new Date().toISOString()}
Quota:       ${quotaText}

YOUR TOKEN (treat this like a password; never commit it):
    ${token}

Usage example
-------------
    curl -X POST http://localhost:8080/v1/chat/completions \\
      -H "Authorization: Bearer ${token}" \\
      -H "Content-Type: application/json" \\
      -d '{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}'

You can revoke this token at any time from the go2api admin console.
`;
  const blob = new Blob([content], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `go2api-token-${prefix}-${Date.now()}.txt`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

// ----- 辅助 ----------------------------------------------------------------

function pill(text, kind = 'info') {
  return el('span', { class: `pill ${kind}` }, [
    el('span', { class: 'pill-dot' }),
    text,
  ]);
}

// ----- 路由 ----------------------------------------------------------------

const titles = {
  dashboard: '概览',
  keys: 'Go Key',
  tokens: '访问令牌',
  models: '模型',
  cache: '缓存',
};

async function route() {
  // 隐藏 shell: 未登录时不路由
  if ($('#app-shell').hidden) return;

  const hash = (location.hash || '#/dashboard').replace(/^#\//, '');
  const view = views[hash] || views.dashboard;

  for (const a of document.querySelectorAll('.nav-item')) {
    a.classList.toggle('active', a.dataset.route === hash);
  }
  $('#page-title').textContent = titles[hash] || '概览';

  try {
    await view();
  } catch (e) {
    console.error(e);
  }
}

window.addEventListener('hashchange', route);
$('#refresh-btn').addEventListener('click', route);

// ----- 启动 ----------------------------------------------------------------

bootstrapAuth().then(route);
pingHealth();
setInterval(pingHealth, 15000);