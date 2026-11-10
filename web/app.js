/* samll-ci 控制台 —— 原生 JS 单页应用（fetch + SSE）
   UI 基于本地内嵌的 Oat UI（/vendor/oat/，零依赖语义化组件库），不使用任何前端框架 */
'use strict';

const $ = (sel, el = document) => el.querySelector(sel);
const main = $('#main');

let token = localStorage.getItem('cicd_token') || '';
let currentUser = null;

/* ---------- 基础工具 ---------- */

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));
}

/* Oat UI toast：ot.toast(message, title?, {variant, placement}) */
function toast(msg, isErr = false) {
  if (window.ot && ot.toast) {
    ot.toast(msg, isErr ? '出错了' : '提示', { variant: isErr ? 'danger' : 'success' });
  } else {
    console.log('[toast]', msg);
  }
}

/* Oat UI 原生 dialog 确认框（替代 window.confirm） */
function confirmDialog(text, title = '确认操作') {
  return new Promise(resolve => {
    const dlg = $('#confirm-dialog');
    $('#confirm-title').textContent = title;
    $('#confirm-text').textContent = text;
    const ok = $('#confirm-ok'), cancel = $('#confirm-cancel');
    const done = val => { dlg.close(); ok.onclick = cancel.onclick = null; resolve(val); };
    ok.onclick = () => done(true);
    cancel.onclick = () => done(false);
    dlg.showModal();
  });
}

