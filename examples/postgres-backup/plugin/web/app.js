let hostThemeObserver;
function syncHostTheme() {
  const root = document.documentElement;
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  const hostTheme = () => {
    try {
      if (window.parent === window) return "";
      return window.parent.document.documentElement.dataset.theme || "";
    } catch {
      return "";
    }
  };
  const sync = () => {
    const theme = hostTheme();
    root.dataset.theme = theme === "dark" || theme === "light" ? theme : media.matches ? "dark" : "light";
  };
  sync();
  media.addEventListener?.("change", sync);
  try {
    if (window.parent !== window) {
      hostThemeObserver = new MutationObserver(sync);
      hostThemeObserver.observe(window.parent.document.documentElement, {
        attributes: true,
        attributeFilter: ["data-theme"],
      });
    }
  } catch {
    // 父页面不可同源访问时继续跟随系统外观。
  }
}
syncHostTheme();

const apiRoot = new URL("../api/", location.href);
const eventsURL = new URL("../events", location.href);
eventsURL.protocol = location.protocol === "https:" ? "wss:" : "ws:";

const $ = (id) => document.getElementById(id);
const app = $("app");
const dialog = $("dialog");
const state = { view: "sources", step: "list", sources: [], schedules: [], backups: [], restores: [], source: "", database: "" };
const catalogs = new Map();
const schemas = new Map();
const dateFormat = new Intl.DateTimeFormat("zh-CN", { dateStyle: "short", timeStyle: "medium" });
const labels = {
  running: "运行中", succeeded: "成功", failed: "失败", skipped: "已跳过", deleted: "文件已删除",
  preflight: "正在预检", creating: "正在创建临时库", restoring: "正在恢复", ready: "待确认切换",
  switching: "正在切换", needs_review: "待核对", checking: "正在核对", rolling_back: "正在切回",
  rolled_back: "已切回", cleaning: "正在清理", cancelled: "已取消",
};
const rowsOf = (value) => Array.isArray(value) ? value : [];
const sourceById = (id) => state.sources.find((source) => source.id === id);
const date = (value) => value && Number.isFinite(Date.parse(value)) ? dateFormat.format(new Date(value)) : "—";
const schemaKey = (source, database) => JSON.stringify([source, database]);

const iconPaths = {
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  database: '<ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5V19A9 3 0 0 0 21 19V5"/><path d="M3 12A9 3 0 0 0 21 12"/>',
  server: '<rect width="20" height="8" x="2" y="2" rx="2" ry="2"/><rect width="20" height="8" x="2" y="14" rx="2" ry="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>',
  archive: '<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>',
  clock: '<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>',
  rotate: '<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/>',
  refresh: '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
  arrowLeft: '<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  play: '<polygon points="6 3 20 12 6 21 6 3"/>',
  pencil: '<path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z"/><path d="m15 5 4 4"/>',
  trash: '<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/><line x1="10" x2="10" y1="11" y2="17"/><line x1="14" x2="14" y1="11" y2="17"/>',
};

function icon(name, className = "icon") {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", "16");
  svg.setAttribute("height", "16");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "2");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  if (className) svg.setAttribute("class", className);
  svg.innerHTML = iconPaths[name] || "";
  return svg;
}

function node(tag, className = "", label = "") {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (label) element.textContent = label;
  return element;
}

function button(label, action, className = "", iconName = "") {
  const element = node("button", className, label);
  if (iconName) element.prepend(icon(iconName));
  element.type = "button";
  element.addEventListener("click", action);
  return element;
}

function feedback(element, message, error = false) {
  element.hidden = !message;
  element.textContent = message || "";
  element.classList.toggle("error", error);
}

let toastTimer;
function showToast(message, error = false) {
  if (!message) return;
  clearTimeout(toastTimer);
  const content = $("toast").firstElementChild;
  content.classList.toggle("error", error);
  content.textContent = message;
  toastTimer = setTimeout(() => { content.textContent = ""; }, error ? 6000 : 3000);
}

function loadingIndicator(message) {
  const status = node("p", "loading-line");
  status.setAttribute("role", "status");
  const spinner = node("span", "spinner");
  spinner.setAttribute("aria-hidden", "true");
  status.append(spinner, message);
  return status;
}

async function request(path, method = "GET", body) {
  const options = { method, cache: "no-store" };
  if (body) {
    options.headers = { "Content-Type": "application/json" };
    options.body = JSON.stringify(body);
  }
  const response = await fetch(new URL(path, apiRoot), options);
  const data = await response.json().catch(() => null);
  if (!response.ok || !data) throw new Error(data?.error || data?.message || `请求失败（HTTP ${response.status}）。`);
  return data;
}

