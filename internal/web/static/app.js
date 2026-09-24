// Roundhouse dashboard. Plain JavaScript against the daemon's JSON API
// (the same endpoints the CLI uses). Every value from the server is
// inserted as text, never as HTML: service names, logs and env values come
// from users and containers.
'use strict';

// ---------------------------------------------------------------------------
// tiny helpers

const $ = (sel, root = document) => root.querySelector(sel);

// h('div.card', {onclick}, child, 'text', …) builds DOM safely.
function h(tag, attrs, ...children) {
  const [name, ...classes] = tag.split('.');
  const el = document.createElement(name);
  if (classes.length) el.className = classes.join(' ');
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className += ' ' + v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function ago(iso) {
  if (!iso || iso.startsWith('0001')) return '';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KiB', 'MiB', 'GiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

function shortImage(ref) {
  return (ref || '')
    .replace(/^docker\.io\/library\//, '')
    .replace(/^mirror\.gcr\.io\/library\//, '');
}

function toast(msg, error = false) {
  const t = h('div.toast' + (error ? '.error' : ''), {}, msg);
  $('#toasts').append(t);
  setTimeout(() => t.remove(), error ? 7000 : 3500);
}

const NAME_RE = /^[a-z][a-z0-9-]{0,30}[a-z0-9]$/;

// ---------------------------------------------------------------------------
// API

class Unauthorized extends Error {}

async function api(method, path, body) {
  const opts = { method, headers: { 'X-Requested-By': 'roundhouse-ui' } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) {
    showLogin();
    throw new Unauthorized('login required');
  }
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  if (!res.ok) throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  return data;
}

// stream reads a line-oriented response (NDJSON or text) and calls onLine.
async function stream(path, onLine, signal) {
  const res = await fetch(path, { signal });
  if (res.status === 401) {
    showLogin();
    return;
  }
  if (!res.ok || !res.body) return;
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (line) onLine(line);
    }
  }
  if (buf) onLine(buf);
}

// ---------------------------------------------------------------------------
// state

const state = {
  services: [],
  builds: [],
  usage: {},
  selected: null,
  tab: 'deployments',
  tabAbort: null,
  newKind: 'template',
};

function statusClass(status) {
  const s = (status || '').toUpperCase();
  if (s.startsWith('ACTIVE') && s.includes('DEPLOYING')) return 'busy';
  if (s === 'ACTIVE' || s === 'SUCCESS') return 'ok';
  if (['FAILED', 'CRASHED'].includes(s)) return 'bad';
  if (['QUEUED', 'DEPLOYING', 'BUILDING', 'CLONING', 'DELETING'].includes(s)) return 'busy';
  if (s === 'REMOVED' || s === 'SKIPPED') return '';
  return 'warn';
}

function pill(status) {
  return h('span.pill.' + (statusClass(status) || 'none'), {}, (status || 'unknown').toLowerCase());
}

function activeBuild(service) {
  return state.builds.find(
    (b) =>
      b.service === service && ['QUEUED', 'CLONING', 'BUILDING', 'DEPLOYING'].includes(b.status),
  );
}

// ---------------------------------------------------------------------------
// canvas

function renderCanvas() {
  const cards = $('#cards');
  cards.replaceChildren();
  const names = new Set(state.services.map((s) => s.name));
  // Services being built for the first time do not exist in the engine yet.
  const pending = state.builds.filter(
    (b) =>
      !names.has(b.service) &&
      ['QUEUED', 'CLONING', 'BUILDING', 'DEPLOYING', 'FAILED'].includes(b.status),
  );
  const seen = new Set();
  for (const b of pending) {
    if (seen.has(b.service)) continue;
    seen.add(b.service);
    cards.append(
      h(
        'div.card',
        { onclick: () => openBuildLog(b) },
        h(
          'div.card-top',
          {},
          h('span.svc-icon', {}, b.service[0].toUpperCase()),
          h('div', {}, h('div.card-name', {}, b.service), h('div.card-sub', {}, b.source)),
        ),
        h(
          'div.card-foot',
          {},
          pill(b.status === 'FAILED' ? 'FAILED' : 'BUILDING'),
          h('span', {}, b.status === 'FAILED' ? 'build failed · view logs' : 'view build logs'),
        ),
      ),
    );
  }
  for (const s of state.services) {
    const act = s.active;
    const ready = s.instances.filter((i) => act && i.deployment === act.id && i.healthy).length;
    const building = activeBuild(s.name);
    const status = building ? 'BUILDING' : s.status;
    const dots = [];
    for (let i = 0; i < (s.spec.replicas || 1); i++) {
      dots.push(
        h('span.replica-dot.' + (i < ready ? 'ok' : s.status === 'CRASHED' ? 'bad' : 'warn')),
      );
    }
    cards.append(
      h(
        'div.card' + (state.selected === s.name ? '.selected' : ''),
        {
          onclick: () => openDrawer(s.name),
          tabindex: 0,
          onkeydown: (e) => e.key === 'Enter' && openDrawer(s.name),
        },
        h(
          'div.card-top',
          {},
          h('span.svc-icon', {}, s.name[0].toUpperCase()),
          h(
            'div',
            { style: 'min-width:0' },
            h('div.card-name', {}, s.name),
            h('div.card-sub', {}, shortImage(s.spec.image)),
          ),
        ),
        h(
          'div.card-foot',
          {},
          pill(status.split(' ')[0]),
          h('span.replicas', { title: `${ready}/${s.spec.replicas} healthy` }, dots),
        ),
        s.publicUrl ? h('div.card-sub', { style: 'margin-top:8px' }, publicHref(s)) : null,
      ),
    );
  }
  $('#empty').hidden = state.services.length > 0 || pending.length > 0;
}

// ---------------------------------------------------------------------------
// drawer

function current() {
  return state.services.find((s) => s.name === state.selected);
}

function publicHref(s) {
  if (!s.publicUrl) return null;
  const port = s.publicUrl.split(':').pop();
  return `${location.protocol}//${location.hostname}:${port}`;
}

function openDrawer(name) {
  state.selected = name;
  $('#drawer').hidden = false;
  setTab(state.tab);
  renderCanvas();
}

function closeDrawer() {
  state.selected = null;
  if (state.tabAbort) state.tabAbort.abort();
  $('#drawer').hidden = true;
  renderCanvas();
}

function renderDrawerHead() {
  const s = current();
  if (!s) return closeDrawer();
  $('#d-icon').textContent = s.name[0].toUpperCase();
  $('#d-name').textContent = s.name;
  $('#d-status').replaceWith(Object.assign(pill(s.status.split(' ')[0]), { id: 'd-status' }));
  const links = $('#d-links');
  links.replaceChildren();
  const pub = publicHref(s);
  if (pub) links.append(h('a', { href: pub, target: '_blank', rel: 'noopener' }, pub + ' ↗'));
  if (s.privateUrl)
    links.append(
      h(
        'code',
        { title: 'private network address, from other services' },
        s.privateUrl.replace('http://', ''),
      ),
    );
}

function setTab(tab) {
  state.tab = tab;
  if (state.tabAbort) state.tabAbort.abort();
  state.tabAbort = new AbortController();
  for (const b of document.querySelectorAll('#drawer .tabs button'))
    b.classList.toggle('active', b.dataset.tab === tab);
  renderDrawerHead();
  renderTab();
}

function renderTab() {
  const s = current();
  if (!s) return;
  const body = $('#tab-body');
  body.replaceChildren();
  ({
    deployments: tabDeployments,
    logs: tabLogs,
    metrics: tabMetrics,
    variables: tabVariables,
    settings: tabSettings,
  })[state.tab](body, s);
}

// Re-render live tabs on refresh; forms and logs manage themselves.
function refreshDrawer() {
  if (!state.selected) return;
  renderDrawerHead();
  if (state.tab === 'deployments' || state.tab === 'metrics') renderTab();
}

function tabDeployments(body, s) {
  const builds = state.builds.filter((b) => b.service === s.name).slice(0, 5);
  const byDeployment = Object.fromEntries(
    builds.filter((b) => b.deployment).map((b) => [b.deployment, b]),
  );
  for (const b of builds.filter((b) => !b.deployment)) {
    body.append(
      h(
        'div.deploy',
        {},
        h(
          'div.deploy-row',
          {},
          h('div', {}, h('strong', {}, 'Build '), pill(b.status)),
          h(
            'div.deploy-actions',
            {},
            h('button.btn', { type: 'button', onclick: () => openBuildLog(b) }, 'Build logs'),
          ),
        ),
        h('div.deploy-meta', {}, `${b.source}${b.dir ? ' · ' + b.dir : ''} · ${ago(b.createdAt)}`),
        b.error ? h('div.deploy-reason', {}, b.error) : null,
      ),
    );
  }
  const deps = [...s.deployments].reverse();
  const activeId = s.active && s.active.id;
  deps.forEach((d, idx) => {
    const build = byDeployment[d.id];
    const actions = h('div.deploy-actions');
    if (idx === 0)
      actions.append(
        h('button.btn', { type: 'button', onclick: () => redeploy(s.name) }, 'Redeploy'),
      );
    if (d.activeAt && !d.activeAt.startsWith('0001') && d.id !== activeId) {
      actions.append(
        h(
          'button.btn',
          { type: 'button', onclick: () => rollback(s.name, d) },
          'Roll back to this',
        ),
      );
    }
    if (build)
      actions.append(
        h('button.btn', { type: 'button', onclick: () => openBuildLog(build) }, 'Build logs'),
      );
    body.append(
      h(
        'div.deploy' + (d.id === activeId ? '.active' : ''),
        {},
        h(
          'div.deploy-row',
          {},
          h('div', {}, h('strong', {}, `Rev ${d.revision} `), pill(d.status)),
          actions,
        ),
        h(
          'div.deploy-meta',
          {},
          `${shortImage(d.spec.image)} · ${d.spec.replicas} replica${d.spec.replicas > 1 ? 's' : ''} · ${ago(d.createdAt)}${build && build.commit ? ' · commit ' + build.commit : ''}`,
        ),
        d.reason ? h('div.deploy-reason', {}, d.reason) : null,
        d.logs && d.logs.length
          ? h('pre.logs', { style: 'margin-top:8px;max-height:200px' }, d.logs.join('\n'))
          : null,
      ),
    );
  });
  if (!deps.length && !builds.length) body.append(h('p.muted', {}, 'No deployments yet.'));
}

function tabLogs(body, s) {
  const pre = h('pre.logs', {}, '');
  const bar = h(
    'div',
    { style: 'display:flex;justify-content:space-between;margin-bottom:10px' },
    h('span.muted', {}, 'Live logs from every replica'),
    h('button.btn', { type: 'button', onclick: () => pre.replaceChildren() }, 'Clear'),
  );
  body.append(bar, pre);
  const signal = state.tabAbort.signal;
  stream(
    `/v1/services/${encodeURIComponent(s.name)}/logs?follow=1&tail=200`,
    (line) => {
      let l;
      try {
        l = JSON.parse(line);
      } catch {
        return;
      }
      const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 30;
      pre.append(
        h('span.inst', {}, l.instance + '  '),
        h('span' + (l.s === 'stderr' ? '.err' : ''), {}, l.l + '\n'),
      );
      while (pre.childNodes.length > 4000) pre.firstChild.remove();
      if (atBottom) pre.scrollTop = pre.scrollHeight;
    },
    signal,
  ).catch(() => {});
}

function tabMetrics(body, s) {
  const running = s.instances.filter((i) => i.status === 'running');
  const mem = running.reduce((a, i) => a + (i.memoryBytes || 0), 0);
  const u = state.usage[s.name];
  body.append(
    h(
      'div.stat-row',
      {},
      h('div.stat', {}, h('div.label', {}, 'Memory now'), h('div.value', {}, bytes(mem))),
      h(
        'div.stat',
        {},
        h('div.label', {}, 'CPU used (total)'),
        h('div.value', {}, u ? `${u.cpuSeconds.toFixed(1)} s` : '–'),
      ),
      h(
        'div.stat',
        {},
        h('div.label', {}, 'Cost so far'),
        h('div.value', {}, u ? `$${u.costUsd.toFixed(4)}` : '–'),
      ),
    ),
    h('div.section-title', {}, 'Instances'),
    h(
      'table',
      {},
      h(
        'thead',
        {},
        h(
          'tr',
          {},
          ['Instance', 'Status', 'Health', 'IP', 'Restarts', 'CPU time', 'Memory'].map((t) =>
            h('th', {}, t),
          ),
        ),
      ),
      h(
        'tbody',
        {},
        s.instances.map((i) =>
          h(
            'tr',
            {},
            h('td', {}, i.name),
            h(
              'td',
              {},
              i.status === 'exited'
                ? `exited (${i.exitCode})${i.oomKilled ? ' OOM' : ''}`
                : i.status,
            ),
            h(
              'td',
              {},
              i.status !== 'running'
                ? '–'
                : i.healthy
                  ? 'healthy'
                  : i.health
                    ? 'unhealthy'
                    : 'starting',
            ),
            h('td', {}, i.ip || ''),
            h('td', {}, String(i.restarts)),
            h('td', {}, i.cpuNanos ? `${(i.cpuNanos / 1e9).toFixed(2)} s` : '–'),
            h('td', {}, bytes(i.memoryBytes)),
          ),
        ),
      ),
    ),
    h(
      'p.hint',
      { style: 'margin-top:14px' },
      "Usage is sampled from each container's cgroup and billed per second of CPU and memory actually used.",
    ),
  );
}

function envRows(container, env) {
  container.replaceChildren();
  const add = (k = '', v = '') => {
    const row = h(
      'div.env-row',
      {},
      h('input', {
        placeholder: 'NAME',
        value: k,
        'aria-label': 'Variable name',
        spellcheck: 'false',
      }),
      h('input', {
        placeholder: 'value or ${{other-service.VAR}}',
        value: v,
        'aria-label': 'Variable value',
        spellcheck: 'false',
      }),
      h(
        'button.icon-btn',
        { type: 'button', 'aria-label': 'Remove', onclick: () => row.remove() },
        '✕',
      ),
    );
    container.append(row);
  };
  for (const [k, v] of Object.entries(env || {})) add(k, v);
  return add;
}

function readEnv(container) {
  const env = {};
  for (const row of container.querySelectorAll('.env-row')) {
    const [k, v] = row.querySelectorAll('input');
    if (k.value.trim()) env[k.value.trim()] = v.value;
  }
  return env;
}

function tabVariables(body, s) {
  const list = h('div');
  const add = envRows(list, s.spec.env);
  body.append(
    h(
      'p.hint',
      {},
      'Saving creates a new deployment. Reference another service with ${{service.VAR}}; ${{service.RH_PRIVATE_DOMAIN}} and ${{service.PORT}} always exist.',
    ),
    list,
    h('button.btn', { type: 'button', onclick: () => add() }, '+ Add variable'),
    h(
      'div.form-actions',
      {},
      h(
        'button.btn.btn-primary',
        { type: 'button', onclick: () => saveSpec({ ...s.spec, env: readEnv(list) }) },
        'Save and deploy',
      ),
    ),
  );
}

function specFields(spec, { withImage = true } = {}) {
  const f = (label, name, value, attrs = {}) =>
    h('label', {}, label, h('input', { name, value: value ?? '', ...attrs }));
  const hc = spec.healthcheck || {};
  return h(
    'div.form-grid',
    {},
    withImage
      ? h('div.full', {}, f('Image', 'image', spec.image, { required: true, spellcheck: 'false' }))
      : null,
    f('Port the app listens on ($PORT)', 'port', spec.port || '', {
      type: 'number',
      min: 0,
      max: 65535,
    }),
    f('Public port (empty = private only)', 'publicPort', spec.publicPort || '', {
      type: 'number',
      min: 0,
      max: 65535,
    }),
    f('Replicas', 'replicas', spec.replicas || 1, { type: 'number', min: 1, max: 50 }),
    f(
      'Health check path (empty = none, "tcp" = port open)',
      'health',
      hc.type === 'tcp' ? 'tcp' : hc.path || '',
    ),
    f('CPU limit (cores)', 'cpu', spec.cpu || '', { type: 'number', step: '0.1', min: 0 }),
    f('Memory limit (MB)', 'memoryMb', spec.memoryMb || '', { type: 'number', min: 0 }),
    f('Start command (optional)', 'cmd', (spec.cmd || []).join(' '), { spellcheck: 'false' }),
    h(
      'label',
      {},
      'Restart policy',
      h(
        'select',
        { name: 'restart' },
        ['on-failure', 'always', 'never'].map((r) =>
          h('option', { value: r, selected: (spec.restart || 'on-failure') === r }, r),
        ),
      ),
    ),
    f('Volume name (optional)', 'volumeName', spec.volume ? spec.volume.name : '', {
      spellcheck: 'false',
    }),
    f('Volume mount path', 'volumePath', spec.volume ? spec.volume.mountPath : '', {
      spellcheck: 'false',
    }),
  );
}

function readSpec(form, base) {
  const v = (n) => form.querySelector(`[name="${n}"]`)?.value.trim() ?? '';
  const num = (n) => (v(n) === '' ? 0 : Number(v(n)));
  const spec = { ...base };
  if (form.querySelector('[name="image"]')) spec.image = v('image');
  spec.port = num('port');
  spec.publicPort = num('publicPort');
  spec.replicas = num('replicas') || 1;
  spec.cpu = num('cpu');
  spec.memoryMb = num('memoryMb');
  spec.restart = v('restart');
  spec.cmd = v('cmd') ? v('cmd').split(/\s+/) : undefined;
  const health = v('health');
  spec.healthcheck =
    health === ''
      ? undefined
      : health === 'tcp'
        ? { type: 'tcp' }
        : { type: 'http', path: health.startsWith('/') ? health : '/' + health };
  spec.volume = v('volumeName')
    ? { name: v('volumeName'), mountPath: v('volumePath') || '/data' }
    : undefined;
  return spec;
}

function tabSettings(body, s) {
  const form = h('form', {}, specFields(s.spec));
  form.append(
    h('div.form-actions', {}, h('button.btn.btn-primary', { type: 'submit' }, 'Save and deploy')),
  );
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    saveSpec(readSpec(form, s.spec));
  });
  body.append(
    form,
    h(
      'div.danger-zone',
      {},
      h('strong', {}, 'Delete service'),
      h('p.hint', {}, 'Stops and removes every instance. Volumes are kept.'),
      h(
        'button.btn.btn-danger',
        { type: 'button', onclick: () => deleteService(s.name) },
        `Delete ${s.name}`,
      ),
    ),
  );
}

