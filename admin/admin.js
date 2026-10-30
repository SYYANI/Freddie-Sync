'use strict';

const $ = (id) => document.getElementById(id);

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === 'class') node.className = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else if (value !== undefined && value !== null && value !== false) node.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat()) {
    if (child !== null && child !== undefined && child !== false) node.append(child);
  }
  return node;
}

const messages = {
  invalid_name: '名称需要 1–100 个字节，且不能包含控制字符。',
  invalid_label: '设备名称需要 1–100 个字节，且不能包含控制字符。',
  library_exists: '已有相同 ID 的库。',
  unknown_library: '这个库已不存在。',
  not_found: '这台设备已不存在。',
  default_library: '默认库不能删除。',
  library_in_use: '库里还有可用的设备，先撤销它们。',
  admin_request_denied: '请通过 SSH 转发的本机地址打开此页面。',
};

async function api(method, path, body) {
  let response;
  try {
    response = await fetch(path, {
      method,
      cache: 'no-store',
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new Error('无法连接管理接口，SSH 转发可能已断开。');
  }
  if (response.status === 204) return null;
  const data = await response.json().catch(() => null);
  if (!response.ok) {
    const code = data?.error?.code;
    throw new Error(messages[code] || data?.error?.message || `请求失败（${response.status}）`);
  }
  return data;
}

function showError(error) {
  const box = $('error');
  box.textContent = error ? error.message : '';
  box.hidden = !error;
}

const number = new Intl.NumberFormat('zh-CN');
const day = new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: 'short', day: 'numeric' });
const relative = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' });

function bytes(value) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i += 1; }
  return `${i === 0 ? value : value.toFixed(value < 10 ? 1 : 0)} ${units[i]}`;
}

function ago(iso) {
  const seconds = (Date.parse(iso) - Date.now()) / 1000;
  for (const [unit, size] of [['day', 86400], ['hour', 3600], ['minute', 60]]) {
    if (Math.abs(seconds) >= size) return relative.format(Math.round(seconds / size), unit);
  }
  return '刚刚';
}

const libraryName = (library) => library.name || (library.default ? '默认库' : '未命名库');

let overview = null;

async function load() {
  try {
    overview = await api('GET', '/libraries');
    showError(null);
    render();
  } catch (error) {
    showError(error);
  } finally {
    $('libraries').removeAttribute('aria-busy');
  }
}

function render() {
  const { server, libraries } = overview;
  $('server').textContent = `磁盘剩余 ${bytes(server.diskFreeBytes)}`;
  $('server').title = `${server.publicURL}\n版本 ${server.version}`;
  const active = [];
  const retired = [];
  for (const library of libraries) {
    const allRevoked = library.devices.length > 0 && library.devices.every((device) => device.revoked);
    (allRevoked ? retired : active).push(library);
  }
  $('libraries').replaceChildren(...active.map((library) => libraryCard(library, server)));
  if (!active.length) $('libraries').append(el('p', { class: 'empty' }, '还没有库'));
  const inactive = $('inactive');
  inactive.hidden = !retired.length;
  const summary = inactive.querySelector('summary');
  summary.textContent = `已停用的库（${retired.length}）`;
  summary.title = '库里的设备都已撤销';
  inactive.querySelector('div').replaceChildren(...retired.map((library) => libraryCard(library, server, true)));
}