async function operation(path, method, body) {
  let result = await request(path, method, body);
  if (!result.operation) return result;
  const url = `operation?id=${encodeURIComponent(result.operation)}`;
  const limit = Date.now() + 45_000;
  do {
    await new Promise((resolve) => setTimeout(resolve, 200));
    result = await request(url);
    if (result.status === "failed") throw new Error(result.error);
    if (result.status === "succeeded") return result.data;
  } while (Date.now() < limit);
  throw new Error("宿主响应超时，请刷新状态后再试。");
}

async function run(trigger, action, message) {
  trigger.disabled = true;
  try {
    await action();
    showToast(message);
    refreshInBackground();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    trigger.disabled = false;
  }
}

let fieldId = 0;
function field(content, label, name, { type = "text", value = "", options, ...attributes } = {}) {
  const wrapper = node("div", "field");
  const input = node(options ? "select" : "input");
  input.name = name;
  if (options) {
    for (const [key, text] of options) input.add(new Option(text, key));
  } else {
    input.type = type;
  }
  for (const [key, item] of Object.entries(attributes)) input[key] = item;
  input.value = value;
  input.id = `field-${++fieldId}`;
  const caption = node("label", "", label);
  caption.htmlFor = input.id;
  wrapper.append(caption, input);
  content.append(wrapper);
  return input;
}

function check(content, label, name, checked = false) {
  const wrapper = node("label", "check");
  const input = node("input");
  input.type = "checkbox";
  input.name = name;
  input.checked = checked;
  wrapper.append(input, node("span", "", label));
  content.append(wrapper);
  return input;
}

let submitDialog;
let dialogBusy = false;
let dialogTrigger;

function openDialog(trigger, title, submitLabel, build, submit, dangerous = false) {
  dialogTrigger = trigger;
  $("dialog-title").textContent = title;
  $("confirm").textContent = submitLabel;
  $("confirm").className = dangerous ? "danger" : "primary";
  feedback($("dialog-status"), "");
  $("dialog-content").replaceChildren();
  const first = build($("dialog-content"));
  submitDialog = submit;
  dialog.showModal();
  (dangerous ? $("cancel") : first ?? $("cancel")).focus();
}

$("cancel").addEventListener("click", () => dialog.close());
dialog.addEventListener("cancel", (event) => { if (dialogBusy) event.preventDefault(); });
dialog.addEventListener("close", () => {
  $("dialog-content").replaceChildren();
  if (dialogTrigger?.isConnected && dialogTrigger.getClientRects().length) dialogTrigger.focus();
  else $("menu").querySelector("[aria-current]")?.focus();
});
$("dialog-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (dialogBusy) return;
  dialogBusy = true;
  $("confirm").disabled = $("cancel").disabled = true;
  feedback($("dialog-status"), "正在处理…");
  try {
    const data = new FormData(event.currentTarget);
    const message = await submitDialog(data);
    dialog.close();
    showToast(message);
    refreshInBackground();
  } catch (error) {
    feedback($("dialog-status"), error.message, true);
  } finally {
    dialogBusy = false;
    $("confirm").disabled = $("cancel").disabled = false;
  }
});

function confirmAction(trigger, title, message, action, { value, dangerous = true } = {}) {
  openDialog(trigger, title, title, (content) => {
    content.append(node("p", "", message));
    if (value) field(content, `输入数据库名称 ${value}`, "confirm", { required: true, autocomplete: "off" });
  }, (data) => action(data.get("confirm")), dangerous);
}

const minimumLoading = () => new Promise((resolve) => setTimeout(resolve, 150));
let viewLoad = 0;

function showSourceList(open) {
  app.toggleAttribute("data-sources-open", open);
  $("sources-toggle").setAttribute("aria-expanded", String(open));
}

async function selectView(view, step, { reload = false, focus = false } = {}) {
  const load = view !== state.view || reload;
  state.view = app.dataset.view = view;
  state.step = app.dataset.step = step;
  showSourceList(false);
  if (load) {
    state.loading = true;
    state.loadError = "";
  }
  render();
  if (load) {
    const current = ++viewLoad;
    const [error] = await Promise.all([refresh().then(() => "", (failure) => failure.message), minimumLoading()]);
    if (current !== viewLoad) return;
    state.loading = false;
    state.loadError = error;
    renderLayout();
  }
  if (focus && !state.loading) {
    if (state.loadError) $("retry-view").focus();
    else focusHeading(state.view === "sources" ? (state.step === "detail" && $("detail").getClientRects().length ? "detail" : "databases") : "page");
  }
}

function heading(title, ...actions) {
  const bar = node("header", "toolbar");
  const titleNode = node("h2", "", title);
  titleNode.tabIndex = -1;
  const controls = node("div", "actions");
  controls.append(...actions);
  bar.append(titleNode, controls);
  return bar;
}

function focusHeading(pane) {
  $(pane).querySelector("h2")?.focus();
}

function backToDatabases() {
  state.step = app.dataset.step = "list";
  ($("databases").querySelector("[aria-current]") || $("databases").querySelector("h2"))?.focus();
}