// ---------------------------------------------------------------------------
// actions

function cleanSpec(spec) {
  const out = {};
  for (const [k, v] of Object.entries(spec)) {
    if (
      v === undefined ||
      v === null ||
      v === '' ||
      (typeof v === 'number' && v === 0 && k !== 'port')
    )
      continue;
    out[k] = v;
  }
  return out;
}

async function saveSpec(spec) {
  try {
    const d = await api('PUT', `/v1/services/${encodeURIComponent(spec.name)}`, cleanSpec(spec));
    toast(`Rev ${d.revision} of ${spec.name}: ${d.status.toLowerCase()}`);
    state.tab = 'deployments';
    await refresh();
    if (state.selected) setTab('deployments');
  } catch (e) {
    if (!(e instanceof Unauthorized)) toast(e.message, true);
  }
}

async function redeploy(name) {
  try {
    const d = await api('POST', `/v1/services/${encodeURIComponent(name)}/redeploy`);
    toast(`Redeploying ${name} (rev ${d.revision})`);
    refresh();
  } catch (e) {
    toast(e.message, true);
  }
}

async function rollback(name, d) {
  if (!confirm(`Roll ${name} back to rev ${d.revision} (${shortImage(d.spec.image)})?`)) return;
  try {
    const nd = await api('POST', `/v1/services/${encodeURIComponent(name)}/rollback`, {
      deployment: d.id,
    });
    toast(`Rolling back: rev ${nd.revision}`);
    refresh();
  } catch (e) {
    toast(e.message, true);
  }
}

