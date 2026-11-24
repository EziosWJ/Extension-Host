const pluginsRoot = document.querySelector("#plugins");
const countLabel = document.querySelector("#count");
const listState = document.querySelector("#list-state");
const notice = document.querySelector("#notice");
const installForm = document.querySelector("#install-form");
const packageInput = document.querySelector("#package");
const refreshButton = document.querySelector("#refresh");
const dialog = document.querySelector("#uninstall-dialog");
const uninstallForm = document.querySelector("#uninstall-form");
const cancelUninstall = document.querySelector("#cancel-uninstall");
const purge = document.querySelector("#purge");
const purgeWarning = document.querySelector("#purge-warning");
const uninstallError = document.querySelector("#uninstall-error");

const statusLabels = {
  stopped: "已停止",
  starting: "启动中",
  running: "运行中",
  stopping: "停止中",
  failed: "启动失败",
};

const rows = new Map();
let refreshVersion = 0;
let uninstalling = null;

async function api(path, options = {}) {
  const response = await fetch(path, { cache: "no-store", ...options });
  const type = response.headers.get("content-type") || "";
  const body = response.status === 204 ? null : type.includes("application/json")
    ? await response.json()
    : await response.text();
  if (!response.ok) {
    const message = body && typeof body === "object" ? body.error : body;
    throw new Error(message || response.statusText || `HTTP ${response.status}`);
  }
  return body;
}

function feedback(node, message, kind = "") {
  node.textContent = message;
  node.classList.toggle("error", kind === "error");
  node.classList.toggle("success", kind === "success");
}

function element(tag, className, content) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (content !== undefined) node.textContent = content;
  return node;
}

function actionButton(label, className, onClick) {
  const button = element("button", className, label);
  button.type = "button";
  button.addEventListener("click", onClick);
  return button;
}

function pluginPath(id) {
  return `/api/v1/extensions/${encodeURIComponent(id)}`;
}

function setBusy(button, busy, label) {
  button.setAttribute("aria-disabled", String(busy));
  button.textContent = label;
}

function confirmUninstall(plugin, trigger) {
  uninstalling = { plugin, trigger, pending: false };
  uninstallForm.reset();
  purgeWarning.hidden = true;
  uninstallError.textContent = "";
  dialog.querySelector("h2").textContent = `卸载 ${plugin.name}？`;
  dialog.showModal();
}

purge.addEventListener("change", () => {
  purgeWarning.hidden = !purge.checked;
});

cancelUninstall.addEventListener("click", () => {
  if (!uninstalling.pending) dialog.close();
});

dialog.addEventListener("cancel", (event) => {
  if (uninstalling.pending) event.preventDefault();
});

dialog.addEventListener("close", () => {
  uninstalling.trigger.focus();
  if (!uninstalling.trigger.isConnected) refreshButton.focus();
  uninstalling = null;
});

uninstallForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (uninstalling.pending) return;
  const { plugin } = uninstalling;
  const shouldPurge = purge.checked;
  const submit = uninstallForm.querySelector("[type=submit]");
  uninstalling.pending = true;
  purge.disabled = true;
  cancelUninstall.setAttribute("aria-disabled", "true");
  setBusy(submit, true, "正在卸载…");
  uninstallError.textContent = "";
  try {
    await api(pluginPath(plugin.id) + (shouldPurge ? "?purge=true" : ""), { method: "DELETE" });
    dialog.close();
    feedback(notice, `已卸载 ${plugin.name}，业务数据${shouldPurge ? "已删除" : "已保留"}。`, "success");
    await refresh(false);
  } catch (error) {
    uninstallError.textContent = `卸载失败：${error.message}`;
  } finally {
    if (uninstalling?.plugin === plugin) uninstalling.pending = false;
    purge.disabled = false;
    cancelUninstall.removeAttribute("aria-disabled");
    setBusy(submit, false, "卸载");
  }
});