function renderNavigation() {
  const search = $("source-search").value.trim().toLocaleLowerCase();
  const matching = state.sources.filter((source) =>
    [source.name, source.service, `${source.host}:${source.port}`].some((text) => text.toLocaleLowerCase().includes(search)));
  listInto("sources", matching.map((source) => {
    const item = button("", () => {
      if (state.source !== source.id) state.database = "";
      state.source = source.id;
      void selectView("sources", "list", { focus: true });
      if (!catalogs.has(source.id)) void probe(source.id);
    });
    item.dataset.key = source.id;
    const text = node("span", "item-text");
    text.append(
      node("span", "item-title", source.name),
      node("span", "meta mono numeric", source.service || `${source.host}:${source.port}`),
    );
    item.append(icon("server", "item-icon"), text);
    if (state.view === "sources" && state.source === source.id) item.setAttribute("aria-current", "true");
    return item;
  }), state.sources.length ? "没有匹配的数据源。" : "尚无数据源");
  for (const item of $("menu").children) {
    if (item.dataset.view === state.view) item.setAttribute("aria-current", "page");
    else item.removeAttribute("aria-current");
  }
}

function listInto(id, children, empty) {
  $(id).replaceChildren(...(children.length ? children : [node("p", "muted", empty)]));
}

function paint(element, content) {
  const active = element.contains(document.activeElement) && document.activeElement;
  const key = active?.dataset?.key;
  const label = active?.textContent;
  element.replaceChildren(...content);
  if (active) {
    const replacement = Array.from(element.querySelectorAll("button")).find((item) =>
      key ? item.dataset.key === key : item.textContent === label);
    (replacement || element.querySelector("h2") || $("add-source")).focus({ preventScroll: true });
  }
}

async function probe(source, database) {
  const cache = database ? schemas : catalogs;
  const key = database ? schemaKey(source, database) : source;
  const entry = { status: "loading" };
  cache.set(key, entry);
  renderSources();
  try {
    const data = await operation("probe", "POST", { source, database });
    Object.assign(entry, { status: "ready", rows: rowsOf(data.rows) });
  } catch (error) {
    Object.assign(entry, { status: "failed", error: error.message });
  }
  if (cache.get(key) === entry) renderSources();
  return entry;
}

function probeState(content, result, empty) {
  if (result?.status === "failed") content.append(node("p", "error", result.error));
  else if (!result || result.status === "loading") content.append(loadingIndicator("正在探测…"));
  else if (!result.rows.length) content.append(node("p", "muted", empty));
}

function renderSources() {
  if (state.view !== "sources") return;
  const source = sourceById(state.source);
  const items = node("div", "content items");
  const detail = node("div", "content");
  if (!source) {
    items.append(node("p", "muted", state.sources.length ? "请选择一个数据源。" : "新建数据源后可以探测数据库。"));
    paint($("databases"), [heading("数据库"), items]);
    paint($("detail"), [heading("数据库详情")]);
    return;
  }
  const catalog = catalogs.get(source.id);
  const reload = button("探测数据库", () => probe(source.id), "primary", "refresh");
  reload.disabled = catalog?.status === "loading";
  const edit = button("编辑连接", (event) => editSource(event.currentTarget, source), "", "pencil");
  probeState(items, catalog, "该账号没有可连接的数据库。");
  for (const entry of catalog?.rows || []) {
    const item = button("", () => {
      state.database = entry.name;
      state.step = app.dataset.step = "detail";
      renderSources();
      focusHeading("detail");
      if (!schemas.has(schemaKey(source.id, entry.name))) void probe(source.id, entry.name);
    });
    item.dataset.key = entry.name;
    const text = node("span", "item-text");
    text.append(node("span", "item-title mono", entry.name), node("span", "meta", `属主 ${entry.owner}`));
    item.append(icon("database", "item-icon"), text);
    if (entry.name === state.database) item.setAttribute("aria-current", "true");
    items.append(item);
  }
  paint($("databases"), [heading(source.name, edit, reload), items]);
  const database = state.database;
  if (!database) {
    detail.append(node("p", "muted", "选择数据库以查看 Schema 和备份记录。"));
    paint($("detail"), [heading("数据库详情"), detail]);
    return;
  }
  const rows = schemas.get(schemaKey(source.id, database));
  const targetBack = button("返回数据库", backToDatabases, "back-db", "arrowLeft");
  const jobs = state.backups.filter((job) => job.source_id === source.id && job.database_name === database);
  const backup = button("立即备份", (event) => run(event.currentTarget,
    () => request("backups", "POST", { source: source.id, database }), "备份任务已开始。"), "primary", "archive");
  backup.disabled = jobs.some((job) => job.status === "running");
  const schemaSection = node("section", "details-section schema-group");
  const schemaHead = node("header", "schema-head");
  schemaHead.append(node("h3", "", "Schema"));
  if (rows?.status === "ready") {
    schemaHead.append(node("span", "schema-count", String(rows.rows.length)));
  }
  const reloadSchemas = button("刷新 Schema", () => probe(source.id, database), "", "refresh");
  reloadSchemas.disabled = rows?.status === "loading";
  schemaHead.append(reloadSchemas);
  schemaSection.append(schemaHead);
  const schemaBody = node("div", "schema-body");
  probeState(schemaBody, rows, "尚无用户 Schema。");
  const schemaList = node("ul", "schema-rows");
  for (const entry of rows?.rows || []) {
    const row = node("li", "schema-row");
    row.append(node("span", "schema-name", entry.name), node("span", "schema-owner", entry.owner));
    schemaList.append(row);
  }
  if (schemaList.childElementCount) schemaBody.append(schemaList);
  schemaSection.append(schemaBody);
  const recordSection = node("section", "details-section");
  recordSection.append(node("h3", "", "备份记录"), backupList(jobs));
  detail.append(schemaSection, recordSection);
  paint($("detail"), [heading(database, targetBack, backup), detail]);
}

