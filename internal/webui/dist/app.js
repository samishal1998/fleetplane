/* Fleetplane dashboard — a single-file Vue 3 app (vendored global build,
   no build step). Talks only to the same-origin /v1 API with a pasted
   bearer token. */
'use strict';
(function () {
  const { createApp, reactive } = Vue;

  // ------------------------------------------------------------------ state

  const store = reactive({
    token: localStorage.getItem('fp.token') || '',
    authState: 'checking', // checking | login | ready
    authError: '',
    route: parseHash(),
    counts: { resources: null, uncertain: 0 },
    theme: localStorage.getItem('fp.theme') || 'system',
  });

  function parseHash() {
    const h = (location.hash || '#/overview').replace(/^#\/?/, '');
    const parts = h.split('/').filter(Boolean);
    return { view: parts[0] || 'overview', id: parts.slice(1).join('/') || '' };
  }
  window.addEventListener('hashchange', () => { store.route = parseHash(); });
  function nav(path) { location.hash = path; }

  // ------------------------------------------------------------------ api

  class ApiError extends Error {
    constructor(status, code, message, retryable) {
      super(message);
      this.status = status; this.code = code; this.retryable = retryable;
    }
  }

  async function api(method, path, body, extraHeaders) {
    const headers = Object.assign({}, extraHeaders || {});
    if (store.token) headers['Authorization'] = 'Bearer ' + store.token;
    const init = { method, headers };
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    let res;
    try { res = await fetch(path, init); } catch (e) {
      throw new ApiError(0, 'network', 'cannot reach the server', true);
    }
    let data = null;
    const text = await res.text();
    if (text) { try { data = JSON.parse(text); } catch (e) { /* non-JSON */ } }
    if (!res.ok) {
      const err = (data && data.error) || {};
      if (res.status === 401 && store.authState === 'ready') store.authState = 'login';
      throw new ApiError(res.status, err.code || 'error',
        err.message || (res.status + ' ' + res.statusText), !!err.retryable);
    }
    return data;
  }

  // ------------------------------------------------------------------ helpers

  const toasts = reactive([]);
  let toastSeq = 0;
  function toast(msg, opts) {
    const t = Object.assign({ id: ++toastSeq, msg: msg, err: false, code: '' }, opts || {});
    toasts.push(t);
    setTimeout(() => {
      const i = toasts.findIndex((x) => x.id === t.id);
      if (i >= 0) toasts.splice(i, 1);
    }, t.err ? 7000 : 3500);
  }
  function toastErr(e) { toast(e.message || String(e), { err: true, code: e.code || '' }); }

  function uuid() {
    return (crypto.randomUUID && crypto.randomUUID()) ||
      'xxxx-xxxx-xxxx'.replace(/x/g, () => Math.floor(Math.random() * 16).toString(16));
  }

  function timeAgo(iso) {
    if (!iso) return '';
    const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
    if (isNaN(s)) return '';
    if (s < 0) return 'now';
    if (s < 60) return s + 's ago';
    if (s < 3600) return Math.floor(s / 60) + 'm ago';
    if (s < 86400) return Math.floor(s / 3600) + 'h ago';
    return Math.floor(s / 86400) + 'd ago';
  }
  function fmtTime(iso) { return iso ? new Date(iso).toLocaleString() : ''; }
  function pretty(v) {
    if (v === undefined || v === null || v === '') return '';
    if (typeof v === 'string') { try { v = JSON.parse(v); } catch (e) { return v; } }
    return JSON.stringify(v, null, 2);
  }
  function parseLabels(text) {
    const out = {};
    for (const line of (text || '').split('\n')) {
      const l = line.trim();
      if (!l) continue;
      const i = l.indexOf('=');
      if (i <= 0) throw new Error('labels must be key=value per line: "' + l + '"');
      out[l.slice(0, i).trim()] = l.slice(i + 1).trim();
    }
    return out;
  }

  // Fixed phase → color-token mapping (identity is always color + text).
  const PHASES = ['requested', 'provisioning', 'ready', 'allocated', 'draining', 'deleting', 'failed', 'orphaned'];
  function phaseVar(p) { return PHASES.includes(p) ? '--ph-' + p : '--muted'; }
  function opToneVar(s) {
    if (s === 'succeeded') return '--st-good';
    if (s === 'failed' || s === 'aborted') return '--st-critical';
    if (s === 'uncertain') return '--st-serious';
    if (s === 'verifying') return '--st-warn';
    return '--ph-requested'; // journaled / in_flight / external_accepted: in progress
  }
  function healthToneVar(s) {
    if (s === 'healthy') return '--st-good';
    if (s === 'degraded') return '--st-warn';
    if (s === 'unavailable') return '--st-critical';
    return '--muted';
  }
  const OP_ACTIVE = ['journaled', 'in_flight', 'external_accepted', 'verifying', 'uncertain'];

  function rememberAcq(id) {
    const l = JSON.parse(localStorage.getItem('fp.acqs') || '[]');
    if (!l.includes(id)) l.unshift(id);
    localStorage.setItem('fp.acqs', JSON.stringify(l.slice(0, 50)));
  }
  function rememberedAcqs() { return JSON.parse(localStorage.getItem('fp.acqs') || '[]'); }

  function loadErr(e) {
    if (e.status === 403) return 'This token lacks the permission needed for this view (' + e.message + ').';
    if (e.status === 0) return 'Cannot reach the server.';
    return e.message;
  }

  // Polling mixin: refresh every 2.5s while the tab is visible.
  const polls = {
    data() { return { err: '', loading: true, _timer: null }; },
    mounted() {
      this.load();
      this._timer = setInterval(() => { if (!document.hidden) this.load(true); }, 2500);
    },
    beforeUnmount() { clearInterval(this._timer); },
  };

  // ------------------------------------------------------------------ app

  const app = createApp({
    data() { return { store: store, toasts: toasts }; },
    computed: {
      viewComp() {
        const r = this.store.route;
        const map = {
          overview: 'overview-view',
          resources: r.id ? 'resource-detail' : 'resources-view',
          pools: r.id ? 'pool-detail' : 'pools-view',
          acquisitions: r.id ? 'acq-detail' : 'acqs-view',
          operations: 'ops-view',
          events: 'events-view',
          providers: 'providers-view',
        };
        return map[r.view] || 'overview-view';
      },
      navItems() {
        return [
          { key: 'overview', label: 'Overview', icon: '◈' },
          { key: 'resources', label: 'Resources', icon: '▣', count: this.store.counts.resources },
          { key: 'pools', label: 'Pools', icon: '⬡' },
          { key: 'acquisitions', label: 'Acquisitions', icon: '⇋' },
          { key: 'operations', label: 'Operations', icon: '≣', alert: this.store.counts.uncertain },
          { key: 'events', label: 'Events', icon: '☰' },
          { key: 'providers', label: 'Providers', icon: '☁' },
        ];
      },
    },
    methods: {
      nav: nav,
      setTheme(t) {
        this.store.theme = t;
        if (t === 'system') { delete document.documentElement.dataset.theme; localStorage.removeItem('fp.theme'); }
        else { document.documentElement.dataset.theme = t; localStorage.setItem('fp.theme', t); }
      },
      signOut() {
        this.store.token = '';
        localStorage.removeItem('fp.token');
        this.store.authState = 'login';
      },
      async pollCounts() {
        if (this.store.authState !== 'ready') return;
        try {
          const rs = await api('GET', '/v1/resources');
          this.store.counts.resources = (rs.items || []).filter((r) => !r.metadata.deletedAt).length;
        } catch (e) { /* sidebar count is best-effort */ }
        try {
          const ops = await api('GET', '/v1/operations');
          this.store.counts.uncertain = (ops.items || []).filter((o) => o.state === 'uncertain').length;
        } catch (e) { /* best-effort */ }
      },
    },
    async mounted() {
      try {
        await api('GET', '/v1/resources');
        store.authState = 'ready';
      } catch (e) {
        if (e.status === 403) store.authState = 'ready';
        else { store.authState = 'login'; if (e.status !== 401) store.authError = e.message; }
      }
      this.pollCounts();
      setInterval(() => { if (!document.hidden) this.pollCounts(); }, 5000);
    },
    template: `
    <login-view v-if="store.authState !== 'ready'"></login-view>
    <div v-else class="shell">
      <aside class="sidebar">
        <div class="brand"><img src="logo.svg" alt=""> fleetplane</div>
        <button v-for="it in navItems" :key="it.key" class="nav-item"
                :class="{active: store.route.view === it.key}" @click="nav('/' + it.key)">
          <span aria-hidden="true">{{ it.icon }}</span> {{ it.label }}
          <span v-if="it.count !== undefined && it.count !== null" class="n-count">{{ it.count }}</span>
          <span v-if="it.alert" class="n-alert badge" :style="{color: 'var(--st-serious)', borderColor: 'var(--st-serious)'}"
                title="uncertain operations">⚠ {{ it.alert }}</span>
        </button>
        <div class="spacer"></div>
        <div class="foot">
          <div class="pill-select" role="group" aria-label="Theme">
            <button :class="{on: store.theme === 'system'}" @click="setTheme('system')">Auto</button>
            <button :class="{on: store.theme === 'light'}" @click="setTheme('light')">Light</button>
            <button :class="{on: store.theme === 'dark'}" @click="setTheme('dark')">Dark</button>
          </div>
          <button v-if="store.token" class="btn sm" @click="signOut">Sign out</button>
        </div>
      </aside>
      <main class="main"><component :is="viewComp" :key="store.route.view + ':' + store.route.id"></component></main>
    </div>
    <div class="toasts">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="{err: t.err}">
        {{ t.msg }} <div v-if="t.code" class="t-code">{{ t.code }}</div>
      </div>
    </div>`,
  });

  app.config.globalProperties.$api = api;
  app.config.globalProperties.$nav = nav;
  app.config.globalProperties.$toast = toast;
  app.config.globalProperties.$toastErr = toastErr;
  app.config.globalProperties.$ago = timeAgo;
  app.config.globalProperties.$time = fmtTime;
  app.config.globalProperties.$pretty = pretty;
  app.config.globalProperties.$store = store;

  // ------------------------------------------------------------------ shared components

  app.component('phase-badge', {
    props: ['phase'],
    template: `<span class="badge"><span class="dot" :style="{background: 'var(' + v + ')'}"></span>{{ phase }}</span>`,
    computed: { v() { return phaseVar(this.phase); } },
  });

  app.component('op-badge', {
    props: ['state'],
    template: `<span class="badge"><span class="dot" :style="{background: 'var(' + v + ')'}"></span>{{ state === 'uncertain' ? '⚠ ' : '' }}{{ state }}</span>`,
    computed: { v() { return opToneVar(this.state); } },
  });

  app.component('health-badge', {
    props: ['state'],
    template: `<span class="badge"><span class="dot" :style="{background: 'var(' + v + ')'}"></span>{{ state }}</span>`,
    computed: { v() { return healthToneVar(this.state); } },
  });

  app.component('modal-box', {
    props: ['title'],
    emits: ['close'],
    mounted() {
      this._esc = (e) => { if (e.key === 'Escape') this.$emit('close'); };
      document.addEventListener('keydown', this._esc);
    },
    beforeUnmount() { document.removeEventListener('keydown', this._esc); },
    template: `
    <div class="overlay" @click.self="$emit('close')">
      <div class="modal" role="dialog" :aria-label="title">
        <h3>{{ title }}</h3><slot></slot>
      </div>
    </div>`,
  });

  app.component('json-view', {
    props: ['value'],
    template: `<pre v-if="text" class="jsonblock"><code>{{ text }}</code></pre><span v-else class="muted">—</span>`,
    computed: { text() { return pretty(this.value); } },
  });

  // ------------------------------------------------------------------ login

  app.component('login-view', {
    data() { return { store: store, input: store.token, busy: false, err: store.authError }; },
    methods: {
      async connect() {
        this.busy = true; this.err = '';
        store.token = this.input.trim();
        try {
          await api('GET', '/v1/resources');
          this.done();
        } catch (e) {
          if (e.status === 403) { this.done(); return; } // authenticated, limited perms
          this.err = e.status === 401 ? 'Invalid token.' : e.message;
          this.busy = false;
        }
      },
      done() {
        if (store.token) localStorage.setItem('fp.token', store.token);
        store.authState = 'ready';
      },
    },
    template: `
    <div class="login-wrap">
      <form class="login" @submit.prevent="connect">
        <div class="brand"><img src="logo.svg" alt=""> fleetplane</div>
        <p class="muted small" style="margin-top:0">Paste an API token (<span class="mono">flp_…</span>).
          Leave empty if the server runs with no tokens configured.</p>
        <label class="f" for="tok">API token</label>
        <input id="tok" type="password" v-model="input" placeholder="flp_xxxxxxxx.…" autocomplete="off">
        <div v-if="err" class="field-err">{{ err }}</div>
        <div class="mt"><button class="btn primary" type="submit" :disabled="busy">
          <span v-if="busy" class="wip"></span> Connect</button></div>
      </form>
    </div>`,
  });

  // ------------------------------------------------------------------ overview

  app.component('overview-view', {
    mixins: [polls],
    data() { return { res: [], ops: [], pools: [], providers: [], events: [], errs: {} }; },
    computed: {
      live() { return this.res.filter((r) => !r.metadata.deletedAt); },
      byPhase() {
        const m = {};
        for (const p of PHASES) m[p] = 0;
        for (const r of this.live) if (m[r.status.phase] !== undefined) m[r.status.phase]++;
        return m;
      },
      distSegs() {
        return PHASES.map((p) => ({ p: p, n: this.byPhase[p], v: phaseVar(p) })).filter((s) => s.n > 0);
      },
      activeOps() { return this.ops.filter((o) => OP_ACTIVE.includes(o.state)); },
      uncertainOps() { return this.ops.filter((o) => o.state === 'uncertain'); },
      healthyProviders() { return this.providers.filter((p) => p.state === 'healthy').length; },
      recentEvents() { return this.events.slice().sort((a, b) => (a.ts < b.ts ? 1 : -1)).slice(0, 10); },
    },
    methods: {
      async load() {
        const get = async (key, path) => {
          try { const d = await api('GET', path); this.errs[key] = ''; return d; }
          catch (e) { this.errs[key] = loadErr(e); return null; }
        };
        const [rs, ops, pools, prov, ev] = await Promise.all([
          get('res', '/v1/resources'), get('ops', '/v1/operations'),
          get('pools', '/v1/pools'), get('prov', '/v1/providers'),
          get('ev', '/v1/events?limit=50'),
        ]);
        if (rs) this.res = rs.items || [];
        if (ops) this.ops = ops.items || [];
        if (pools) this.pools = pools.items || [];
        if (prov) this.providers = prov.items || [];
        if (ev) this.events = ev.items || [];
        this.loading = false;
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Overview</h1><div class="grow"></div><span v-if="loading" class="wip"></span></div>
      <div class="tiles">
        <div class="tile"><div class="t-label">Resources</div><div class="t-value">{{ live.length }}</div>
          <div class="t-sub">{{ byPhase.ready }} ready · {{ byPhase.allocated }} allocated</div></div>
        <div class="tile"><div class="t-label">Provisioning</div><div class="t-value">{{ byPhase.provisioning + byPhase.requested }}</div></div>
        <div class="tile"><div class="t-label">Active operations</div><div class="t-value">{{ activeOps.length }}</div></div>
        <div class="tile" :class="{alert: uncertainOps.length > 0}">
          <div class="t-label">{{ uncertainOps.length ? '⚠ Uncertain operations' : 'Uncertain operations' }}</div>
          <div class="t-value">{{ uncertainOps.length }}</div>
          <div v-if="uncertainOps.length" class="t-sub"><a href="#/operations">resolve →</a></div></div>
        <div class="tile"><div class="t-label">Pools</div><div class="t-value">{{ pools.length }}</div></div>
        <div class="tile"><div class="t-label">Providers healthy</div><div class="t-value">{{ healthyProviders }}<span class="muted">/{{ providers.length }}</span></div></div>
      </div>

      <div class="card section">
        <h2>Fleet by phase</h2>
        <div v-if="errs.res" class="err-inline">{{ errs.res }}</div>
        <template v-else-if="live.length">
          <div class="distbar" role="img" :aria-label="'phase distribution of ' + live.length + ' resources'">
            <div v-for="s in distSegs" :key="s.p" class="seg" :style="{flexGrow: s.n, background: 'var(' + s.v + ')'}"
                 :title="s.p + ': ' + s.n"></div>
          </div>
          <div class="dist-legend">
            <span v-for="s in distSegs" :key="s.p" class="li">
              <span class="dot" :style="{background: 'var(' + s.v + ')'}"></span>{{ s.p }} · {{ s.n }}</span>
          </div>
        </template>
        <div v-else class="empty">No resources yet — create one from the Resources page.</div>
      </div>

      <div class="grid2 section">
        <div class="card">
          <h2>Providers</h2>
          <div v-if="errs.prov" class="err-inline">{{ errs.prov }}</div>
          <div v-else-if="!providers.length" class="empty">No provider instances configured.</div>
          <div v-else class="tbl-wrap"><table class="tbl"><tbody>
            <tr v-for="p in providers" :key="p.instance">
              <td><strong>{{ p.instance }}</strong> <span class="sub">({{ p.driver }})</span></td>
              <td><health-badge :state="p.state"></health-badge></td>
              <td class="sub">{{ p.lastError || '' }}</td>
            </tr></tbody></table></div>
        </div>
        <div class="card">
          <h2>Recent events</h2>
          <div v-if="errs.ev" class="err-inline">{{ errs.ev }}</div>
          <div v-else-if="!recentEvents.length" class="empty">No events yet.</div>
          <div v-else class="tbl-wrap"><table class="tbl"><tbody>
            <tr v-for="e in recentEvents" :key="e.id">
              <td class="sub" :title="$time(e.ts)">{{ $ago(e.ts) }}</td>
              <td>{{ e.type }}<span v-if="e.outcome" class="sub"> · {{ e.outcome }}</span></td>
              <td class="id"><a v-if="e.resourceId" :href="'#/resources/' + e.resourceId">{{ e.resourceId }}</a></td>
            </tr></tbody></table></div>
        </div>
      </div>
    </div>`,
  });

  // ------------------------------------------------------------------ resources

  const MACHINE_TMPL = '{\n  "serverType": "cx22",\n  "image": "name:ubuntu-24.04"\n}';
  const VOLUME_TMPL = '{\n  "sizeGiB": 100\n}';

  app.component('resources-view', {
    mixins: [polls],
    data() {
      return {
        items: [], providers: [], showCreate: false, busy: false, formErr: '',
        form: { name: '', kind: 'compute.machine', provider: '', spec: MACHINE_TMPL, labels: '' },
        idem: uuid(),
      };
    },
    computed: { live() { return this.items.filter((r) => !r.metadata.deletedAt); } },
    watch: {
      'form.kind'(k) {
        if (this.form.spec === MACHINE_TMPL || this.form.spec === VOLUME_TMPL || !this.form.spec.trim()) {
          this.form.spec = k === 'storage.volume' ? VOLUME_TMPL : MACHINE_TMPL;
        }
      },
    },
    methods: {
      async load() {
        try {
          const d = await api('GET', '/v1/resources');
          this.items = d.items || [];
          this.err = '';
        } catch (e) { this.err = loadErr(e); }
        try {
          const p = await api('GET', '/v1/providers');
          this.providers = p.items || [];
          if (!this.form.provider && this.providers.length) this.form.provider = this.providers[0].instance;
        } catch (e) { /* provider dropdown degrades to free text */ }
        this.loading = false;
      },
      openCreate() { this.showCreate = true; this.formErr = ''; this.idem = uuid(); },
      async create() {
        this.formErr = '';
        let spec, labels;
        try { spec = JSON.parse(this.form.spec); } catch (e) { this.formErr = 'spec: ' + e.message; return; }
        try { labels = parseLabels(this.form.labels); } catch (e) { this.formErr = e.message; return; }
        this.busy = true;
        try {
          const body = {
            apiVersion: 'fleetplane.io/v1alpha1', kind: 'Resource',
            metadata: { id: '', name: this.form.name, labels: labels },
            spec: { kind: this.form.kind, provider: this.form.provider, machine: spec },
          };
          if (!Object.keys(labels).length) delete body.metadata.labels;
          const res = await api('POST', '/v1/resources', body, { 'Idempotency-Key': this.idem });
          toast('Resource ' + res.metadata.id + ' creating');
          this.showCreate = false;
          nav('/resources/' + res.metadata.id);
        } catch (e) { this.formErr = e.message; } finally { this.busy = false; }
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Resources</h1><div class="grow"></div>
        <button class="btn primary" @click="openCreate">New resource</button></div>
      <div class="card">
        <div v-if="err" class="err-inline">{{ err }}</div>
        <div v-else-if="!live.length && !loading" class="empty">No resources. Create one, declare a pool, or acquire capacity.</div>
        <div v-else class="tbl-wrap"><table class="tbl">
          <thead><tr><th>Name / ID</th><th>Kind</th><th>Class</th><th>Provider</th><th>Phase</th><th>External ID</th><th>Age</th></tr></thead>
          <tbody><tr v-for="r in live" :key="r.metadata.id" class="rowlink" @click="$nav('/resources/' + r.metadata.id)">
            <td><strong>{{ r.metadata.name || '—' }}</strong><div class="id sub">{{ r.metadata.id }}</div></td>
            <td>{{ r.spec.kind }}</td>
            <td>{{ r.spec.class || '—' }}</td>
            <td>{{ r.spec.provider }}</td>
            <td><phase-badge :phase="r.status.phase"></phase-badge></td>
            <td class="id">{{ r.status.externalId || '—' }}</td>
            <td class="sub" :title="$time(r.metadata.createdAt)">{{ $ago(r.metadata.createdAt) }}</td>
          </tr></tbody></table></div>
      </div>

      <modal-box v-if="showCreate" title="New resource" @close="showCreate = false">
        <form @submit.prevent="create">
          <label class="f" for="r-name">Name</label>
          <input id="r-name" type="text" v-model="form.name" placeholder="ci-runner-1">
          <label class="f" for="r-kind">Kind</label>
          <select id="r-kind" v-model="form.kind">
            <option>compute.machine</option><option>storage.volume</option>
          </select>
          <label class="f" for="r-prov">Provider instance</label>
          <select v-if="providers.length" id="r-prov" v-model="form.provider">
            <option v-for="p in providers" :key="p.instance" :value="p.instance">{{ p.instance }} ({{ p.driver }})</option>
          </select>
          <input v-else id="r-prov" type="text" v-model="form.provider" placeholder="hetzner">
          <label class="f" for="r-spec">Spec (JSON)</label>
          <textarea id="r-spec" v-model="form.spec" spellcheck="false"></textarea>
          <div class="hint">compute.machine: serverType, image (id:&lt;n&gt; | name:&lt;os&gt; | snapshot:&lt;selector&gt;), location, userData, readiness. storage.volume: sizeGiB, zone, filesystem.</div>
          <label class="f" for="r-labels">Labels (key=value per line)</label>
          <textarea id="r-labels" v-model="form.labels" style="min-height:56px" spellcheck="false"></textarea>
          <div v-if="formErr" class="field-err">{{ formErr }}</div>
          <div class="actions">
            <button type="button" class="btn" @click="showCreate = false">Cancel</button>
            <button type="submit" class="btn primary" :disabled="busy"><span v-if="busy" class="wip"></span> Create</button>
          </div>
        </form>
      </modal-box>
    </div>`,
  });

  app.component('resource-detail', {
    mixins: [polls],
    data() {
      return {
        id: store.route.id, r: null, ops: [], events: [],
        confirmDelete: false, dryRun: null, busy: false, idem: uuid(),
      };
    },
    methods: {
      async load() {
        try {
          this.r = await api('GET', '/v1/resources/' + this.id);
          this.err = '';
        } catch (e) { this.err = loadErr(e); this.loading = false; return; }
        try {
          const ops = await api('GET', '/v1/operations');
          this.ops = (ops.items || []).filter((o) => o.resourceId === this.id);
        } catch (e) { /* section is optional */ }
        try {
          const ev = await api('GET', '/v1/events?limit=200');
          this.events = (ev.items || []).filter((e2) => e2.resourceId === this.id)
            .sort((a, b) => (a.ts < b.ts ? 1 : -1)).slice(0, 20);
        } catch (e) { /* optional */ }
        this.loading = false;
      },
      async askDelete() {
        this.busy = true;
        try {
          this.dryRun = await api('DELETE', '/v1/resources/' + this.id + '?dryRun=true');
          this.confirmDelete = true;
          this.idem = uuid();
        } catch (e) { toastErr(e); } finally { this.busy = false; }
      },
      async doDelete() {
        this.busy = true;
        try {
          await api('DELETE', '/v1/resources/' + this.id, undefined, { 'Idempotency-Key': this.idem });
          toast('Deletion journaled for ' + this.id);
          this.confirmDelete = false;
        } catch (e) { toastErr(e); } finally { this.busy = false; }
      },
      async drain() {
        try {
          await api('POST', '/v1/resources/' + this.id + ':drain');
          toast('Draining ' + this.id);
        } catch (e) { toastErr(e); }
      },
    },
    template: `
    <div>
      <div class="topbar">
        <h1><a href="#/resources">Resources</a> / <span class="mono">{{ id }}</span></h1>
        <div class="grow"></div>
        <template v-if="r && !r.metadata.deletedAt">
          <button class="btn" @click="drain" :disabled="busy">Drain</button>
          <button class="btn danger" @click="askDelete" :disabled="busy || r.metadata.protected"
                  :title="r.metadata.protected ? 'delete-protected' : ''">Delete…</button>
        </template>
      </div>
      <div v-if="err" class="card err-inline">{{ err }}</div>
      <template v-else-if="r">
        <div class="card">
          <dl class="kv">
            <dt>Name</dt><dd>{{ r.metadata.name || '—' }}</dd>
            <dt>Phase</dt><dd><phase-badge :phase="r.status.phase"></phase-badge>
              <span v-if="r.metadata.deletedAt" class="badge" style="margin-left:6px">deleted {{ $ago(r.metadata.deletedAt) }}</span></dd>
            <dt>Kind</dt><dd>{{ r.spec.kind }}</dd>
            <dt>Provider</dt><dd>{{ r.spec.provider }}</dd>
            <dt>Class</dt><dd>{{ r.spec.class || '—' }}</dd>
            <dt>Ownership</dt><dd>{{ r.metadata.ownership }}<span v-if="r.metadata.protected"> · 🔒 protected</span></dd>
            <dt>External ID</dt><dd class="mono">{{ r.status.externalId || '—' }}</dd>
            <dt>Created</dt><dd>{{ $time(r.metadata.createdAt) }} ({{ $ago(r.metadata.createdAt) }})</dd>
            <dt>Updated</dt><dd>{{ $time(r.metadata.updatedAt) }}</dd>
            <dt>Generation</dt><dd>{{ r.metadata.generation }} (observed {{ r.metadata.observedGeneration }})</dd>
          </dl>
        </div>
        <div class="grid2 section">
          <div class="card"><h2>Spec</h2><json-view :value="r.spec.machine"></json-view>
            <template v-if="r.metadata.labels"><h2 class="mt">Labels</h2><json-view :value="r.metadata.labels"></json-view></template></div>
          <div class="card"><h2>Capacity</h2><json-view :value="r.status.capacity"></json-view>
            <h2 class="mt">Provider extensions</h2><json-view :value="r.status.extensions"></json-view></div>
        </div>
        <div class="card section">
          <h2>Open operations</h2>
          <div v-if="!ops.length" class="empty">No open operations — completed ones appear in events below.</div>
          <div v-else class="tbl-wrap"><table class="tbl">
            <thead><tr><th>ID</th><th>Kind</th><th>State</th><th>Attempt</th><th>Error class</th><th>Updated</th></tr></thead>
            <tbody><tr v-for="o in ops" :key="o.id">
              <td class="id">{{ o.id }}</td><td>{{ o.kind }}</td>
              <td><op-badge :state="o.state"></op-badge></td>
              <td class="num">{{ o.attempt }}</td><td>{{ o.errorClass || '—' }}</td>
              <td class="sub" :title="$time(o.updatedAt)">{{ $ago(o.updatedAt) }}</td>
            </tr></tbody></table></div>
        </div>
        <div class="card section">
          <h2>Events</h2>
          <div v-if="!events.length" class="empty">No events for this resource in the recent window.</div>
          <div v-else class="tbl-wrap"><table class="tbl"><tbody>
            <tr v-for="e in events" :key="e.id">
              <td class="sub" :title="$time(e.ts)">{{ $ago(e.ts) }}</td>
              <td>{{ e.type }}</td><td>{{ e.outcome }}</td><td class="sub">{{ e.actor }}</td>
            </tr></tbody></table></div>
        </div>
      </template>

      <modal-box v-if="confirmDelete" title="Delete resource" @close="confirmDelete = false">
        <p>The server confirms this delete would be accepted:</p>
        <json-view :value="dryRun"></json-view>
        <p class="small muted">Deletion is journaled and executed by the operation engine; gates
        (active leases, uncertain operations, ownership) are re-checked at journal time.</p>
        <div class="actions">
          <button class="btn" @click="confirmDelete = false">Cancel</button>
          <button class="btn danger" @click="doDelete" :disabled="busy"><span v-if="busy" class="wip"></span> Delete {{ id }}</button>
        </div>
      </modal-box>
    </div>`,
  });

  // ------------------------------------------------------------------ pools

  const POOL_TMPL = '{\n  "class": "ci",\n  "replicas": 2,\n  "minReady": 1\n}';

  app.component('pools-view', {
    mixins: [polls],
    data() {
      return { items: [], showCreate: false, busy: false, formErr: '', form: { name: '', spec: POOL_TMPL } };
    },
    methods: {
      specOf(p) { try { return typeof p.spec === 'string' ? JSON.parse(p.spec) : p.spec; } catch (e) { return {}; } },
      async load() {
        try { const d = await api('GET', '/v1/pools'); this.items = d.items || []; this.err = ''; }
        catch (e) { this.err = loadErr(e); }
        this.loading = false;
      },
      async create() {
        this.formErr = '';
        let spec;
        try { spec = JSON.parse(this.form.spec); } catch (e) { this.formErr = 'spec: ' + e.message; return; }
        this.busy = true;
        try {
          const p = await api('POST', '/v1/pools', { metadata: { id: '', name: this.form.name }, spec: spec });
          toast('Pool ' + p.metadata.id + ' created');
          this.showCreate = false;
          nav('/pools/' + p.metadata.id);
        } catch (e) { this.formErr = e.message; } finally { this.busy = false; }
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Pools</h1><div class="grow"></div>
        <button class="btn primary" @click="showCreate = true; formErr = ''">New pool</button></div>
      <div class="card">
        <div v-if="err" class="err-inline">{{ err }}</div>
        <div v-else-if="!items.length && !loading" class="empty">No pools declared.</div>
        <div v-else class="tbl-wrap"><table class="tbl">
          <thead><tr><th>Name / ID</th><th>Class</th><th>Replicas</th><th>Min ready</th><th>Paused</th><th>Updated</th></tr></thead>
          <tbody><tr v-for="p in items" :key="p.metadata.id" class="rowlink" @click="$nav('/pools/' + p.metadata.id)">
            <td><strong>{{ p.metadata.name }}</strong><div class="id sub">{{ p.metadata.id }}</div></td>
            <td>{{ specOf(p).class || (specOf(p).kind || '—') }}</td>
            <td class="num">{{ specOf(p).replicas }}</td>
            <td class="num">{{ specOf(p).minReady || 0 }}</td>
            <td>{{ p.paused ? 'yes' : 'no' }}</td>
            <td class="sub" :title="$time(p.metadata.updatedAt)">{{ $ago(p.metadata.updatedAt) }}</td>
          </tr></tbody></table></div>
      </div>

      <modal-box v-if="showCreate" title="New pool" @close="showCreate = false">
        <form @submit.prevent="create">
          <label class="f" for="p-name">Name</label>
          <input id="p-name" type="text" v-model="form.name" placeholder="ci-pool" required>
          <label class="f" for="p-spec">Spec (JSON)</label>
          <textarea id="p-spec" v-model="form.spec" spellcheck="false" style="min-height:150px"></textarea>
          <div class="hint">Fields: class, replicas, minReady, maxResources, reclaim.idleAfter — or inline kind/provider/machine instead of class.</div>
          <div v-if="formErr" class="field-err">{{ formErr }}</div>
          <div class="actions">
            <button type="button" class="btn" @click="showCreate = false">Cancel</button>
            <button type="submit" class="btn primary" :disabled="busy"><span v-if="busy" class="wip"></span> Create</button>
          </div>
        </form>
      </modal-box>
    </div>`,
  });

  app.component('pool-detail', {
    mixins: [polls],
    data() {
      return { id: store.route.id, p: null, members: [], showEdit: false, editSpec: '', formErr: '', busy: false };
    },
    computed: {
      spec() { if (!this.p) return {}; try { return typeof this.p.spec === 'string' ? JSON.parse(this.p.spec) : this.p.spec; } catch (e) { return {}; } },
    },
    methods: {
      async load() {
        try { this.p = await api('GET', '/v1/pools/' + this.id); this.err = ''; }
        catch (e) { this.err = loadErr(e); this.loading = false; return; }
        if (this.spec.class) {
          try {
            const d = await api('GET', '/v1/resources?class=' + encodeURIComponent(this.spec.class));
            this.members = (d.items || []).filter((r) => !r.metadata.deletedAt);
          } catch (e) { /* optional */ }
        }
        this.loading = false;
      },
      openEdit() {
        this.editSpec = JSON.stringify(this.spec, null, 2);
        this.formErr = ''; this.showEdit = true;
      },
      async saveSpec(spec) {
        this.busy = true;
        try {
          await api('PUT', '/v1/pools/' + this.id, { metadata: { id: this.id, name: this.p.metadata.name }, spec: spec });
          toast('Pool updated');
          this.showEdit = false;
          this.load(true);
        } catch (e) { this.formErr = e.message; toastErr(e); } finally { this.busy = false; }
      },
      async submitEdit() {
        this.formErr = '';
        let spec;
        try { spec = JSON.parse(this.editSpec); } catch (e) { this.formErr = 'spec: ' + e.message; return; }
        await this.saveSpec(spec);
      },
      async scale(delta) {
        const s = Object.assign({}, this.spec);
        s.replicas = Math.max(0, (s.replicas || 0) + delta);
        await this.saveSpec(s);
      },
      async reconcile() {
        try { await api('POST', '/v1/pools/' + this.id + ':reconcile'); toast('Reconcile kicked'); }
        catch (e) { toastErr(e); }
      },
    },
    template: `
    <div>
      <div class="topbar">
        <h1><a href="#/pools">Pools</a> / {{ p ? p.metadata.name : id }}</h1>
        <div class="grow"></div>
        <button class="btn" @click="reconcile">Reconcile now</button>
        <button class="btn" @click="openEdit">Edit spec</button>
      </div>
      <div v-if="err" class="card err-inline">{{ err }}</div>
      <template v-else-if="p">
        <div class="tiles">
          <div class="tile"><div class="t-label">Desired replicas</div><div class="t-value">{{ spec.replicas || 0 }}</div>
            <div class="btn-row mt"><button class="btn sm" @click="scale(-1)" :disabled="busy || !spec.replicas">−</button>
              <button class="btn sm" @click="scale(1)" :disabled="busy">+</button></div></div>
          <div class="tile"><div class="t-label">In class</div><div class="t-value">{{ members.length }}</div>
            <div class="t-sub">{{ spec.class ? 'class ' + spec.class : 'inline spec' }}</div></div>
          <div class="tile"><div class="t-label">Paused</div><div class="t-value">{{ p.paused ? 'yes' : 'no' }}</div></div>
          <div class="tile"><div class="t-label">Generation</div><div class="t-value">{{ p.metadata.generation }}</div>
            <div class="t-sub">observed {{ p.metadata.observedGeneration }}</div></div>
        </div>
        <div class="card section"><h2>Spec</h2><json-view :value="p.spec"></json-view></div>
        <div class="card section">
          <h2>Resources in this class</h2>
          <div v-if="!spec.class" class="empty">Pool uses an inline spec; member listing by class is unavailable.</div>
          <div v-else-if="!members.length" class="empty">No live resources in class {{ spec.class }}.</div>
          <div v-else class="tbl-wrap"><table class="tbl">
            <thead><tr><th>Name / ID</th><th>Phase</th><th>Provider</th><th>External ID</th><th>Age</th></tr></thead>
            <tbody><tr v-for="r in members" :key="r.metadata.id" class="rowlink" @click="$nav('/resources/' + r.metadata.id)">
              <td><strong>{{ r.metadata.name || '—' }}</strong><div class="id sub">{{ r.metadata.id }}</div></td>
              <td><phase-badge :phase="r.status.phase"></phase-badge></td>
              <td>{{ r.spec.provider }}</td>
              <td class="id">{{ r.status.externalId || '—' }}</td>
              <td class="sub">{{ $ago(r.metadata.createdAt) }}</td>
            </tr></tbody></table></div>
        </div>
      </template>

      <modal-box v-if="showEdit" title="Edit pool spec" @close="showEdit = false">
        <form @submit.prevent="submitEdit">
          <textarea v-model="editSpec" spellcheck="false" style="min-height:180px"></textarea>
          <div v-if="formErr" class="field-err">{{ formErr }}</div>
          <div class="actions">
            <button type="button" class="btn" @click="showEdit = false">Cancel</button>
            <button type="submit" class="btn primary" :disabled="busy"><span v-if="busy" class="wip"></span> Save</button>
          </div>
        </form>
      </modal-box>
    </div>`,
  });

  // ------------------------------------------------------------------ acquisitions

  app.component('acqs-view', {
    mixins: [polls],
    data() {
      return {
        acqs: [], lookup: '', showAcquire: false, busy: false, formErr: '',
        form: { class: '', kind: '', exclusive: false, ttl: '', constraints: '' },
        idem: uuid(),
      };
    },
    methods: {
      async load() {
        const ids = rememberedAcqs();
        const out = [];
        for (const id of ids.slice(0, 20)) {
          try { out.push(await api('GET', '/v1/acquisitions/' + id)); }
          catch (e) { if (e.status === 403) { this.err = loadErr(e); break; } }
        }
        this.acqs = out;
        this.loading = false;
      },
      go() { if (this.lookup.trim()) nav('/acquisitions/' + this.lookup.trim()); },
      openAcquire() { this.showAcquire = true; this.formErr = ''; this.idem = uuid(); },
      async acquire() {
        this.formErr = '';
        const body = {};
        if (this.form.class) body.class = this.form.class;
        if (this.form.kind) body.kind = this.form.kind;
        if (this.form.exclusive) body.exclusive = true;
        if (this.form.ttl) body.lease = { ttl: this.form.ttl };
        if (this.form.constraints.trim()) {
          try { body.constraints = JSON.parse(this.form.constraints); }
          catch (e) { this.formErr = 'constraints: ' + e.message; return; }
        }
        this.busy = true;
        try {
          const a = await api('POST', '/v1/acquisitions', body, { 'Idempotency-Key': this.idem });
          rememberAcq(a.id);
          toast('Acquisition ' + a.id + ' → ' + a.state);
          this.showAcquire = false;
          nav('/acquisitions/' + a.id);
        } catch (e) { this.formErr = e.message; } finally { this.busy = false; }
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Acquisitions</h1><div class="grow"></div>
        <button class="btn primary" @click="openAcquire">Acquire capacity</button></div>
      <div class="card">
        <div class="btn-row">
          <input type="text" v-model="lookup" placeholder="acq_… look up by ID" style="max-width:320px"
                 @keyup.enter="go" class="mono">
          <button class="btn" @click="go">Open</button>
          <span class="muted small">The API has no acquisition list; shown below are ones created from this browser.</span>
        </div>
        <div v-if="err" class="err-inline mt">{{ err }}</div>
        <div v-else-if="!acqs.length && !loading" class="empty">Nothing acquired from this browser yet.</div>
        <div v-else class="tbl-wrap mt"><table class="tbl">
          <thead><tr><th>ID</th><th>State</th><th>Class</th><th>Resource</th><th>Age</th><th></th></tr></thead>
          <tbody><tr v-for="a in acqs" :key="a.id" class="rowlink" @click="$nav('/acquisitions/' + a.id)">
            <td class="id">{{ a.id }}</td>
            <td><op-badge v-if="a.state === 'failed' || a.state === 'expired'" :state="'failed'"></op-badge>
                <span v-else class="badge"><span class="dot" :style="{background: a.state === 'bound' ? 'var(--st-good)' : 'var(--ph-requested)'}"></span>{{ a.state }}</span></td>
            <td>{{ a.class || '—' }}</td>
            <td class="id"><a v-if="a.resourceId" :href="'#/resources/' + a.resourceId" @click.stop>{{ a.resourceId }}</a></td>
            <td class="sub">{{ $ago(a.createdAt) }}</td><td></td>
          </tr></tbody></table></div>
      </div>

      <modal-box v-if="showAcquire" title="Acquire capacity" @close="showAcquire = false">
        <form @submit.prevent="acquire">
          <label class="f" for="a-class">Class</label>
          <input id="a-class" type="text" v-model="form.class" placeholder="ci" required>
          <label class="f" for="a-ttl">Lease TTL (optional)</label>
          <input id="a-ttl" type="text" v-model="form.ttl" placeholder="90m">
          <label class="f" for="a-cons">Constraints (optional JSON)</label>
          <textarea id="a-cons" v-model="form.constraints" style="min-height:64px" spellcheck="false"
                    placeholder='{"cpu":{"min":2},"memoryMiB":{"min":4096}}'></textarea>
          <label class="f"><input type="checkbox" v-model="form.exclusive" style="width:auto"> Exclusive (whole resource)</label>
          <div v-if="formErr" class="field-err">{{ formErr }}</div>
          <div class="actions">
            <button type="button" class="btn" @click="showAcquire = false">Cancel</button>
            <button type="submit" class="btn primary" :disabled="busy"><span v-if="busy" class="wip"></span> Acquire</button>
          </div>
        </form>
      </modal-box>
    </div>`,
  });

  app.component('acq-detail', {
    mixins: [polls],
    data() { return { id: store.route.id, a: null, busy: false }; },
    methods: {
      async load() {
        try { this.a = await api('GET', '/v1/acquisitions/' + this.id); this.err = ''; rememberAcq(this.id); }
        catch (e) { this.err = loadErr(e); }
        this.loading = false;
      },
      async release() {
        this.busy = true;
        try {
          await api('DELETE', '/v1/acquisitions/' + this.id);
          toast('Released ' + this.id);
          this.load(true);
        } catch (e) { toastErr(e); } finally { this.busy = false; }
      },
    },
    template: `
    <div>
      <div class="topbar">
        <h1><a href="#/acquisitions">Acquisitions</a> / <span class="mono">{{ id }}</span></h1>
        <div class="grow"></div>
        <button v-if="a && (a.state === 'bound' || a.state === 'pending' || a.state === 'provisioning')"
                class="btn danger" @click="release" :disabled="busy">Release</button>
      </div>
      <div v-if="err" class="card err-inline">{{ err }}</div>
      <div v-else-if="a" class="card">
        <dl class="kv">
          <dt>State</dt><dd>
            <span class="badge"><span class="dot" :style="{background: a.state === 'bound' ? 'var(--st-good)' :
              (a.state === 'failed' || a.state === 'expired' ? 'var(--st-critical)' : 'var(--ph-requested)')}"></span>
              {{ a.state }}</span>
            <span v-if="a.state === 'pending' || a.state === 'provisioning'" class="wip" style="margin-left:8px"></span></dd>
          <dt>Class</dt><dd>{{ a.class || '—' }}</dd>
          <dt>Resource kind</dt><dd>{{ a.resourceKind || '—' }}</dd>
          <dt>Resource</dt><dd class="mono"><a v-if="a.resourceId" :href="'#/resources/' + a.resourceId">{{ a.resourceId }}</a><span v-else>—</span></dd>
          <dt>Lease</dt><dd class="mono">{{ a.leaseId || '—' }}</dd>
          <dt>Actor</dt><dd>{{ a.actor || '—' }}</dd>
          <dt>Created</dt><dd>{{ $time(a.createdAt) }} ({{ $ago(a.createdAt) }})</dd>
          <dt>Updated</dt><dd>{{ $time(a.updatedAt) }}</dd>
        </dl>
      </div>
    </div>`,
  });

  // ------------------------------------------------------------------ operations

  app.component('ops-view', {
    mixins: [polls],
    data() { return { items: [], filter: 'all', resolveOp: null, busy: false }; },
    computed: {
      filtered() {
        if (this.filter === 'uncertain') return this.items.filter((o) => o.state === 'uncertain');
        if (this.filter === 'verifying') return this.items.filter((o) => o.state === 'verifying');
        return this.items;
      },
    },
    methods: {
      async load() {
        try {
          const d = await api('GET', '/v1/operations');
          this.items = (d.items || []).sort((a, b) => (a.updatedAt < b.updatedAt ? 1 : -1));
          this.err = '';
        } catch (e) { this.err = loadErr(e); }
        this.loading = false;
      },
      async resolve(action) {
        this.busy = true;
        try {
          await api('POST', '/v1/operations/' + this.resolveOp.id + ':resolve', { action: action });
          toast(this.resolveOp.id + ': ' + action);
          this.resolveOp = null;
          this.load(true);
        } catch (e) { toastErr(e); } finally { this.busy = false; }
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Operations</h1>
        <span class="muted small">open operations — terminal ones are visible in events</span>
        <div class="grow"></div>
        <div class="pill-select" role="group" aria-label="Filter">
          <button v-for="f in ['all', 'verifying', 'uncertain']" :key="f"
                  :class="{on: filter === f}" @click="filter = f">{{ f }}</button>
        </div></div>
      <div class="card">
        <div v-if="err" class="err-inline">{{ err }}</div>
        <div v-else-if="!filtered.length && !loading" class="empty">No open operations{{ filter !== 'all' ? ' (' + filter + ')' : '' }} — the fleet is converged.</div>
        <div v-else class="tbl-wrap"><table class="tbl">
          <thead><tr><th>ID</th><th>Kind</th><th>State</th><th>Resource</th><th>Provider</th><th>Attempt</th><th>Error class</th><th>Updated</th><th></th></tr></thead>
          <tbody><tr v-for="o in filtered" :key="o.id">
            <td class="id">{{ o.id }}</td><td>{{ o.kind }}</td>
            <td><op-badge :state="o.state"></op-badge></td>
            <td class="id"><a v-if="o.resourceId" :href="'#/resources/' + o.resourceId">{{ o.resourceId }}</a></td>
            <td>{{ o.provider }}</td><td class="num">{{ o.attempt }}</td>
            <td>{{ o.errorClass || '—' }}</td>
            <td class="sub" :title="$time(o.updatedAt)">{{ $ago(o.updatedAt) }}</td>
            <td><button v-if="o.state === 'uncertain'" class="btn sm" @click="resolveOp = o">Resolve…</button></td>
          </tr></tbody></table></div>
      </div>

      <modal-box v-if="resolveOp" :title="'Resolve ' + resolveOp.id" @close="resolveOp = null">
        <p>This operation is <strong>uncertain</strong>: the provider may or may not have performed the
        mutation, and automatic verification gave up. Choose how to resolve it (requires the
        <span class="mono">provider.admin</span> permission):</p>
        <ul class="small">
          <li><strong>Retry verification</strong> — re-run discovery against the provider to find the result.</li>
          <li><strong>Mark failed</strong> — declare the mutation as not-performed. Only after checking the
            provider console: if the resource does exist, discovery will surface it as a ghost.</li>
        </ul>
        <div class="actions">
          <button class="btn" @click="resolveOp = null">Cancel</button>
          <button class="btn" @click="resolve('retry-verification')" :disabled="busy">Retry verification</button>
          <button class="btn danger" @click="resolve('mark-failed')" :disabled="busy">Mark failed</button>
        </div>
      </modal-box>
    </div>`,
  });

  // ------------------------------------------------------------------ events

  app.component('events-view', {
    mixins: [polls],
    data() { return { items: [], q: '' }; },
    computed: {
      filtered() {
        const q = this.q.trim().toLowerCase();
        const sorted = this.items.slice().sort((a, b) => (a.ts < b.ts ? 1 : -1));
        if (!q) return sorted;
        return sorted.filter((e) => JSON.stringify(e).toLowerCase().includes(q));
      },
    },
    methods: {
      async load() {
        try {
          const d = await api('GET', '/v1/events?limit=300');
          this.items = d.items || [];
          this.err = '';
        } catch (e) { this.err = loadErr(e); }
        this.loading = false;
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Events</h1><div class="grow"></div>
        <input type="text" v-model="q" placeholder="filter…" style="max-width:240px"></div>
      <div class="card">
        <div v-if="err" class="err-inline">{{ err }}</div>
        <div v-else-if="!filtered.length && !loading" class="empty">No events.</div>
        <div v-else class="tbl-wrap"><table class="tbl">
          <thead><tr><th>Time</th><th>Type</th><th>Outcome</th><th>Resource</th><th>Operation</th><th>Provider</th><th>Actor</th></tr></thead>
          <tbody><tr v-for="e in filtered" :key="e.id">
            <td class="sub" :title="$time(e.ts)">{{ $ago(e.ts) }}</td>
            <td>{{ e.type }}</td><td>{{ e.outcome || '—' }}</td>
            <td class="id"><a v-if="e.resourceId" :href="'#/resources/' + e.resourceId">{{ e.resourceId }}</a></td>
            <td class="id">{{ e.operationId || '' }}</td>
            <td>{{ e.provider || '' }}</td><td class="sub">{{ e.actor || '' }}</td>
          </tr></tbody></table></div>
      </div>
    </div>`,
  });

  // ------------------------------------------------------------------ providers

  app.component('providers-view', {
    mixins: [polls],
    data() { return { items: [] }; },
    methods: {
      async load() {
        try { const d = await api('GET', '/v1/providers'); this.items = d.items || []; this.err = ''; }
        catch (e) { this.err = loadErr(e); }
        this.loading = false;
      },
    },
    template: `
    <div>
      <div class="topbar"><h1>Providers</h1></div>
      <div v-if="err" class="card err-inline">{{ err }}</div>
      <div v-else class="cards-row">
        <div v-for="p in items" :key="p.instance" class="card">
          <h2 style="text-transform:none; font-size:15px; color:var(--ink)">{{ p.instance }}</h2>
          <dl class="kv" style="grid-template-columns: 110px 1fr">
            <dt>Driver</dt><dd>{{ p.driver }}</dd>
            <dt>State</dt><dd><health-badge :state="p.state"></health-badge></dd>
            <dt>Since</dt><dd :title="$time(p.since)">{{ $ago(p.since) }}</dd>
            <dt>Last check</dt><dd :title="$time(p.lastCheck)">{{ $ago(p.lastCheck) || '—' }}</dd>
            <dt>Failures</dt><dd>{{ p.consecutiveFailures }}</dd>
            <dt v-if="p.lastError">Last error</dt><dd v-if="p.lastError" class="small">{{ p.lastError }}</dd>
          </dl>
        </div>
        <div v-if="!items.length && !loading" class="card empty">No provider instances configured.</div>
      </div>
    </div>`,
  });

  app.mount('#app');
})();