async function api(method, path, body) {
  const headers = { 'Content-Type': 'application/json' };
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const resp = await fetch(path, {
    method, headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) { showLogin(); throw new Error('未登录'); }
  let json = null;
  try { json = await resp.json(); } catch (_) { /* SSE 或空响应 */ }
  if (!resp.ok || (json && json.code !== 0)) {
    throw new Error((json && json.message) || ('HTTP ' + resp.status));
  }
  return json && json.data;
}

function fmtSize(bytes) {
  if (!bytes) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, n = bytes;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(n >= 100 || i === 0 ? 0 : 1) + ' ' + units[i];
}

function fmtDur(startAt, endAt) {
  if (!startAt || !endAt) return '-';
  const ms = new Date(endAt.replace(' ', 'T')) - new Date(startAt.replace(' ', 'T'));
  if (!(ms > 0)) return '-';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + 's';
  return Math.floor(s / 60) + 'm' + (s % 60) + 's';
}

/* Oat badge 变体：pending 灰(outline) / running 蓝(默认primary) / success 绿 / failed 红 / cancelled 灰(outline) */
function statusBadge(s) {
  const variant = {
    pending: 'outline', running: '', success: 'success',
    failed: 'danger', cancelled: 'outline',
    not_installed: 'outline', installing: 'warning', ready: 'success',
  }[s] || '';
  return `<span class="badge ${variant}" data-variant="${variant}">${esc(statusText(s))}</span>`;
}
function statusText(s) {
  return { pending: '排队中', running: '运行中', success: '成功', failed: '失败', cancelled: '已取消',
           not_installed: '未安装', installing: '安装中', ready: '就绪' }[s] || s;
}

/* ---------- 路由 ---------- */

const routes = [
  { re: /^#\/login$/, view: viewLogin },
  { re: /^#\/projects$/, view: viewProjects },
  { re: /^#\/projects\/new$/, view: () => viewProjectForm(null) },
  { re: /^#\/projects\/(\d+)\/edit$/, view: m => viewProjectForm(+m[1]) },
  { re: /^#\/builds$/, view: viewBuilds },
  { re: /^#\/builds\/(\d+)$/, view: m => viewBuildDetail(+m[1]) },
  { re: /^#\/toolchains$/, view: viewToolchains },
  { re: /^#\/cache$/, view: viewCache },
  { re: /^#\/settings$/, view: viewSettings },
];

async function route() {
  const hash = location.hash || '#/projects';
  for (const r of routes) {
    const m = hash.match(r.re);
    if (m) {
      if (!currentUser && hash !== '#/login') { showLogin(); return; }
      setNav(hash);
      try { await r.view(m); } catch (e) { if (e.message !== '未登录') toast(e.message, true); }
      return;
    }
  }
  location.hash = '#/projects';
}

function setNav(hash) {
  const page = hash.split('/')[1] || 'projects';
  document.querySelectorAll('#shell nav a').forEach(a => {
    if (a.dataset.nav === page) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
}

window.addEventListener('hashchange', route);

/* ---------- 登录 / 外壳切换 ---------- */

function showLogin() {
  currentUser = null;
  $('#shell').classList.add('hidden');
  $('#login-view').classList.remove('hidden');
  $('#login-err').classList.add('hidden');
  if (location.hash !== '#/login') { history.replaceState(null, '', '#/login'); }
  document.title = '登录 · samll-ci';
  $('#login-pwd').focus();
}

function showShell() {
  currentUser = true;
  $('#login-view').classList.add('hidden');
  $('#shell').classList.remove('hidden');
}

async function viewLogin() {
  // 登录视图的 DOM 在 index.html 中，这里只做事件绑定
  $('#login-form').onsubmit = async e => {
    e.preventDefault();
    const pwd = $('#login-pwd').value;
    if (!pwd) return;
    const btn = $('#login-form').querySelector('button');
    btn.disabled = true;
    try {
      const data = await api('POST', '/api/login', { password: pwd });
      token = data.token;
      localStorage.setItem('cicd_token', token);
      showShell();
      location.hash = '#/projects';
      route();
    } catch (err) {
      const el = $('#login-err');
      el.textContent = err.message === '未登录' ? '密码错误' : err.message;
      el.setAttribute('data-variant', 'danger');
      el.classList.remove('hidden');
    } finally { btn.disabled = false; }
  };
}

$('#logout-btn').onclick = async () => {
  try { await api('POST', '/api/logout'); } catch (_) {}
  token = '';
  localStorage.removeItem('cicd_token');
  showLogin();
};

/* ---------- 项目列表 ---------- */

async function viewProjects() {
  document.title = '项目 · samll-ci';
  const [projects, toolchains] = await Promise.all([
    api('GET', '/api/projects'), api('GET', '/api/toolchains'),
  ]);
  const tcName = id => {
    const tc = toolchains.find(t => t.id === id);
    return tc ? tc.name : '-';
  };
  const rows = projects.map(p => `
    <tr>
      <td><b>${esc(p.name)}</b></td>
      <td class="muted">${esc(p.branch || 'main')}</td>
      <td>${p.build_mode === 'docker' ? '🐳 Docker' : '💻 Shell'}</td>
      <td class="muted">${esc(tcName(p.toolchain_id))}</td>
      <td>${p.last_build ? statusBadge(p.last_build.status) : '<span class="muted">未构建</span>'}</td>
      <td>${p.last_build && p.last_build.git_commit ? `<code class="commit">${esc(p.last_build.git_commit.slice(0, 8))}</code>` : '-'}</td>
      <td>
        <div class="actions">
          <button class="small" data-build="${p.id}" ${p.last_build && (p.last_build.status === 'running' || p.last_build.status === 'pending') ? 'disabled' : ''}>▶ 构建</button>
          <a class="button outline small" href="#/projects/${p.id}/edit">编辑</a>
          <button class="outline small" data-variant="danger" data-del="${p.id}">删除</button>
        </div>
      </td>
    </tr>`).join('');
  main.innerHTML = `
    <div class="page-head">
      <h1>项目</h1>
      <div class="actions"><a class="button" href="#/projects/new">＋ 新建项目</a></div>
    </div>
    <div class="table card" style="padding:0">
      <table>
        <thead><tr><th>名称</th><th>分支</th><th>模式</th><th>工具链</th><th>最近构建</th><th>Commit</th><th>操作</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="7" class="muted" style="text-align:center;padding:30px">还没有项目，点击右上角「新建项目」开始</td></tr>'}</tbody>
      </table>
    </div>`;
  main.querySelectorAll('[data-build]').forEach(btn => btn.onclick = async () => {
    btn.disabled = true;
    try {
      const d = await api('POST', `/api/projects/${btn.dataset.build}/build`);
      toast('构建 #' + d.build_id + ' 已提交');
      location.hash = '#/builds/' + d.build_id;
    } catch (e) { toast(e.message, true); btn.disabled = false; }
  });
  main.querySelectorAll('[data-del]').forEach(btn => btn.onclick = async () => {
    if (!await confirmDialog('确定删除该项目？构建历史将保留。')) return;
    try { await api('DELETE', `/api/projects/${btn.dataset.del}`); toast('已删除'); viewProjects(); }
    catch (e) { toast(e.message, true); }
  });
}

/* ---------- 项目编辑 ---------- */

const buildCmdTemplates = {
  node: 'npm install\nnpm run build',
  jdk: 'mvn clean package -DskipTests',
  maven: 'mvn clean package -DskipTests',
  gradle: 'gradle clean build -x test',
  go: 'go build -o app ./...',
  bun: 'bun install\nbun run build',
};

// Maven settings.xml 国内镜像模板（阿里云公共仓库）
const mavenSettingsTemplate = `<settings>
  <mirrors>
    <mirror>
      <id>aliyun</id>
      <mirrorOf>central</mirrorOf>
      <name>aliyun maven</name>
      <url>https://maven.aliyun.com/repository/public</url>
    </mirror>
  </mirrors>
  <localRepository>\${env.MAVEN_REPO_LOCAL}</localRepository>
</settings>`;

async function viewProjectForm(id) {
  document.title = '项目编辑 · samll-ci';
  const [toolchains, projects] = await Promise.all([
    api('GET', '/api/toolchains'), api('GET', '/api/projects'),
  ]);
  const p = id ? (projects.find(x => x.id === id) || {}) : {};
  const isNew = !id;
  main.innerHTML = `
    <div class="page-head">
      <h1>${isNew ? '新建项目' : '编辑项目 · ' + esc(p.name || '')}</h1>
      <div class="actions"><a class="button outline" href="#/projects">返回列表</a></div>
    </div>
    <div class="card"><div class="form-grid">
      <div class="field"><label>项目名 *</label><input id="f-name" value="${esc(p.name || '')}" placeholder="my-app"></div>
      <div class="field"><label>分支</label><input id="f-branch" value="${esc(p.branch || 'main')}" placeholder="main"></div>
      <div class="field full"><label>Git 地址 *</label><input id="f-git" value="${esc(p.git_url || '')}" placeholder="https://github.com/user/repo.git"></div>
      <div class="field"><label>访问令牌（私有库，可空）</label><input id="f-token" type="password" value="${esc(p.token || '')}" placeholder="ghp_xxx"></div>
      <div class="field"><label>构建模式 *</label>
        <select id="f-mode">
          <option value="shell" ${p.build_mode !== 'docker' ? 'selected' : ''}>Shell（本机执行）</option>
          <option value="docker" ${p.build_mode === 'docker' ? 'selected' : ''}>Docker（容器内构建）</option>
        </select></div>
      <div class="field"><label>工具链</label>
        <select id="f-toolchain">
          <option value="0">- 无（使用宿主 PATH）-</option>
          ${toolchains.map(t => `<option value="${t.id}" data-type="${t.type}" ${p.toolchain_id === t.id ? 'selected' : ''}>${esc(t.name)}（${esc(t.type)} ${esc(t.version)}）</option>`).join('')}
        </select></div>
      <div class="field" id="docker-image-field" style="display:none"><label>基础镜像（Docker 模式）</label><input id="f-image" value="${esc(p.docker_image || '')}" placeholder="留空按工具链自动选择，如 node:20-alpine"></div>
      <div class="field" id="dockerfile-field" style="display:none"><label>Dockerfile（填了则构建后打成镜像，相对工作区）</label><input id="f-dockerfile" value="${esc(p.dockerfile || '')}" placeholder="如 Dockerfile（留空 = 仅容器内构建）"></div>
      <div class="field" id="imagetag-field" style="display:none"><label>镜像 Tag（模式②）</label><input id="f-imagetag" value="${esc(p.image_tag || '')}" placeholder="如 myapp:latest"></div>
      <div class="field full"><label>构建命令（每行一条，失败即停）</label><textarea id="f-build" placeholder="npm install&#10;npm run build">${esc(p.build_cmds || '')}</textarea>
        <div class="muted mt">默认模板：<a class="link" data-tpl="node">Node</a> · <a class="link" data-tpl="maven">Java/Maven</a> · <a class="link" data-tpl="gradle">Gradle</a> · <a class="link" data-tpl="go">Go</a> · <a class="link" data-tpl="bun">Bun</a></div></div>
      <div class="field"><label>产物输出目录（相对仓库根，可空）</label><input id="f-output" value="${esc(p.output_dir || '')}" placeholder="如 dist / target"></div>
      <div class="field"><label>超时（秒）</label><input id="f-timeout" type="number" value="${p.timeout || 3600}" min="10"></div>
      <div class="field"><label>内存上限覆盖（MB，0 = 用全局设置；重项目可单独放宽）</label><input id="f-mem-limit" type="number" value="${p.mem_limit_mib || 0}" min="0" placeholder="0 = 继承全局"></div>
      <div class="field full"><label>部署命令（构建成功后执行，可空）</label><textarea id="f-deploy" placeholder="systemctl restart myapp">${esc(p.deploy_cmds || '')}</textarea></div>
      <div class="switch-row full"><input id="f-cache" type="checkbox" ${p.cache_enabled === false ? '' : 'checked'}><label for="f-cache">启用构建缓存（依赖/编译缓存跨次复用）</label></div>
    </div>
    <div class="form-actions">
      <button id="f-save">${isNew ? '创建' : '保存'}</button>
      ${!isNew ? '<button id="f-del" class="outline" data-variant="danger">删除项目</button>' : ''}
    </div></div>`;

  const modeSel = $('#f-mode');
  const syncDockerFields = () => {
    const isDocker = modeSel.value === 'docker';
    $('#docker-image-field').style.display = isDocker ? '' : 'none';
    $('#dockerfile-field').style.display = isDocker ? '' : 'none';
    $('#imagetag-field').style.display = isDocker ? '' : 'none';
  };
  modeSel.onchange = syncDockerFields; syncDockerFields();

  main.querySelectorAll('[data-tpl]').forEach(a => a.onclick = () => {
    $('#f-build').value = buildCmdTemplates[a.dataset.tpl] || '';
  });

  $('#f-save').onclick = async () => {
    const body = {
      name: $('#f-name').value.trim(),
      git_url: $('#f-git').value.trim(),
      branch: $('#f-branch').value.trim() || 'main',
      token: $('#f-token').value.trim(),
      build_mode: modeSel.value,
      toolchain_id: +$('#f-toolchain').value || 0,
      docker_image: $('#f-image') ? $('#f-image').value.trim() : '',
      dockerfile: $('#f-dockerfile') ? $('#f-dockerfile').value.trim() : '',
      image_tag: $('#f-imagetag') ? $('#f-imagetag').value.trim() : '',
      build_cmds: $('#f-build').value,
      output_dir: $('#f-output').value.trim(),
      deploy_cmds: $('#f-deploy').value,
      timeout: +$('#f-timeout').value || 3600,
      cache_enabled: $('#f-cache').checked,
      mem_limit_mib: +$('#f-mem-limit').value || 0,
    };
    if (!body.name || !body.git_url) { toast('项目名与 Git 地址必填', true); return; }
    try {
      if (isNew) { await api('POST', '/api/projects', body); toast('项目已创建'); }
      else { await api('PUT', `/api/projects/${id}`, body); toast('已保存'); }
      location.hash = '#/projects';
    } catch (e) { toast(e.message, true); }
  };
  if (!isNew) $('#f-del').onclick = async () => {
    if (!await confirmDialog('确定删除该项目？')) return;
    try { await api('DELETE', `/api/projects/${id}`); toast('已删除'); location.hash = '#/projects'; }
    catch (e) { toast(e.message, true); }
  };
}

/* ---------- 构建历史 ---------- */

async function viewBuilds() {
  document.title = '构建历史 · samll-ci';
  const projects = await api('GET', '/api/projects');
  main.innerHTML = `
    <div class="page-head"><h1>构建历史</h1></div>
    <div class="card">
      <div style="display:flex;gap:var(--space-3);margin-bottom:var(--space-4)">
        <select id="filter-project" style="width:220px"><option value="">全部项目</option>
          ${projects.map(p => `<option value="${p.id}">${esc(p.name)}</option>`).join('')}</select>
        <select id="filter-status" style="width:140px">
          <option value="">全部状态</option>
          ${['pending', 'running', 'success', 'failed', 'cancelled'].map(s => `<option value="${s}">${statusText(s)}</option>`).join('')}
        </select>
      </div>
      <div id="builds-table"><p class="muted">加载中...</p></div>
    </div>`;
  const load = async () => {
    const pid = $('#filter-project').value, st = $('#filter-status').value;
    const builds = await api('GET', `/api/builds?limit=100&project_id=${pid}&status=${st}`);
    const pmap = Object.fromEntries(projects.map(p => [p.id, p.name]));
    const rows = builds.map(b => `
      <tr>
        <td><a class="link" href="#/builds/${b.id}">#${b.id}</a></td>
        <td>${esc(pmap[b.project_id] || b.project_id)}</td>
        <td>${statusBadge(b.status)}</td>
        <td>${b.git_commit ? `<code class="commit">${esc(b.git_commit.slice(0, 8))}</code>` : '-'}</td>
        <td class="muted">${esc(b.created_at || '')}</td>
        <td class="muted">${fmtDur(b.start_at, b.end_at)}</td>
      </tr>`).join('');
    $('#builds-table').innerHTML = `
      <div class="table"><table>
        <thead><tr><th>#</th><th>项目</th><th>状态</th><th>Commit</th><th>提交时间</th><th>耗时</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">暂无构建记录</td></tr>'}</tbody>
      </table></div>`;
  };
  $('#filter-project').onchange = load;
  $('#filter-status').onchange = load;
  await load();
}

/* ---------- 构建详情（SSE 实时日志） ---------- */

async function viewBuildDetail(id) {
  document.title = '构建详情 · samll-ci';
  const d = await api('GET', `/api/builds/${id}`);
  const b = d.build;
  main.innerHTML = `
    <div class="page-head">
      <h1>构建 #${b.id} <span class="muted">· ${esc(d.project_name)}</span></h1>
      <div class="actions">
        <button id="cancel-btn" data-variant="danger" style="display:none">✕ 取消构建</button>
        <a class="button outline" href="#/builds">返回历史</a>
      </div>
    </div>
    <div class="card detail-stats">
      <div><div class="muted">状态</div><div style="margin-top:4px" id="d-status">${statusBadge(b.status)}</div></div>
      <div><div class="muted">Commit</div><div style="margin-top:4px"><code class="commit" id="d-commit">${esc(b.git_commit || '-')}</code></div></div>
      <div><div class="muted">开始</div><div class="muted" style="margin-top:6px">${esc(b.start_at || '-')}</div></div>
      <div><div class="muted">结束</div><div class="muted" style="margin-top:6px" id="d-end">${esc(b.end_at || '-')}</div></div>
      <div><div class="muted">耗时</div><div class="muted" style="margin-top:6px" id="d-dur">${fmtDur(b.start_at, b.end_at)}</div></div>
    </div>
    <div class="log-box" id="log"></div>`;

  const logEl = $('#log');
  let autoScroll = true;
  logEl.addEventListener('scroll', () => {
    autoScroll = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 30;
  });
  const append = text => {
    logEl.textContent += text;
    if (autoScroll) logEl.scrollTop = logEl.scrollHeight;
  };
  const refreshStatus = async () => {
    const d2 = await api('GET', `/api/builds/${id}`);
    const b2 = d2.build;
    $('#d-status').innerHTML = statusBadge(b2.status);
    $('#d-commit').textContent = b2.git_commit || '-';
    $('#d-end').textContent = b2.end_at || '-';
    $('#d-dur').textContent = fmtDur(b2.start_at, b2.end_at);
    $('#cancel-btn').style.display = (b2.status === 'running') ? '' : 'none';
    return b2.status;
  };
  $('#cancel-btn').onclick = async () => {
    if (!await confirmDialog('确定取消该构建？')) return;
    try { await api('POST', `/api/builds/${id}/cancel`); toast('取消指令已发送'); }
    catch (e) { toast(e.message, true); }
  };
  await refreshStatus();

  // SSE 实时日志（未结束的构建）或全文（历史构建）
  const es = new EventSource(`/api/builds/${id}/log`);
  es.addEventListener('log', e => {
    try { append(JSON.parse(e.data).text); } catch (_) {}
  });
  es.addEventListener('status', async e => {
    try {
      const { status } = JSON.parse(e.data);
      $('#d-status').innerHTML = statusBadge(status);
      $('#cancel-btn').style.display = status === 'running' ? '' : 'none';
    } catch (_) {}
  });
  es.addEventListener('done', async () => {
    es.close();
    append('\n[连接关闭：构建已结束]\n');
    await refreshStatus();
  });
  es.onerror = () => { /* EventSource 自动重连；构建结束由 done 事件关闭 */ };
}

/* ---------- 工具链管理 ---------- */

async function viewToolchains() {
  document.title = '工具链 · samll-ci';
  const tcs = await api('GET', '/api/toolchains');
  const rows = tcs.map(t => `
    <tr>
      <td><b>${esc(t.name)}</b></td>
      <td>${esc(t.type)}</td>
      <td class="muted">${esc(t.version || '-')}</td>
      <td class="muted" style="max-width:220px;overflow:hidden;text-overflow:ellipsis">${esc(t.download_url || '-')}</td>
      <td>${statusBadge(t.status)}</td>
      <td>
        <div class="actions">
          <button class="small" data-install="${t.id}" ${t.status === 'installing' ? 'disabled' : ''}>${t.status === 'ready' ? '重装' : '安装'}</button>
          <button class="outline small" data-edit="${t.id}">编辑</button>
          <button class="outline small" data-variant="danger" data-del="${t.id}">删除</button>
        </div>
      </td>
    </tr>`).join('');
  main.innerHTML = `
    <div class="page-head">
      <h1>工具链</h1>
      <div class="actions"><button id="tc-new">＋ 新增工具链</button></div>
    </div>
    <div class="table card" style="padding:0">
      <table>
        <thead><tr><th>名称</th><th>类型</th><th>版本</th><th>下载地址</th><th>状态</th><th>操作</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">暂无工具链。Shell 模式可不配置（使用宿主 PATH）；需要托管 JDK/Maven/Gradle/Node/Go/Bun 时在此添加。</td></tr>'}</tbody>
      </table>
    </div>
    <div class="card hidden" id="tc-form"><div class="form-grid">
      <div class="field"><label>类型 *</label>
        <select id="t-type">${['jdk', 'maven', 'gradle', 'node', 'go', 'bun'].map(t => `<option value="${t}">${t.toUpperCase()}</option>`).join('')}</select></div>
      <div class="field"><label>在线版本（国内镜像，点选自动填充）</label>
        <select id="t-preset"><option value="">选择类型后加载…</option></select></div>
      <div class="field"><label>展示名 *</label><input id="t-name" placeholder="JDK 8"></div>
      <div class="field"><label>版本</label><input id="t-version" placeholder="8u402-b10"></div>
      <div class="field full"><label>下载地址（可自定义镜像源）</label><input id="t-url" placeholder="https://mirrors.tuna.tsinghua.edu.cn/..."></div>
      <div class="field"><label>安装目录（留空自动为 &lt;data&gt;/toolchains/&lt;type&gt;/&lt;version&gt;；填已有目录则直接标记就绪）</label><input id="t-path" placeholder=""></div>
      <div class="field full"><label>Maven settings.xml（仅 Maven 类型；镜像源/仓库配置）<a class="link" id="t-fill-settings" style="margin-left:10px">填入阿里云镜像模板</a></label><textarea id="t-settings" placeholder="&lt;settings&gt;...&lt;/settings&gt;"></textarea></div>
    </div>
    <div class="form-actions">
      <button id="t-save">保存</button>
      <button class="outline" id="t-cancel">取消</button>
    </div></div>`;

  $('#tc-new').onclick = () => { $('#tc-form').classList.remove('hidden'); $('#tc-form').dataset.editId = ''; loadTcPresets(); };
  $('#t-cancel').onclick = () => $('#tc-form').classList.add('hidden');
  $('#t-fill-settings').onclick = () => { $('#t-settings').value = mavenSettingsTemplate; };
  $('#t-type').onchange = loadTcPresets;

  async function loadTcPresets(force) {
    const type = $('#t-type').value;
    const sel = $('#t-preset');
    sel.innerHTML = '<option value="">加载中…</option>';
    try {
      const items = await api('GET', `/api/toolchain-versions?type=${type}${force ? '&refresh=1' : ''}`);
      sel.innerHTML = '<option value="">— 手动填写 —</option>' +
        items.map(it => `<option value='${esc(JSON.stringify(it))}'>${esc(it.version)}</option>`).join('');
    } catch (e) {
      sel.innerHTML = `<option value="">获取失败（${esc(e.message)}）</option>`;
    }
  }
  $('#t-preset').onchange = () => {
    const raw = $('#t-preset').value;
    if (!raw) return;
    try {
      const it = JSON.parse(raw);
      $('#t-version').value = it.version;
      $('#t-url').value = it.url;
      if (!$('#t-name').value) $('#t-name').value = it.version;
    } catch (_) {}
  };

  main.querySelectorAll('[data-install]').forEach(btn => btn.onclick = async () => {
    try { await api('POST', `/api/toolchains/${btn.dataset.install}/install`); toast('安装任务已启动'); viewToolchains(); }
    catch (e) { toast(e.message, true); }
  });
  main.querySelectorAll('[data-edit]').forEach(btn => btn.onclick = () => {
    const t = tcs.find(x => x.id === +btn.dataset.edit);
    $('#tc-form').classList.remove('hidden');
    $('#tc-form').dataset.editId = t.id;
    $('#t-type').value = t.type; $('#t-name').value = t.name;
    $('#t-version').value = t.version; $('#t-url').value = t.download_url;
    $('#t-path').value = t.install_path; $('#t-settings').value = t.maven_settings;
  });
  main.querySelectorAll('[data-del]').forEach(btn => btn.onclick = async () => {
    if (!await confirmDialog('确定删除该工具链？')) return;
    try { await api('DELETE', `/api/toolchains/${btn.dataset.del}`); toast('已删除'); viewToolchains(); }
    catch (e) { toast(e.message, true); }
  });

  // 安装中自动刷新
  if (tcs.some(t => t.status === 'installing')) {
    clearTimeout(viewToolchains._timer);
    viewToolchains._timer = setTimeout(() => {
      if (location.hash === '#/toolchains') viewToolchains();
    }, 1500);
  }
}

/* ---------- 缓存管理 ---------- */

async function viewCache() {
  document.title = '缓存管理 · samll-ci';
  const d = await api('GET', '/api/cache');
  const dirNames = { m2: 'Maven 本地仓库', gradle: 'Gradle', npm: 'npm/pnpm/yarn', 'go-mod': 'Go 模块', 'go-build': 'Go 编译', bun: 'Bun' };
  const max = Math.max(...Object.values(d.usage), 1);
  const rows = Object.entries(d.usage).map(([k, v]) => `
    <div class="cache-row">
      <div class="name">${dirNames[k] || k} <span class="muted">(${k})</span></div>
      <progress value="${v}" max="${max}"></progress>
      <div class="cache-size">${fmtSize(v)}</div>
    </div>`).join('');
  main.innerHTML = `
    <div class="page-head">
      <h1>缓存管理 <span class="muted" style="font-size:13px">共 ${fmtSize(d.total)} · ${esc(d.root)}</span></h1>
      <div class="actions"><button data-variant="danger" id="cache-clear">🧹 一键清理缓存</button></div>
    </div>
    <div class="card">${rows}</div>
    <p class="muted mt">缓存只存依赖与编译中间产物（磁盘换时间），最终产物不缓存、直接覆盖输出目录。</p>`;
  $('#cache-clear').onclick = async () => {
    if (!await confirmDialog('确定清理全部构建缓存？下次构建将重新下载依赖。')) return;
    try { await api('POST', '/api/cache/clear'); toast('缓存已清理'); viewCache(); }
    catch (e) { toast(e.message, true); }
  };
}

/* ---------- 设置 ---------- */

async function viewSettings() {
  document.title = '设置 · samll-ci';
  const s = await api('GET', '/api/settings');
  main.innerHTML = `
    <div class="page-head"><h1>设置</h1></div>
    <div class="card">
      <h3 style="margin-bottom:var(--space-4);font-size:1rem">全局设置</h3>
      <div class="form-grid">
        <div class="field"><label>会话有效期（秒）</label><input id="s-ttl" type="number" value="${esc(s['auth.session_ttl'] || 86400)}"></div>
        <div class="field"><label>服务端口（重启生效）</label><input id="s-port" type="number" value="${esc(s['server.port'] || 8080)}"></div>
        <div class="field"><label>启用 Docker 模式</label>
          <select id="s-docker"><option value="true" ${s['docker.enabled'] !== 'false' ? 'selected' : ''}>启用</option><option value="false" ${s['docker.enabled'] === 'false' ? 'selected' : ''}>停用</option></select></div>
        <div class="field"><label>默认构建超时（秒）</label><input id="s-timeout" type="number" value="${esc(s['default.timeout'] || 3600)}"></div>
        <div class="field full"><label>缓存根目录</label><input id="s-cache-root" value="${esc(s['cache.root'] || '')}"></div>
        <div class="field"><label>Go 模块代理 GOPROXY（构建时注入）</label><input id="s-goproxy" value="${esc(s['mirror.goproxy'] || '')}" placeholder="默认 https://goproxy.cn,direct"></div>
        <div class="field"><label>npm registry（构建时注入）</label><input id="s-npm-registry" value="${esc(s['mirror.npm_registry'] || '')}" placeholder="默认 https://registry.npmmirror.com"></div>
        <div class="field"><label>最大内存使用（MB，0 = 不限）</label><input id="s-mem-limit" type="number" value="${esc(s['build.mem_limit_mib'] || 384)}" placeholder="默认 384"></div>
        <div class="field"><label>最大 CPU 核数（0 = 不限）</label><input id="s-cpu-limit" type="number" value="${esc(s['build.max_cpu_cores'] || 0)}" placeholder="默认 0 = 不限"></div>
        <div class="field"><label>Node 新生代 MB（--max-semi-space-size，小内存降 RSS，0 = 默认）</label><input id="s-semi-space" type="number" value="${esc(s['node.semi_space_mib'] || 0)}" placeholder="默认 0 = 不设置"></div>
        <div class="field"><label>Gradle daemon（小内存机器建议禁用）</label>
          <select id="s-gradle-daemon"><option value="true" ${s['gradle.no_daemon'] !== 'false' ? 'selected' : ''}>禁用（构建完即释放内存）</option><option value="false" ${s['gradle.no_daemon'] === 'false' ? 'selected' : ''}>启用（多项目构建更快）</option></select></div>
        <div class="field full muted" style="align-self:end">内存护栏自动映射：Node → --max-old-space-size，JVM → JAVA_TOOL_OPTIONS(-Xmx)，Go → GOMEMLIMIT，Docker → --memory；CPU 护栏：Docker → --cpus，Shell → Linux taskset 绑核（构建/部署阶段）。
      </div>
      <div class="form-actions"><button id="s-save">保存设置</button></div>
    </div>
    <div class="card">
      <h3 style="margin-bottom:var(--space-4);font-size:1rem">修改密码</h3>
      <div class="form-grid">
        <div class="field"><label>旧密码</label><input id="p-old" type="password"></div>
        <div class="field"><label>新密码</label><input id="p-new" type="password"></div>
      </div>
      <div class="form-actions"><button id="p-save">修改密码</button></div>
    </div>`;
  $('#s-save').onclick = async () => {
    try {
      await api('PUT', '/api/settings', {
        'auth.session_ttl': $('#s-ttl').value,
        'server.port': $('#s-port').value,
        'docker.enabled': $('#s-docker').value,
        'default.timeout': $('#s-timeout').value,
        'cache.root': $('#s-cache-root').value,
        'mirror.goproxy': $('#s-goproxy').value,
        'mirror.npm_registry': $('#s-npm-registry').value,
        'build.mem_limit_mib': $('#s-mem-limit').value,
        'build.max_cpu_cores': $('#s-cpu-limit').value,
        'gradle.no_daemon': $('#s-gradle-daemon').value,
        'node.semi_space_mib': $('#s-semi-space').value,
      });
      toast('设置已保存（端口修改需重启生效）');
    } catch (e) { toast(e.message, true); }
  };
  $('#p-save').onclick = async () => {
    try {
      await api('POST', '/api/password', {
        old_password: $('#p-old').value, new_password: $('#p-new').value,
      });
      toast('密码已修改，请重新登录');
      token = ''; localStorage.removeItem('cicd_token');
      showLogin();
    } catch (e) { toast(e.message, true); }
  };
}

/* ---------- 启动：校验登录态 ---------- */

(async function init() {
  viewLogin(); // 预绑定登录表单事件
  if (token) {
    try { await api('GET', '/api/me'); currentUser = true; showShell(); }
    catch (_) { currentUser = null; }
  }
  route();
})();