function editSource(trigger, source) {
  const affected = state.schedules.filter((job) => job.source_id === source?.id);
  openDialog(trigger, source ? "编辑数据源" : "新建数据源", "验证并保存", (content) => {
    const name = field(content, "名称", "name", { value: source?.name, required: true, maxLength: 160 });
    const mode = field(content, "连接方式", "mode", {
      value: source?.service ? "service" : "host", options: [["host", "主机与端口"], ["service", "PostgreSQL 服务名"]],
    });
    const direct = node("div", "fields");
    const host = field(direct, "主机", "host", { value: source?.host, placeholder: "127.0.0.1" });
    const port = field(direct, "端口", "port", { type: "number", value: source?.port || 5432, min: 1, max: 65535 });
    const username = field(direct, "用户名", "username", { value: source?.username });
    content.append(direct);
    const service = field(content, "服务名", "service", { value: source?.service });
    const database = field(content, "入口数据库", "database", { value: source?.database ?? "postgres", maxLength: 63 });
    const password = field(content, "密码", "password", {
      type: "password", autocomplete: "new-password", placeholder: source?.passwordSaved ? "留空保留已保存的密码" : "可留空",
    });
    if (source?.passwordSaved) {
      const clear = check(content, "清除已保存的密码", "clearPassword");
      clear.addEventListener("change", () => { password.disabled = clear.checked; });
    }
    const impact = node("div");
    let confirm;
    if (affected.length) {
      impact.append(node("p", "attention", "以下定时任务将使用新的连接地址"));
      const tasks = node("ul", "records");
      for (const job of affected) tasks.append(node("li", "record", `${job.name}（${job.database_name}）`));
      impact.append(tasks);
      confirm = check(impact, "确认更改连接地址", "confirmEndpoint");
      content.append(impact);
    }
    function update() {
      const named = mode.value === "service";
      direct.hidden = named;
      for (const input of [host, port, username]) input.disabled = named, input.required = !named;
      service.closest(".field").hidden = !named;
      service.disabled = !named;
      service.required = named;
      database.required = !named;
      if (confirm) {
        const changed = named ? service.value !== source.service :
          Boolean(source.service) || host.value !== source.host || Number(port.value) !== source.port;
        impact.hidden = !changed;
        confirm.required = changed;
      }
    }
    for (const input of [mode, host, port, service]) input.addEventListener("input", update);
    update();
    if (source) {
      const remove = button("删除数据源", () => {
        dialog.close();
        window.setTimeout(() => confirmAction(trigger, "删除数据源",
          `删除「${source.name}」。已有备份记录和文件会保留。`, async () => {
            await request("sources", "DELETE", { id: source.id });
            catalogs.delete(source.id);
            state.source = state.database = "";
            void selectView("sources", "list");
            return "数据源已删除。";
          }), 0);
      }, "danger-ghost", "trash");
      remove.disabled = affected.length > 0;
      content.append(remove);
      if (affected.length) content.append(node("p", "meta", "删除前请先删除或改绑关联定时任务。"));
    }
    return name;
  }, async (data) => {
    const payload = { ...Object.fromEntries(data), id: source?.id, port: Number(data.get("port") || 5432),
      clearPassword: data.has("clearPassword"), confirmEndpoint: data.has("confirmEndpoint") };
    const saved = await operation("sources", "PUT", payload);
    state.sources = [...state.sources.filter((item) => item.id !== saved.id), saved];
    state.source = saved.id;
    state.database = "";
    void selectView("sources", "list");
    for (const key of schemas.keys()) if (JSON.parse(key)[0] === saved.id) schemas.delete(key);
    void probe(saved.id);
    return "数据源已保存。";
  });
}

