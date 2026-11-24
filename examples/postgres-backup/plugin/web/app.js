const apiRoot = new URL("../api/", location.href);
const connection = document.getElementById("connection");
const form = document.getElementById("connection-form");
const formFields = form.querySelector("fieldset");
const connectionNotice = document.getElementById("connection-notice");
const clearPassword = form.elements.namedItem("clearPassword");
const password = form.elements.namedItem("password");
const connectionFields = ["host", "port", "database", "username"];
const button = document.getElementById("submit");
const target = document.getElementById("target");
const notice = document.getElementById("notice");
const list = document.getElementById("jobs");
const template = document.getElementById("job");
const labels = { running: "运行中", succeeded: "成功", failed: "失败" };
let jobs = [];
let ready = false;
let submitting = false;
let refreshing = false;
let submitError = "";
let generation = 0;
let initialHistoryLoaded = false;
const dateFormat = new Intl.DateTimeFormat("zh-CN", { dateStyle: "short", timeStyle: "medium" });

async function request(path, method = "GET", body) {
  const options = { method, cache: "no-store" };
  if (body) {
    options.headers = { "Content-Type": "application/json" };
    options.body = JSON.stringify(body);
  }
  const response = await fetch(new URL(path, apiRoot), options);
  const data = await response.json().catch(() => null);
  if (!response.ok || !data) {
    throw new Error(data?.error || data?.message || `请求失败（HTTP ${response.status}）。`);
  }
  return data;
}

function showNotice(message, error = false) {
  notice.textContent = message;
  notice.toggleAttribute("data-error", error);
}

function updateButton() {
  button.disabled = !ready || submitting || jobs.some((job) => job.status === "running");
}

function render() {
  const items = jobs.map((job) => {
    const row = template.content.firstElementChild.cloneNode(true);
    const time = row.querySelector("time");
    time.dateTime = job.startedAt;
    time.textContent = dateFormat.format(new Date(job.startedAt));
    const status = row.querySelector(".status");
    status.dataset.status = job.status;
    status.textContent = labels[job.status] || job.status;
    const detail = row.querySelector(".detail");
    detail.textContent = job.error || job.fileName || "";
    detail.classList.toggle("file", Boolean(job.fileName));
    return row;
  });
  list.replaceChildren(...items);
  updateButton();
}

async function refresh() {
  if (submitting || refreshing) return;
  refreshing = true;
  const startedAt = generation;
  try {
    const data = await request("backups");
    if (submitting || startedAt !== generation) return;
    target.textContent = data.target || "未配置备份目标";
    jobs = data.jobs;
    ready = data.configured;
    if (!initialHistoryLoaded && !ready) connection.open = true;
    initialHistoryLoaded = true;
    const empty = jobs.length ? "" : "还没有备份。点击「立即备份」开始。";
    const message = submitError || (ready ? empty : "请先保存数据库连接。");
    showNotice(message, Boolean(submitError));
    render();
  } catch (error) {
    if (submitting || startedAt !== generation) return;
    ready = false;
    target.textContent = "无法读取备份目标";
    showNotice(error.message, true);
    updateButton();
  } finally {
    refreshing = false;
  }
}

button.addEventListener("click", async () => {
  submitting = true;
  generation++;
  submitError = "";
  updateButton();
  showNotice("正在提交备份…");
  try {
    const job = await request("backups", "POST");
    jobs = [job, ...jobs.filter((item) => item.id !== job.id)];
    showNotice("");
    render();
  } catch (error) {
    submitError = error.message;
    showNotice(submitError, true);
  } finally {
    submitting = false;
    updateButton();
    void refresh();
  }
});

function displayConnection(data) {
  for (const name of connectionFields) form.elements.namedItem(name).value = data[name];
  password.value = "";
  password.disabled = false;
  password.placeholder = data.passwordSaved ? "留空保留已保存的密码" : "可留空";
  clearPassword.checked = false;
  document.getElementById("clear-password").hidden = !data.passwordSaved;
}

function showConnectionNotice(message, error = false) {
  connectionNotice.textContent = message;
  connectionNotice.toggleAttribute("data-error", error);
}

async function loadConnection() {
  try {
    displayConnection(await request("connection"));
    formFields.disabled = false;
    showConnectionNotice("");
  } catch (error) {
    showConnectionNotice(error.message, true);
  }
}

clearPassword.addEventListener("change", () => {
  password.disabled = clearPassword.checked;
});

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const fields = new FormData(form);
  const payload = {
    host: fields.get("host").trim(),
    port: Number(fields.get("port")),
    database: fields.get("database"),
    username: fields.get("username"),
    password: fields.get("password") || "",
    clearPassword: clearPassword.checked,
  };
  formFields.disabled = true;
  showConnectionNotice("正在保存…");
  try {
    displayConnection(await request("connection", "PUT", payload));
    generation++;
    submitError = "";
    showConnectionNotice("连接已保存。");
    void refresh();
  } catch (error) {
    showConnectionNotice(error.message, true);
  } finally {
    formFields.disabled = false;
  }
});

connection.addEventListener("toggle", () => {
  if (connection.open && formFields.disabled && connectionNotice.hasAttribute("data-error")) {
    void loadConnection();
  }
});

void loadConnection();
void refresh();
window.setInterval(refresh, 1500);