async function deleteService(name) {
  if (prompt(`Type ${name} to delete it.`) !== name) return;
  try {
    await api('DELETE', `/v1/services/${encodeURIComponent(name)}`);
    toast(`Deleting ${name}`);
    closeDrawer();
    refresh();
  } catch (e) {
    toast(e.message, true);
  }
}

// ---------------------------------------------------------------------------
// new service

const TEMPLATES = [
  {
    id: 'postgres',
    icon: 'P',
    name: 'PostgreSQL',
    desc: 'Postgres 17 with a persistent volume. Other services reach it at postgres.rh.internal:5432.',
    spec: {
      name: 'postgres',
      image: 'mirror.gcr.io/library/postgres:17-alpine',
      port: 5432,
      env: { POSTGRES_USER: 'app', POSTGRES_PASSWORD: 'change-me', POSTGRES_DB: 'app' },
      volume: { name: 'postgres-data', mountPath: '/var/lib/postgresql/data' },
      healthcheck: { type: 'tcp' },
      memoryMb: 512,
      stopGraceSeconds: 30,
      deployTimeoutSeconds: 180,
    },
  },
  {
    id: 'redis',
    icon: 'R',
    name: 'Redis',
    desc: 'In-memory data store at redis.rh.internal:6379.',
    spec: {
      name: 'redis',
      image: 'mirror.gcr.io/library/redis:7-alpine',
      port: 6379,
      healthcheck: { type: 'tcp' },
      memoryMb: 256,
    },
  },
  {
    id: 'nginx',
    icon: 'N',
    name: 'nginx',
    desc: 'A web server on public port 8080, two replicas behind the edge proxy.',
    spec: {
      name: 'nginx',
      image: 'mirror.gcr.io/library/nginx:alpine',
      port: 80,
      publicPort: 8080,
      replicas: 2,
      healthcheck: { type: 'http', path: '/' },
    },
  },
  {
    id: 'web',
    icon: 'W',
    name: 'Static site (busybox)',
    desc: 'A tiny web server that shows which replica answered. Good for watching rolling deploys.',
    spec: {
      name: 'web',
      image: 'mirror.gcr.io/library/busybox:latest',
      cmd: [
        'sh',
        '-c',
        'mkdir -p /www && echo "$VERSION from $HOSTNAME" > /www/index.html && echo ok > /www/health && exec httpd -f -v -p $PORT -h /www',
      ],
      env: { VERSION: 'v1' },
      port: 8080,
      publicPort: 8090,
      replicas: 2,
      healthcheck: { type: 'http', path: '/health' },
      drainSeconds: 3,
    },
  },
  {
    id: 'hello',
    icon: 'H',
    name: 'Hello app (build from source)',
    desc: 'Builds examples/hello from the Roundhouse repository with the multi-stage Railfile, then deploys it.',
    build: { source: 'https://github.com/aggarwalpulkit596/roundhouse', dir: 'examples/hello' },
    spec: {
      name: 'hello',
      port: 8080,
      publicPort: 8000,
      replicas: 2,
      healthcheck: { type: 'http', path: '/health' },
      env: { VERSION: 'v1' },
    },
  },
];