function configForm(plugin) {
  const form = element("form", "config");
  form.append(element("h4", "", "插件配置"));
  const grid = element("div", "field-grid");
  const fields = [];
  const status = element("p", "feedback");
  status.setAttribute("role", "status");
  const save = element("button", "primary", "保存配置");
  save.type = "submit";

  for (const key of Object.keys(plugin.configSchema).sort()) {
    const field = plugin.configSchema[key];
    const label = element("label", field.type === "boolean" ? "check" : "field");
    const input = document.createElement("input");
    input.type = field.type === "boolean" ? "checkbox" : field.secret ? "password" : field.type;
    if (input.type === "number") input.step = "any";
    if (input.type === "password") {
      input.placeholder = "留空保留现有值";
      input.autocomplete = "new-password";
    }
    const title = element("span", "", key + (field.required ? " *" : "") + (field.secret ? "（密钥）" : ""));
    label.append(...(input.type === "checkbox" ? [input, title] : [title, input]));
    const property = input.type === "checkbox" ? "checked" : "value";
    fields.push({ key, field, input, property, baseline: input[property] });
    grid.append(label);
  }

  function update(config = {}) {
    for (const entry of fields) {
      const { key, field, input, property, baseline } = entry;
      const next = property === "checked" ? Boolean(config[key]) : field.secret ? "" : String(config[key] ?? "");
      if (input[property] === baseline && baseline !== next) input[property] = next;
      entry.baseline = next;
    }
  }

  update(plugin.config);
  form.append(grid, save, status);
  let saving = false;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (saving) return;
    const values = Object.create(null);
    const submitted = new Map();
    for (const { key, field, input, property } of fields) {
      const value = input[property];
      submitted.set(key, value);
      if (value === "" && (field.secret || field.type === "number")) continue;
      values[key] = field.type === "number" ? Number(value) : value;
    }
    saving = true;
    setBusy(save, true, "正在保存…");
    feedback(status, "");
    try {
      const saved = await api(`${pluginPath(plugin.id)}/config`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(values),
      });
      for (const entry of fields) {
        const { key, field, input, property } = entry;
        if (input[property] !== submitted.get(key)) continue;
        if (field.secret && property === "value") input.value = "";
        entry.baseline = input[property];
      }
      update(saved?.config);
      feedback(status, "配置已保存。", "success");
      await refresh(false);
    } catch (error) {
      feedback(status, `保存失败：${error.message}`, "error");
    } finally {
      saving = false;
      setBusy(save, false, "保存配置");
    }
  });
  return { form, update };
}

function formatTime(value) {
  return value ? new Date(value).toLocaleString() : "—";
}

function createRow() {
  const node = element("article", "plugin");
  const header = element("div", "plugin-head");
  const identity = element("div", "identity");
  const title = element("h3");
  const version = element("span", "caption");
  const badge = element("span", "badge");
  identity.append(title, version, badge);
  const actions = element("div", "actions");
  header.append(identity, actions);
  const error = element("p", "feedback error");
  const details = element("details");
  const summary = element("summary", "", "详情与操作");
  const body = element("div", "details-body");
  const metadata = element("dl", "metadata");
  const more = element("div", "actions");
  details.append(summary, body);
  body.append(metadata, more);
  node.append(header, error, details);

  let signature;
  let schema;
  let config;
  let pending = false;

  function update(plugin) {
    title.textContent = plugin.name;
    version.textContent = `v${plugin.version}`;
    badge.dataset.state = plugin.enabled ? plugin.status : "disabled";
    badge.textContent = plugin.enabled ? statusLabels[plugin.status] || plugin.status : "未启用";
    error.textContent = plugin.error || "";

    metadata.replaceChildren();
    function add(label, value, code = false) {
      metadata.append(element("dt", "", label), element("dd", code ? "mono" : "", value));
    }
    add("插件 ID", plugin.id, true);
    add("安装时间", formatTime(plugin.installedAt));
    add("本次启动", formatTime(plugin.startedAt));
    if (plugin.hasWeb) add("Web 路径", plugin.webPath, true);
    if (plugin.hasLua) {
      add("API 路径", plugin.apiPath, true);
      add("事件路径", plugin.eventsPath, true);
    }
    if (plugin.logPath) add("日志路径", plugin.logPath, true);

    const nextSchema = JSON.stringify(plugin.configSchema || {});
    if (nextSchema !== schema) {
      if (config?.form.contains(document.activeElement)) summary.focus();
      config?.form.remove();
      config = Object.keys(plugin.configSchema || {}).length ? configForm(plugin) : null;
      if (config) body.insertBefore(config.form, more);
      schema = nextSchema;
    }
    config?.update(plugin.config);

    const nextSignature = JSON.stringify([plugin.enabled, plugin.status, plugin.webPath, plugin.name]);
    if (nextSignature === signature) return;
    signature = nextSignature;
    const active = document.activeElement;
    const restoreAction = actions.contains(active) || more.contains(active);
    actions.replaceChildren();
    more.replaceChildren();

    function lifecycle(label, action, style = "") {
      const button = actionButton(label, style, async () => {
        if (pending) return;
        pending = true;
        setBusy(button, true, `${label}中…`);
        try {
          await api(`${pluginPath(plugin.id)}/${action}`, { method: "POST" });
          feedback(notice, `${plugin.name}已${label === "重试" ? "启动" : label}。`, "success");
          await refresh(false);
        } catch (cause) {
          feedback(notice, `${label}失败：${cause.message}`, "error");
          await refresh(false);
        } finally {
          pending = false;
          setBusy(button, false, label);
        }
      });
      button.dataset.action = action;
      return button;
    }

    if (!plugin.enabled) {
      actions.append(lifecycle("启用", "enable", "primary"));
    } else if (plugin.status === "running" && plugin.hasWeb) {
      const link = element("a", "button primary", "打开");
      link.href = plugin.webPath;
      link.target = "_blank";
      link.rel = "noopener";
      link.dataset.action = "open";
      actions.append(link);
    } else if (plugin.status === "failed" || plugin.status === "stopped") {
      actions.append(lifecycle(plugin.status === "failed" ? "重试" : "启动", "start", "primary"));
    }
    if (plugin.enabled && plugin.status === "running") {
      (plugin.hasWeb ? more : actions).append(lifecycle("停止", "stop"));
    }
    if (plugin.enabled && !["starting", "stopping"].includes(plugin.status)) {
      more.append(lifecycle("禁用", "disable"));
    }
    const uninstall = actionButton("卸载", "danger-text", () => {
      if (!pending) confirmUninstall(plugin, uninstall);
    });
    uninstall.dataset.action = "uninstall";
    more.append(uninstall);

    if (restoreAction) {
      const equivalent = node.querySelector(`[data-action="${active.dataset.action}"]`);
      (equivalent || actions.firstElementChild || summary).focus();
    }
  }

  return { node, update };
}