const statusTones = {
  succeeded: "success", rolled_back: "success",
  running: "info", preflight: "info", creating: "info", restoring: "info",
  switching: "info", checking: "info", rolling_back: "info", cleaning: "info",
  skipped: "warning", ready: "warning", needs_review: "warning",
  failed: "danger",
  deleted: "muted", cancelled: "muted",
};
const busyStatuses = new Set(["running", "preflight", "creating", "restoring", "switching", "checking", "rolling_back", "cleaning"]);

function pill(text, tone, busy = false) {
  const element = node("span", `pill pill-${tone}`, text);
  const mark = node("span", busy ? "pill-spinner" : "pill-dot");
  mark.setAttribute("aria-hidden", "true");
  element.prepend(mark);
  return element;
}

function badge(status) {
  return pill(labels[status] || status || "—", statusTones[status] || "muted", busyStatuses.has(status));
}

function recordTitle(iconName, text) {
  const title = node("h3", "record-title");
  title.append(icon(iconName), node("span", "", text));
  return title;
}

function rowDetails(entries) {
  const list = node("dl");
  for (const [label, value, className] of entries) {
    if (value) list.append(node("dt", "", label), node("dd", className || "", value));
  }
  return list;
}

function backupList(jobs) {
  if (!jobs.length) return node("p", "muted", "暂无备份记录。");
  const list = node("ul", "records");
  for (const job of jobs) {
    const row = node("li", "record");
    row.dataset.status = job.status;
    const top = node("div", "row-top");
    top.append(recordTitle("archive", job.target), badge(job.status));
    const trigger = { scheduled: "定时执行", schedule_manual: "任务手动执行", manual: "手动备份" };
    row.append(top, rowDetails([
      ["开始时间", date(job.started_at), "numeric"], ["触发方式", trigger[job.trigger]],
      ["归档", job.archive_path, "path-label"], ["详情", job.error, "error"],
    ]));
    if (job.status === "succeeded") {
      const actions = node("div", "actions");
      const recover = button("恢复", (event) => editRestore(event.currentTarget, job), "", "rotate");
      recover.dataset.key = `restore-${job.id}`;
      const remove = button("删除归档", (event) => confirmAction(event.currentTarget, "删除归档",
        `将删除 ${job.archive_path}。此文件将无法用于恢复，执行记录会保留。`, async () => {
          await request("backups", "DELETE", { id: job.id });
          return "归档已删除。";
        }), "danger-ghost", "trash");
      remove.dataset.key = `delete-${job.id}`;
      actions.append(recover, remove);
      row.append(actions);
    }
    list.append(row);
  }
  return list;
}

function sourceOptions() {
  return state.sources.map((source) => [source.id, source.name]);
}

function editSchedule(trigger, schedule) {
  openDialog(trigger, schedule ? "编辑定时任务" : "新建定时任务", "保存定时任务", (content) => {
    const name = field(content, "任务名称", "name", { value: schedule?.name, required: true, maxLength: 160 });
    const source = field(content, "数据源", "source", {
      options: [["", "选择数据源"], ...sourceOptions()], value: schedule?.source_id || state.source, required: true,
    });
    const database = field(content, "数据库", "database", { options: [["", "请选择数据源"]], required: true });
    const lookup = node("p", "meta");
    lookup.setAttribute("role", "status");
    content.append(lookup);
    let token = 0;
    async function load(selected) {
      const requestNumber = ++token;
      const id = source.value;
      database.disabled = true;
      database.replaceChildren(new Option(id ? "正在探测…" : "请选择数据源", ""));
      feedback(lookup, "");
      if (!id) return;
      let list = catalogs.get(id);
      if (list?.status !== "ready") list = await probe(id);
      if (requestNumber !== token) return;
      database.disabled = false;
      database.replaceChildren(new Option("选择数据库", ""));
      for (const row of list.rows || []) database.add(new Option(row.name, row.name));
      if (selected && ![...database.options].some((option) => option.value === selected)) {
        database.add(new Option(`${selected}（原任务目标）`, selected));
      }
      database.value = selected || "";
      if (list.error) feedback(lookup, list.error, true);
    }
    source.addEventListener("change", () => void load(""));
    void load(schedule?.database_name || (state.source === source.value ? state.database : ""));
    const previous = schedule?.expression?.match(/^(\d{1,2}) (\d{1,2}) \* \* (\*|[0-6])$/);
    const initialMode = previous ? (previous[3] === "*" ? "daily" : "weekly") : schedule ? "advanced" : "daily";
    const mode = field(content, "周期", "period", {
      options: [["daily", "每天"], ["weekly", "每周"], ["advanced", "高级表达式"]], value: initialMode,
    });
    const time = field(content, "执行时间", "time", {
      type: "time", value: previous ? `${previous[2].padStart(2, "0")}:${previous[1].padStart(2, "0")}` : "02:00",
    });
    const days = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
    const weekday = field(content, "星期", "weekday", { options: days.map((day, index) => [index, day]), value: previous?.[3] === "*" ? "1" : previous?.[3] || "1" });
    const expression = field(content, "计划表达式", "expression", {
      value: schedule?.expression || "", placeholder: "0 2 * * * 或 @every 24h", maxLength: 256,
    });
    function update() {
      const advanced = mode.value === "advanced";
      for (const [input, show] of [[time, !advanced], [weekday, mode.value === "weekly"], [expression, advanced]]) {
        input.closest(".field").hidden = input.disabled = !show;
        input.required = show;
      }
    }
    mode.addEventListener("change", update);
    update();
    content.append(node("p", "meta", "按宿主本地时区执行。停机期间错过的执行不补跑。"));
    check(content, "启用任务", "enabled", schedule?.enabled !== 0);
    return name;
  }, async (data) => {
    if (!data.get("database")) throw new Error("请等待探测完成并选择数据库。");
    let expression = data.get("expression")?.trim();
    if (data.get("period") !== "advanced") {
      const [hour, minute] = data.get("time").split(":").map(Number);
      expression = `${minute} ${hour} * * ${data.get("weekday") ?? "*"}`;
    }
    await request("schedules", "PUT", { id: schedule?.id, name: data.get("name"), source: data.get("source"),
      database: data.get("database"), expression, enabled: data.has("enabled") });
    return "定时任务已保存。";
  });
}