function openNew(kind = 'template') {
  state.newKind = kind;
  $('#modal').hidden = false;
  renderNew();
}

function closeNew() {
  $('#modal').hidden = true;
}

function renderNew() {
  for (const b of document.querySelectorAll('#new-tabs button'))
    b.classList.toggle('active', b.dataset.kind === state.newKind);
  const body = $('#modal-body');
  body.replaceChildren();
  if (state.newKind === 'template') {
    body.append(
      h(
        'div.templates',
        {},
        TEMPLATES.map((t) =>
          h(
            'button.template',
            { type: 'button', onclick: () => deployTemplate(t) },
            h('div.t-name', {}, h('span.svc-icon', {}, t.icon), t.name),
            h('div.t-desc', {}, t.desc),
          ),
        ),
      ),
    );
    return;
  }
  const isRepo = state.newKind === 'repo';
  const form = h('form');
  const nameInput = h('input', {
    name: 'name',
    required: true,
    placeholder: 'my-service',
    pattern: NAME_RE.source.slice(1, -1),
    spellcheck: 'false',
  });
  form.append(
    h(
      'div.form-grid',
      {},
      h('label.full', {}, 'Service name (becomes <name>.rh.internal)', nameInput),
    ),
  );
  if (isRepo) {
    form.append(
      h(
        'div.form-grid',
        { style: 'margin-top:14px' },
        h(
          'label.full',
          {},
          'Git URL or folder on this machine',
          h('input', {
            name: 'source',
            required: true,
            placeholder: 'https://github.com/you/app  or  /home/ubuntu/app',
            spellcheck: 'false',
          }),
        ),
        h(
          'label',
          {},
          'Branch or tag (optional)',
          h('input', { name: 'ref', spellcheck: 'false' }),
        ),
        h(
          'label',
          {},
          'Folder inside the repo (optional)',
          h('input', { name: 'dir', placeholder: 'e.g. examples/hello', spellcheck: 'false' }),
        ),
        h(
          'p.hint.full',
          {},
          'Needs a Railfile or Dockerfile. Public repositories only (or a folder the daemon can read).',
        ),
      ),
    );
  }
  const specDiv = h(
    'div',
    { style: 'margin-top:14px' },
    specFields({ port: 8080, replicas: 1 }, { withImage: !isRepo }),
  );
  const envList = h('div');
  const addEnv = envRows(envList, {});
  form.append(
    specDiv,
    h('div.section-title', {}, 'Variables'),
    envList,
    h('button.btn', { type: 'button', onclick: () => addEnv() }, '+ Add variable'),
    h(
      'div.form-actions',
      {},
      h('button.btn', { type: 'button', onclick: closeNew }, 'Cancel'),
      h('button.btn.btn-primary', { type: 'submit' }, isRepo ? 'Build and deploy' : 'Deploy'),
    ),
  );
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const name = nameInput.value.trim();
    if (!NAME_RE.test(name))
      return toast('Name: 2–32 characters, lowercase letters, digits and dashes.', true);
    const spec = readSpec(form, { name });
    spec.env = readEnv(envList);
    if (isRepo) {
      const v = (n) => form.querySelector(`[name="${n}"]`).value.trim();
      await startBuild({
        source: v('source'),
        ref: v('ref') || undefined,
        dir: v('dir') || undefined,
        spec: cleanSpec(spec),
      });
    } else {
      await saveSpec(spec);
      closeNew();
      openDrawer(name);
    }
  });
  body.append(form);
}