function libraryCard(library, server, retired = false) {
  const name = libraryName(library);
  const used = server.blobQuotaBytes ? library.blobBytes / server.blobQuotaBytes : 0;
  const fill = el('span');
  fill.style.width = `${Math.min(100, used * 100)}%`;
  const usage = `${bytes(library.blobBytes)} / ${bytes(server.blobQuotaBytes)}`;
  const active = library.devices.filter((device) => !device.revoked);
  const revoked = library.devices.filter((device) => device.revoked);
  return el('section', { class: 'library', 'aria-label': name },
    el('div', { class: 'library-head' },
      el('h2', {}, name, library.default && library.name ? el('span', { class: 'tag' }, '默认') : null),
      el('button', { type: 'button', class: 'quiet', onclick: () => rename(library) }, '重命名'),
      retired
        ? el('button', { type: 'button', class: 'danger', onclick: () => deleteLibrary(library) }, '删除')
        : el('button', { type: 'button', onclick: () => addDevice(library) }, '添加设备')),
    el('div', { class: 'library-id' }, el('code', {}, library.id), copyButton(library.id)),
    el('div', { class: 'usage' },
      el('div', {
        class: used > 0.8 ? 'meter high' : 'meter',
        role: 'meter',
        'aria-label': '文件空间',
        'aria-valuemin': '0',
        'aria-valuemax': '100',
        'aria-valuenow': String(Math.round(used * 100)),
        'aria-valuetext': usage,
      }, fill),
      el('span', {}, usage),
      el('span', {}, `${number.format(library.blobCount)} 个文件`),
      el('span', {}, `${number.format(library.records)} 条记录`)),
    active.length ? deviceTable(active) : el('p', { class: 'empty' }, '没有可用的设备'),
    revoked.length ? el('details', { class: 'revoked' }, el('summary', {}, `已撤销（${revoked.length}）`), deviceTable(revoked)) : null);
}

function deviceTable(devices) {
  return el('table', {},
    el('thead', {}, el('tr', {},
      el('th', { scope: 'col' }, '设备'),
      el('th', { scope: 'col', class: 'when' }, '创建'),
      el('th', { scope: 'col' }, '最近活动'),
      el('th', { scope: 'col' }, el('span', { class: 'visually-hidden' }, '操作')))),
    el('tbody', {}, devices.map((device) => el('tr', {},
      el('td', { class: 'name', title: device.id }, device.label),
      el('td', { class: 'when' }, day.format(new Date(device.createdAt))),
      device.lastSeenAt
        ? el('td', { title: new Date(device.lastSeenAt).toLocaleString('zh-CN') }, ago(device.lastSeenAt))
        : el('td', { class: 'muted', title: '服务启动后还没有连接' }, '—'),
      el('td', {}, device.revoked ? null : el('button', {
        type: 'button',
        class: 'danger',
        'aria-label': `撤销 ${device.label}`,
        onclick: () => revoke(device),
      }, '撤销'))))));
}

function copyButton(text) {
  const ns = 'http://www.w3.org/2000/svg';
  const icon = document.createElementNS(ns, 'svg');
  icon.setAttribute('viewBox', '0 0 16 16');
  icon.setAttribute('width', '14');
  icon.setAttribute('height', '14');
  icon.setAttribute('aria-hidden', 'true');
  for (const d of ['M5.5 5.5h7v8h-7z', 'M3.5 10.5v-8h7']) {
    const path = document.createElementNS(ns, 'path');
    path.setAttribute('d', d);
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.3');
    icon.append(path);
  }
  const button = el('button', { type: 'button', class: 'quiet', 'aria-label': '复制库 ID', title: '复制库 ID' }, icon);
  button.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
      button.title = '已复制';
    } catch (error) {
      showError(error);
    }
  });
  return button;
}

// One dialog serves every form. onSubmit resolves true to keep the dialog open.
function openDialog({ title, body = [], fields = [], submit, danger = false, cancelLabel = '取消', onSubmit, canClose }) {
  const dialog = $('dialog');
  const submitButton = $('dialog-submit');
  const cancelButton = $('dialog-cancel');
  const error = el('p', { class: 'error', role: 'alert', hidden: true });
  const inputs = fields.map((field) => el('input', {
    name: field.name,
    value: field.value ?? '',
    required: true,
    maxlength: '100',
    autocomplete: 'off',
    spellcheck: 'false',
  }));
  $('dialog-title').textContent = title;
  $('dialog-body').replaceChildren(...body, ...fields.map((field, i) => el('label', {}, el('span', {}, field.label), inputs[i])), error);
  submitButton.textContent = submit;
  submitButton.className = danger ? 'primary danger' : 'primary';
  submitButton.disabled = false;
  cancelButton.textContent = cancelLabel;
  cancelButton.className = '';
  const close = () => {
    if (!canClose || canClose()) dialog.close();
  };
  cancelButton.onclick = close;
  dialog.oncancel = (event) => { event.preventDefault(); close(); };
  $('dialog-form').onsubmit = async (event) => {
    event.preventDefault();
    submitButton.disabled = true;
    error.hidden = true;
    try {
      const values = Object.fromEntries(inputs.map((input) => [input.name, input.value.trim()]));
      if (!await onSubmit(values)) dialog.close();
    } catch (failure) {
      error.textContent = failure.message;
      error.hidden = false;
    } finally {
      submitButton.disabled = false;
    }
  };
  if (!dialog.open) dialog.showModal();
  (inputs[0] || submitButton).focus();
}