function schedulesPane() {
  const add = button("新建定时任务", (event) => editSchedule(event.currentTarget), "primary", "plus");
  add.disabled = state.sources.length === 0;
  const content = node("div", "content");
  const list = node("ul", "records");
  for (const schedule of state.schedules) {
    const row = node("li", "record");
    const top = node("div", "row-top");
    top.append(
      recordTitle("clock", schedule.name),
      pill(schedule.enabled ? "已启用" : "已停用", schedule.enabled ? "success" : "muted"),
    );
    row.append(top, rowDetails([
      ["备份目标", `${sourceById(schedule.source_id)?.name || schedule.source_id} / ${schedule.database_name || "服务默认数据库"}`],
      ["计划表达式", schedule.expression, "mono"],
      ["下次执行", schedule.enabled ? (schedule.next_at ? date(schedule.next_at) : "暂无时间") : "—", "numeric"],
      ["最近执行", schedule.last_status ? `${labels[schedule.last_status]}（${date(schedule.last_at)}）` : "—", "numeric"],
      ["调度错误", schedule.error, "error"],
    ]));
    const actions = node("div", "actions");
    const start = button("立即运行", (event) => run(event.currentTarget,
      () => request("run-schedule", "POST", { id: schedule.id }), "备份任务已开始。"), "", "play");
    const edit = button("编辑", (event) => editSchedule(event.currentTarget, schedule), "", "pencil");
    const remove = button("删除", (event) => confirmAction(event.currentTarget, "删除定时任务",
      `将停止「${schedule.name}」后续的执行。已有备份会保留。`, async () => {
        await request("schedules", "DELETE", { id: schedule.id });
        return "定时任务已删除。";
      }), "danger-ghost", "trash");
    for (const [action, key] of [[start, "run"], [edit, "edit"], [remove, "delete"]]) action.dataset.key = `${key}-${schedule.id}`;
    actions.append(start, edit, remove);
    row.append(actions);
    list.append(row);
  }
  content.append(list.childElementCount ? list : node("p", "muted", state.sources.length ? "暂无定时任务。" : "请先新建一个数据源。"));
  return [heading("定时任务", add), content];
}

function editRestore(trigger, job) {
  openDialog(trigger, "恢复数据库", "开始恢复", (content) => {
    content.append(rowDetails([["备份", job.target], ["备份时间", date(job.started_at), "numeric"]]));
    const source = field(content, "目标数据源", "source", {
      options: [["", "选择数据源"], ...sourceOptions()], value: sourceById(job.source_id) ? job.source_id : "", required: true,
    });
    const mode = field(content, "恢复方式", "mode", {
      options: [["new", "恢复到新数据库"], ["replace", "替换现有数据库"]], value: "new",
    });
    const database = field(content, "目标数据库名称", "database", {
      required: true, maxLength: 63, autocomplete: "off",
    });
    const control = field(content, "管理数据库", "control", { required: true, maxLength: 63 });
    const hint = node("p", "meta");
    content.append(hint);
    function updateControl() {
      const preferred = sourceById(source.value)?.database || "postgres";
      control.value = preferred === database.value ? (preferred === "postgres" ? "template1" : "postgres") : preferred;
    }
    function updateMode() {
      hint.textContent = mode.value === "replace"
        ? "先恢复到临时数据库。成功后，到「恢复记录」确认切换。"
        : "备份将恢复为一个新数据库。请使用尚未存在的数据库名称。";
    }
    source.addEventListener("change", updateControl);
    database.addEventListener("change", () => { if (database.value === control.value) updateControl(); });
    mode.addEventListener("change", updateMode);
    updateControl();
    updateMode();
    check(content, "将对象归目标连接用户，跳过原对象授权", "portable");
    return source;
  }, async (data) => {
    await request("restores", "POST", { ...Object.fromEntries(data), backup: job.id, portable: data.has("portable") });
    void selectView("restores", "page", { focus: true });
    return "恢复任务已开始。";
  });
}