async function deployTemplate(t) {
  let name = t.spec.name;
  const taken = new Set(state.services.map((s) => s.name));
  for (let i = 2; taken.has(name); i++) name = `${t.spec.name}-${i}`;
  const spec = { ...t.spec, name };
  if (spec.volume) spec.volume = { ...spec.volume, name: name + '-data' };
  if (spec.publicPort) {
    const used = new Set(state.services.map((s) => s.spec.publicPort).filter(Boolean));
    while (used.has(spec.publicPort)) spec.publicPort++;
  }
  if (t.build) {
    await startBuild({ ...t.build, spec });
  } else {
    await saveSpec(spec);
    closeNew();
    openDrawer(name);
  }
}

async function startBuild(req) {
  try {
    const b = await api('POST', '/v1/builds', req);
    toast(`Building ${b.service}…`);
    closeNew();
    await refresh();
    openBuildLog(b);
  } catch (e) {
    if (!(e instanceof Unauthorized)) toast(e.message, true);
  }
}

let buildLogAbort = null;
function openBuildLog(b) {
  if (buildLogAbort) buildLogAbort.abort();
  buildLogAbort = new AbortController();
  $('#bl-title').textContent = `Build ${b.id.slice(0, 8)} · ${b.service}`;
  const pre = $('#bl-body');
  pre.replaceChildren();
  $('#buildlog').hidden = false;
  stream(
    `/v1/builds/${b.id}/logs?follow=1`,
    (line) => {
      pre.append(h('span' + (line.startsWith('=> error') ? '.err' : ''), {}, line + '\n'));
      pre.scrollTop = pre.scrollHeight;
    },
    buildLogAbort.signal,
  ).catch(() => {});
}