function newLibrary() {
  let created = null;
  openDialog({
    title: '新建库',
    fields: [{ name: 'name', label: '库名称' }, { name: 'label', label: '第一台设备' }],
    submit: '创建',
    onSubmit: async ({ name, label }) => {
      // A retry after the device step fails reuses the library already created.
      created ??= await api('POST', '/libraries', { name });
      try {
        showCredential(await api('POST', '/devices', { label, libraryID: created.id }));
      } finally {
        load();
      }
      return true;
    },
  });
}

function addDevice(library) {
  openDialog({
    title: `添加设备到「${libraryName(library)}」`,
    fields: [{ name: 'label', label: '设备名称' }],
    submit: '生成凭证',
    onSubmit: async ({ label }) => {
      showCredential(await api('POST', '/devices', { label, libraryID: library.id }));
      load();
      return true;
    },
  });
}

function showCredential(credential) {
  let downloaded = false;
  openDialog({
    title: `「${credential.device.label}」的凭证`,
    body: [
      el('p', {}, '在 Freddie 的“设置 › 同步”中导入这个文件。'),
      el('p', { class: 'consequence' }, '凭证只显示这一次，关闭后无法再取回。'),
    ],
    submit: '下载凭证文件',
    cancelLabel: '完成',
    canClose: () => downloaded || window.confirm('还没有下载凭证。关闭后无法再取回，只能撤销这台设备后重新生成。仍要关闭吗？'),
    onSubmit: async () => {
      download(credential);
      downloaded = true;
      $('dialog-submit').className = '';
      $('dialog-cancel').className = 'primary';
      $('dialog-cancel').focus();
      return true;
    },
  });
}

function download(credential) {
  const blob = new Blob([JSON.stringify(credential, null, 2) + '\n'], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const name = credential.device.label.replace(/[^\p{L}\p{N}]+/gu, '-').replace(/^-+|-+$/g, '') || 'device';
  const link = el('a', { href: url, download: `freddie-${name}.json` });
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function rename(library) {
  openDialog({
    title: '重命名库',
    fields: [{ name: 'name', label: '库名称', value: library.name }],
    submit: '保存',
    onSubmit: async ({ name }) => {
      await api('PATCH', `/libraries/${encodeURIComponent(library.id)}`, { name });
      load();
    },
  });
}

function deleteLibrary(library) {
  openDialog({
    title: `删除「${libraryName(library)}」？`,
    body: [el('p', { class: 'consequence' },
      `${number.format(library.records)} 条记录和 ${number.format(library.blobCount)} 个文件（${bytes(library.blobBytes)}）会从服务器上永久删除，不能恢复。已有的每日备份中最多还保留 7 天。`)],
    submit: '删除',
    danger: true,
    onSubmit: async () => {
      await api('DELETE', `/libraries/${encodeURIComponent(library.id)}`);
      load();
    },
  });
}

function revoke(device) {
  openDialog({
    title: `撤销「${device.label}」？`,
    body: [el('p', { class: 'consequence' }, '这台设备将无法再同步，此操作不能撤销。设备上已有的数据不会被删除。')],
    submit: '撤销',
    danger: true,
    onSubmit: async () => {
      await api('DELETE', `/devices/${encodeURIComponent(device.id)}`);
      load();
    },
  });
}

$('new-library').addEventListener('click', newLibrary);
window.addEventListener('focus', load);
setInterval(() => { if (document.visibilityState === 'visible') load(); }, 30000);
load();