function restoreButtons(job) {
  const actions = node("div", "actions");
  function add(label, action, message, { className = "", iconName = "" } = {}) {
    const control = button(label, (event) => {
      const send = (value) => request("restore-action", "POST", { id: job.id, action, confirm: value });
      if (!message) {
        void run(event.currentTarget, () => send(), "正在核对数据库状态。");
      } else {
        confirmAction(event.currentTarget, label, message, async (value) => {
          await send(value);
          return "操作已开始。";
        }, { value: job.database_name });
      }
    }, className, iconName);
    control.dataset.key = `${action}-${job.id}`;
    actions.append(control);
  }
  const target = `${job.source_name} / ${job.database_name}`;
  if (job.status === "ready") {
    add("确认切换", "switch", job.mode === "replace" ?
      `切换 ${target} 会断开现有连接。切换后将使用 ${date(job.backup_at)} 的备份数据，现有库将改名为 ${job.previous} 并保留。` :
      `将 ${date(job.backup_at)} 的备份发布到 ${target}。`, { className: "primary", iconName: "check" });
  }
  if (job.status === "needs_review") add("重新核对", "check", undefined, { iconName: "refresh" });
  if (job.status === "succeeded" && job.mode === "replace" && !job.cleaned_previous) {
    add("切回旧库", "rollback", `切回 ${target} 会断开现有连接。当前使用的数据库将改名为 ${job.displaced} 并保留。`, { iconName: "rotate" });
    add("删除保留旧库", "clean", `永久删除 ${job.source_name} 上的 ${job.previous}。此记录将无法切回旧库。`,
      { className: "danger-ghost", iconName: "trash" });
  } else if (job.status === "rolled_back" && !job.cleaned_displaced) {
    add("删除保留库", "clean", `永久删除 ${job.source_name} 上的 ${job.displaced}，其中包含切回前的写入。`,
      { className: "danger-ghost", iconName: "trash" });
  } else if (["ready", "failed"].includes(job.status) && job.stage_oid && !job.cleaned_stage) {
    add("删除临时库", "clean", `永久删除 ${job.source_name} 上的 ${job.stage}，放弃本次恢复。`,
      { className: "danger-ghost", iconName: "trash" });
  }
  return actions;
}

function restoresPane() {
  const content = node("div", "content");
  const list = node("ul", "records");
  for (const job of state.restores) {
    const row = node("li", "record");
    row.dataset.status = job.status;
    const top = node("div", "row-top");
    top.append(recordTitle("rotate", `${job.source_name} / ${job.database_name}`), badge(job.status));
    const review = job.status === "needs_review";
    const observed = (name, oid) => oid ? `${name}（OID ${oid}）` : `${name}（上次核对未发现）`;
    row.append(top, rowDetails([
      ["恢复方式", job.mode === "replace" ? "替换现有数据库" : "恢复到新数据库"],
      ["备份时间", date(job.backup_at), "numeric"], ["开始时间", date(job.started_at), "numeric"],
      ["目标库", review ? observed(job.database_name, job.observed_target) : "", "mono"],
      ["临时库", review ? observed(job.stage, job.observed_stage) : job.status === "ready" ? job.stage : "", "mono"],
      ["保留旧库", review ? observed(job.previous, job.observed_previous) :
        job.status === "succeeded" && job.mode === "replace" && !job.cleaned_previous ? job.previous : "", "mono"],
      ["切回保留库", review ? observed(job.displaced, job.observed_displaced) :
        job.status === "rolled_back" && !job.cleaned_displaced ? job.displaced : "", "mono"],
      ["详情", job.error, "error"],
    ]), restoreButtons(job));
    list.append(row);
  }
  content.append(list.childElementCount ? list : node("p", "muted", "暂无恢复记录。可从备份记录开始恢复。"));
  return [heading("恢复记录", refreshControl()), content];
}

let manualRefresh = false;
function refreshControl() {
  const refreshButton = button("", async () => {
    if (manualRefresh) return;
    manualRefresh = true;
    renderPage();
    try {
      await Promise.all([refresh(), minimumLoading()]);
      showToast("已刷新。");
    } catch (error) {
      showToast(error.message, true);
    } finally {
      manualRefresh = false;
      renderPage();
    }
  }, "refresh-control");
  refreshButton.dataset.key = "refresh-page";
  if (manualRefresh) {
    refreshButton.setAttribute("aria-disabled", "true");
    refreshButton.setAttribute("aria-busy", "true");
    const spinner = node("span", "spinner");
    spinner.setAttribute("aria-hidden", "true");
    refreshButton.append(spinner, "正在刷新…");
  } else {
    refreshButton.append(icon("refresh"), "刷新");
  }
  return refreshButton;
}

