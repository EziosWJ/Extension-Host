const pluginsRoot = document.querySelector("#plugins");
const countLabel = document.querySelector("#count");
const notice = document.querySelector("#notice");
const installForm = document.querySelector("#install-form");
const packageInput = document.querySelector("#package");
const refreshButton = document.querySelector("#refresh");

const STATUS_LABELS = {
  stopped: "已停止",
  starting: "启动中",
  running: "运行中",
  stopping: "停止中",
  failed: "启动失败",
};
const STATUS_CLASSES = {
  stopped: "off",
  starting: "pending",
  running: "running",
  stopping: "pending",
  failed: "failed",
};

let plugins = [];

async function api(path, options = {}) {
  const response = await fetch(path, options);
  const type = response.headers.get("content-type") || "";
  let body = null;
  if (response.status !== 204) {
    body = type.includes("application/json") ? await response.json() : await response.text();
  }
  if (!response.ok) {
    const message = body && typeof body === "object" ? body.error : body;
    throw new Error(message || response.statusText || `HTTP ${response.status}`);
  }
  return body;
}

function showNotice(message, kind = "error") {
  notice.hidden = false;
  notice.className = `notice ${kind}`;
  notice.textContent = message;
}

function hideNotice() {
  notice.hidden = true;
  notice.textContent = "";
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function badge(label, className) {
  return element("span", `badge ${className}`, label);
}

function formatTime(value) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function actionButton(label, className, onClick) {
  const button = element("button", className, label);
  button.type = "button";
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      await onClick();
    } finally {
      button.disabled = false;
    }
  });
  return button;
}

function actionPath(id, action, query = "") {
  return `/api/v1/extensions/${encodeURIComponent(id)}/${action}${query}`;
}

async function runAction(plugin, action, successMessage, query = "") {
  try {
    if (action === "uninstall") {
      await api(actionPath(plugin.id, action, query), { method: "DELETE" });
    } else {
      await api(actionPath(plugin.id, action), { method: "POST" });
    }
    showNotice(successMessage, "ok");
  } catch (error) {
    showNotice(error.message);
  }
  await refresh(false);
}

function configForm(plugin) {
  const schema = plugin.configSchema || {};
  const keys = Object.keys(schema).sort();
  if (keys.length === 0) return null;

  const fieldset = document.createElement("fieldset");
  fieldset.append(element("legend", "", "插件配置"));
  const grid = element("div", "detail-grid");
  const inputs = new Map();

  for (const key of keys) {
    const field = schema[key];
    const wrapper = element("div", "field");
    const label = element("label", "");
    label.append(document.createTextNode(key));
    if (field.required) {
      label.append(element("span", "required", " *"));
    }
    if (field.secret) {
      label.append(document.createTextNode("（密钥）"));
    }
    wrapper.append(label);

    let input;
    if (field.type === "boolean") {
      const check = element("div", "check");
      input = document.createElement("input");
      input.type = "checkbox";
      input.checked = Boolean(plugin.config[key]);
      check.append(input, document.createTextNode(field.default ? "默认开启" : "启用"));
      wrapper.append(check);
    } else {
      input = document.createElement("input");
      input.type = field.secret ? "password" : field.type === "number" ? "number" : "text";
      if (field.type === "number") input.step = "any";
      if (field.secret) {
        input.placeholder = "留空保留现有值";
        input.autocomplete = "new-password";
      } else if (plugin.config[key] !== undefined && plugin.config[key] !== null) {
        input.value = String(plugin.config[key]);
      }
      wrapper.append(input);
    }
    inputs.set(key, { input, field });
    grid.append(wrapper);
  }

  fieldset.append(grid);
  const actions = element("div", "config-actions");
  const save = actionButton("保存配置", "", async () => {
    const values = {};
    for (const [key, { input, field }] of inputs) {
      if (field.type === "boolean") {
        values[key] = input.checked;
      } else if (field.secret) {
        if (input.value !== "") values[key] = input.value;
      } else if (field.type === "number") {
        if (input.value !== "") values[key] = Number(input.value);
      } else {
        values[key] = input.value;
      }
    }
    try {
      await api(actionPath(plugin.id, "config"), {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(values),
      });
      showNotice("配置已保存。", "ok");
      await refresh(false);
    } catch (error) {
      showNotice(error.message);
    }
  });
  actions.append(save, element("span", "muted", "密钥值不会出现在管理响应中。"));
  fieldset.append(actions);
  return fieldset;
}