function render(plugins) {
  countLabel.textContent = `${plugins.length} 个插件`;
  const ids = new Set(plugins.map((plugin) => plugin.id));
  for (const [id, row] of rows) {
    if (ids.has(id)) continue;
    if (row.node.contains(document.activeElement)) refreshButton.focus();
    row.node.remove();
    rows.delete(id);
  }

  const sorted = [...plugins].sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
  let next = pluginsRoot.firstElementChild;
  for (const plugin of sorted) {
    let row = rows.get(plugin.id);
    if (!row) {
      row = createRow();
      rows.set(plugin.id, row);
    }
    row.update(plugin);
    if (row.node !== next) {
      const active = document.activeElement;
      pluginsRoot.insertBefore(row.node, next);
      if (row.node.contains(active)) active.focus({ preventScroll: true });
    }
    next = row.node.nextElementSibling;
  }
}

async function refresh(clearNotice = true) {
  const version = ++refreshVersion;
  setBusy(refreshButton, true, "正在刷新…");
  feedback(listState, "正在加载插件…");
  try {
    const data = await api("/api/v1/extensions");
    if (version !== refreshVersion) return;
    if (clearNotice) feedback(notice, "");
    const plugins = data.plugins || [];
    render(plugins);
    feedback(listState, plugins.length ? "" : "还没有安装插件。请选择 ZIP 文件。");
  } catch (error) {
    if (version === refreshVersion) {
      feedback(listState, `无法读取插件列表：${error.message}。请刷新重试。`, "error");
    }
  } finally {
    if (version === refreshVersion) setBusy(refreshButton, false, "刷新");
  }
}

installForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const submit = installForm.querySelector("button");
  const file = packageInput.files[0];
  if (!file || submit.getAttribute("aria-disabled") === "true") return;
  setBusy(submit, true, "正在安装…");
  try {
    const form = new FormData();
    form.append("package", file);
    const plugin = await api("/api/v1/extensions", { method: "POST", body: form });
    if (packageInput.files[0] === file) packageInput.value = "";
    feedback(notice, `已安装 ${plugin.name} ${plugin.version}，当前未启用。`, "success");
    await refresh(false);
  } catch (error) {
    feedback(notice, `安装失败：${error.message}`, "error");
  } finally {
    setBusy(submit, false, "安装");
  }
});

refreshButton.addEventListener("click", () => {
  if (refreshButton.getAttribute("aria-disabled") !== "true") void refresh();
});
void refresh();