function renderPage() {
  if (state.view === "schedules") paint($("page"), schedulesPane());
  else if (state.view === "history") {
    const content = node("div", "content");
    content.append(backupList(state.backups));
    paint($("page"), [heading("备份记录", refreshControl()), content]);
  } else if (state.view === "restores") paint($("page"), restoresPane());
}

function renderLayout() {
  const sourceView = state.view === "sources";
  const blocked = state.loading || Boolean(state.loadError);
  $("view-loading").hidden = !state.loading;
  $("view-error").hidden = !state.loadError;
  $("view-error-message").textContent = state.loadError || "";
  $("databases").hidden = $("detail").hidden = blocked || !sourceView;
  $("page").hidden = blocked || sourceView;
}

function render() {
  paintSourcesNavigation();
  renderLayout();
  if (state.view === "sources") renderSources();
  else renderPage();
}

let refreshPromise;
let refreshAgain = false;
function refresh(queue = false) {
  if (refreshPromise) {
    if (queue) refreshAgain = true;
    return refreshPromise;
  }
  refreshPromise = (async () => {
    let lastError;
    do {
      refreshAgain = false;
      try {
        const [sources, schedules, backups, restores] = await Promise.all(
          ["sources", "schedules", "backups", "restores"].map((path) => request(path)),
        );
        const oldSources = state.sources;
        const oldPage = JSON.stringify([state.schedules, state.backups, state.restores]);
        state.sources = rowsOf(sources.sources);
        state.schedules = rowsOf(schedules.schedules);
        state.backups = rowsOf(backups.jobs);
        state.restores = rowsOf(restores.jobs);
        const sourceChanged = JSON.stringify(oldSources) !== JSON.stringify(state.sources);
        if (sourceChanged) paintSourcesNavigation();
        if (sourceChanged || oldPage !== JSON.stringify([state.schedules, state.backups, state.restores])) {
          if (state.view === "sources") renderSources();
          else renderPage();
        }
        lastError = null;
      } catch (error) {
        lastError = error;
      }
    } while (refreshAgain);
    if (lastError) throw lastError;
  })().finally(() => { refreshPromise = null; });
  return refreshPromise;
}

function refreshInBackground(queue = true) {
  void refresh(queue).catch(() => {});
}

function paintSourcesNavigation() {
  const key = $("sources").contains(document.activeElement) && document.activeElement.dataset.key;
  renderNavigation();
  if (key) ([...$("sources").children].find((item) => item.dataset.key === key) || $("add-source")).focus();
}

let eventsSocket;
let reconnect;
let fallback;
let delay = 1000;
function connectEvents() {
  const socket = new WebSocket(eventsURL);
  eventsSocket = socket;
  socket.addEventListener("open", () => {
    delay = 1000;
    refreshInBackground(false);
  });
  socket.addEventListener("message", ({ data }) => {
    if (JSON.parse(data).event === "backup-changed") refreshInBackground();
  });
  socket.addEventListener("close", () => {
    if (eventsSocket !== socket) return;
    reconnect = setTimeout(connectEvents, delay);
    delay = Math.min(delay * 2, 30000);
  });
}

function start() {
  void selectView(state.view, state.step, { reload: true });
  connectEvents();
  fallback = setInterval(() => { if (eventsSocket?.readyState !== WebSocket.OPEN) refreshInBackground(false); }, 5000);
}

$("add-source").addEventListener("click", (event) => editSource(event.currentTarget));
$("source-search").addEventListener("input", paintSourcesNavigation);
$("sources-toggle").addEventListener("click", () => {
  const open = !app.hasAttribute("data-sources-open");
  showSourceList(open);
  if (open) $("source-search").focus();
});
$("navigation").addEventListener("keydown", (event) => {
  if (event.key !== "Escape" || !app.hasAttribute("data-sources-open")) return;
  showSourceList(false);
  $("sources-toggle").focus();
});
$("retry-view").addEventListener("click", () => void selectView(state.view, state.step, { reload: true, focus: true }));
for (const item of $("menu").children) {
  item.addEventListener("click", () => {
    const view = item.dataset.view;
    const step = view === "sources" ? (state.database ? "detail" : "list") : "page";
    void selectView(view, step);
  });
}
window.addEventListener("pagehide", () => {
  clearTimeout(reconnect);
  clearInterval(fallback);
  const socket = eventsSocket;
  eventsSocket = null;
  socket?.close();
});
window.addEventListener("pageshow", (event) => { if (event.persisted) start(); });

start();