function pluginCard(plugin) {
  const card = element("article", "plugin");

  const head = element("div", "plugin-head");
  const title = element("div", "");
  title.append(element("h3", "plugin-title", plugin.name));
  title.append(element("p", "plugin-id", `${plugin.id} · v${plugin.version}`));
  const badges = element("div", "badges");
  badges.append(badge(plugin.enabled ? "已启用" : "未启用", plugin.enabled ? "on" : "off"));
  badges.append(badge(STATUS_LABELS[plugin.status] || plugin.status, STATUS_CLASSES[plugin.status] || "off"));
  head.append(title, badges);
  card.append(head);

  if (plugin.error) {
    card.append(element("p", "error-box", plugin.error));
  }

  const paths = element("div", "paths");
  if (plugin.hasWeb) {
    const item = element("span", "");
    item.append("Web: ", element("code", "", plugin.webPath));
    paths.append(item);
  }
  if (plugin.hasLua) {
    const api = element("span", "");
    api.append("API: ", element("code", "", plugin.apiPath));
    paths.append(api);
    const events = element("span", "");
    events.append("事件: ", element("code", "", plugin.eventsPath));
    paths.append(events);
  }
  if (!plugin.hasWeb && !plugin.hasLua) {
    paths.append(element("span", "", "无入口"));
  }
  card.append(paths);

  const actions = element("div", "actions");
  if (!plugin.enabled) {
    actions.append(actionButton("启用", "", () => runAction(plugin, "enable", `已启用 ${plugin.name}。`)));
  } else {
    if (plugin.status !== "running" && plugin.status !== "starting") {
      actions.append(actionButton("启动", "", () => runAction(plugin, "start", `已启动 ${plugin.name}。`)));
    }
    if (plugin.status === "running" || plugin.status === "starting") {
      actions.append(actionButton("停止", "ghost", () => runAction(plugin, "stop", `已停止 ${plugin.name}，启用意图保留。`)));
    }
    actions.append(actionButton("禁用", "ghost", () => runAction(plugin, "disable", `已禁用 ${plugin.name}。`)));
  }
  if (plugin.hasWeb && plugin.status === "running") {
    const link = element("a", "button-link", "打开插件页面");
    link.href = plugin.webPath;
    link.target = "_blank";
    link.rel = "noopener";
    actions.append(link);
  }
  actions.append(actionButton("卸载", "danger", () => {
    if (!window.confirm(`卸载 ${plugin.name}？默认保留业务数据。`)) return;
    return runAction(plugin, "uninstall", `已卸载 ${plugin.name}，业务数据保留。`);
  }));
  actions.append(actionButton("卸载并清除数据", "danger", () => {
    if (!window.confirm(`卸载 ${plugin.name} 并删除业务数据库？此操作不可恢复。`)) return;
    return runAction(plugin, "uninstall", `已卸载 ${plugin.name}，业务数据已清除。`, "?purge=true");
  }));
  card.append(actions);

  const details = element("div", "detail-grid");
  const installed = element("div", "field");
  installed.append(element("label", "", "安装时间"), element("div", "", formatTime(plugin.installedAt)));
  const started = element("div", "field");
  started.append(element("label", "", "本次启动"), element("div", "", formatTime(plugin.startedAt)));
  details.append(installed, started);
  card.append(details);

  const form = configForm(plugin);
  if (form) card.append(form);

  card.append(element("p", "log-path", `日志：${plugin.logPath}`));
  return card;
}

function render() {
  countLabel.textContent = `${plugins.length} 个插件`;
  pluginsRoot.replaceChildren();
  if (plugins.length === 0) {
    pluginsRoot.append(element("div", "empty-state", "还没有安装插件。选择一个 ZIP 开始。"));
    return;
  }
  const sorted = [...plugins].sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
  for (const plugin of sorted) {
    pluginsRoot.append(pluginCard(plugin));
  }
}

async function refresh(clearNotice = true) {
  refreshButton.disabled = true;
  try {
    const data = await api("/api/v1/extensions");
    plugins = data.plugins || [];
    if (clearNotice) hideNotice();
    render();
  } catch (error) {
    showNotice(`无法读取插件列表：${error.message}`);
  } finally {
    refreshButton.disabled = false;
  }
}

installForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const file = packageInput.files[0];
  if (!file) return;
  const submit = installForm.querySelector("button");
  submit.disabled = true;
  try {
    const form = new FormData();
    form.append("package", file);
    const plugin = await api("/api/v1/extensions", { method: "POST", body: form });
    packageInput.value = "";
    showNotice(`已安装 ${plugin.name} ${plugin.version}，当前未启用。`, "ok");
    await refresh(false);
  } catch (error) {
    showNotice(`安装失败：${error.message}`);
  } finally {
    submit.disabled = false;
  }
});

refreshButton.addEventListener("click", () => refresh());
refresh();