// ---------------------------------------------------------------------------
// activity feed

function startActivity() {
  const list = $('#activity-list');
  const add = (ev) => {
    const li = h(
      'li',
      {},
      h('div.when', {}, new Date(ev.time).toLocaleTimeString() + (ev.type ? ' · ' + ev.type : '')),
      h('div', {}, ev.service ? h('span.svc', {}, ev.service + ' ') : null, ev.message),
    );
    list.prepend(li);
    while (list.children.length > 200) list.lastChild.remove();
    if (ev.type === 'deployment' || ev.type === 'health' || ev.type === 'service')
      scheduleRefresh();
  };
  const run = () =>
    stream('/v1/events?follow=1', (line) => {
      try {
        add(JSON.parse(line));
      } catch {}
    })
      .catch(() => {})
      .finally(() => setTimeout(run, 2000)); // reconnect after daemon restarts
  run();
}

// ---------------------------------------------------------------------------
// refresh loop

let refreshTimer = null;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(refresh, 150);
}

async function refresh() {
  try {
    const [services, builds] = await Promise.all([
      api('GET', '/v1/services?stats=1'),
      api('GET', '/v1/builds'),
    ]);
    // Go encodes empty slices as null; normalise once here.
    state.services = (services || []).map((s) => ({
      ...s,
      instances: s.instances || [],
      deployments: s.deployments || [],
      spec: s.spec || {},
    }));
    state.builds = builds || [];
    renderCanvas();
    refreshDrawer();
  } catch (e) {
    if (!(e instanceof Unauthorized)) console.warn(e);
  }
}

async function refreshSlow() {
  try {
    const [usage, node] = await Promise.all([api('GET', '/v1/usage'), api('GET', '/v1/node')]);
    state.usage = Object.fromEntries((usage.services || []).map((u) => [u.service, u]));
    $('#node-name').textContent = node.name;
    $('#node-meta').textContent =
      `${node.cpus} CPUs · ${(node.memoryMb / 1024).toFixed(1)} GB · ${node.instances} instances reserved`;
  } catch {}
}

// ---------------------------------------------------------------------------
// login

function showLogin() {
  $('#login').hidden = false;
}

$('#login-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const t = e.target.token.value.trim();
  location.href = '/login?token=' + encodeURIComponent(t);
});

// ---------------------------------------------------------------------------
// wiring

$('#new-btn').addEventListener('click', () => openNew('template'));
$('#modal-close').addEventListener('click', closeNew);
$('#bl-close').addEventListener('click', () => {
  $('#buildlog').hidden = true;
  if (buildLogAbort) buildLogAbort.abort();
});
$('#d-close').addEventListener('click', closeDrawer);
for (const b of document.querySelectorAll('#drawer .tabs button'))
  b.addEventListener('click', () => setTab(b.dataset.tab));
for (const b of document.querySelectorAll('#new-tabs button'))
  b.addEventListener('click', () => ((state.newKind = b.dataset.kind), renderNew()));
for (const b of document.querySelectorAll('[data-open-new]'))
  b.addEventListener('click', () => openNew(b.dataset.openNew));
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  if (!$('#buildlog').hidden) $('#bl-close').click();
  else if (!$('#modal').hidden) closeNew();
  else if (state.selected) closeDrawer();
});

(async function main() {
  const session = await fetch('/v1/session')
    .then((r) => r.json())
    .catch(() => ({}));
  if (session.tokenRequired && !session.authenticated) {
    showLogin();
    if (new URLSearchParams(location.search).get('login') === 'failed')
      toast('That token was not accepted.', true);
    return;
  }
  await refresh();
  refreshSlow();
  setInterval(refresh, 2500);
  setInterval(refreshSlow, 10000);
  startActivity();
})();
