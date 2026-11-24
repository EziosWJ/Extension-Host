const apiRoot = new URL("../api/", location.href);
const connection = document.querySelector("#connection");
const form = document.querySelector("#connection-form");
const fields = form.querySelector("fieldset");
const save = document.querySelector("#save-connection");
const retry = document.querySelector("#retry-connection");
const connectionNotice = document.querySelector("#connection-notice");
const clearPassword = form.elements.namedItem("clearPassword");
const password = form.elements.namedItem("password");
const connectionFields = ["host", "port", "database", "username"];
const backup = document.querySelector("#submit");
const target = document.querySelector("#target");
const backupState = document.querySelector("#backup-state");
const historyState = document.querySelector("#history-state");
const list = document.querySelector("#jobs");
const template = document.querySelector("#job");
const labels = { running: "运行中", succeeded: "成功", failed: "失败" };
const dateFormat = new Intl.DateTimeFormat("zh-CN", { dateStyle: "short", timeStyle: "medium" });

let jobs = [];
let jobsSnapshot = "";
let configured = null;
let configuredTarget = "";
let targetStale = true;
let refreshing = null;
let initialHistoryLoaded = false;
let submitting = false;
let saving = false;
let connectionLoaded = false;
let submitError = "";

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

function feedback(node, message, error = false) {
  if (node.textContent !== message) node.textContent = message;
  node.classList.toggle("error", error);
}

function renderControls() {
  const running = jobs.find((job) => job.status === "running");
  const blocked = !configured || targetStale || Boolean(refreshing) || submitting || saving || Boolean(running);
  backup.setAttribute("aria-disabled", String(blocked));
  save.setAttribute("aria-disabled", String(fields.disabled || submitting));
  backup.classList.toggle("primary", configured === true);
  const label = submitting ? "正在提交…" : running ? "备份运行中" : "立即备份";
  if (backup.textContent !== label) backup.textContent = label;

  if (submitting) {
    feedback(backupState, `正在提交备份，目标为 ${configuredTarget}。`);
  } else if (running) {
    feedback(backupState, `正在备份 ${running.target || "未知目标"}。${submitError ? ` ${submitError}` : ""}`, Boolean(submitError));
  } else if (submitError) {
    feedback(backupState, submitError, true);
  } else if (!targetStale && !configured) {
    feedback(backupState, "请先保存数据库连接。");
  } else {
    feedback(backupState, "");
  }
}

function renderHistory() {
  const snapshot = JSON.stringify(jobs);
  if (snapshot === jobsSnapshot) return;
  jobsSnapshot = snapshot;
  list.replaceChildren(...jobs.map((job) => {
    const row = template.content.firstElementChild.cloneNode(true);
    const time = row.querySelector("time");
    const date = new Date(job.startedAt);
    time.dateTime = job.startedAt || "";
    time.textContent = Number.isNaN(date.getTime()) ? "时间未知" : dateFormat.format(date);
    const status = row.querySelector(".badge");
    status.dataset.state = job.status;
    status.textContent = labels[job.status] || job.status;
    row.querySelector(".job-target").textContent = job.target || "未知目标";
    row.querySelector(".result-label").textContent = job.error ? "失败原因" : job.fileName ? "备份文件" : "结果";
    const result = row.querySelector(".result");
    result.textContent = job.error || job.fileName || (job.status === "running" ? "等待备份完成" : "—");
    result.classList.toggle("mono", Boolean(job.fileName) && !job.error);
    return row;
  }));
}

function refresh() {
  if (refreshing || saving || submitting) return refreshing;
  refreshing = (async () => {
    try {
      const data = await request("backups");
      if (saving) return;
      configured = data.configured;
      configuredTarget = data.target;
      targetStale = false;
      target.textContent = configured ? `备份目标：${configuredTarget}` : "未配置备份目标";
      jobs = data.jobs || [];
      if (!initialHistoryLoaded && !configured) connection.open = true;
      initialHistoryLoaded = true;
      feedback(historyState, jobs.length ? "" : "还没有备份记录。");
      renderHistory();
    } catch (error) {
      if (saving) return;
      targetStale = true;
      target.textContent = "无法确认备份目标，正在重试。";
      feedback(historyState, `无法刷新备份历史：${error.message}`, true);
    } finally {
      refreshing = null;
      renderControls();
    }
  })();
  renderControls();
  return refreshing;
}

backup.addEventListener("click", async () => {
  if (backup.getAttribute("aria-disabled") === "true") return;
  submitting = true;
  submitError = "";
  renderControls();
  try {
    const job = await request("backups", "POST");
    jobs = [job, ...jobs.filter((item) => item.id !== job.id)];
    feedback(historyState, "");
    renderHistory();
  } catch (error) {
    submitError = `提交备份失败：${error.message}`;
  } finally {
    submitting = false;
    void refresh();
  }
});

function displayConnection(data) {
  for (const name of connectionFields) form.elements.namedItem(name).value = data[name];
  password.value = "";
  password.disabled = false;
  password.placeholder = data.passwordSaved ? "留空保留已保存的密码" : "可留空";
  clearPassword.checked = false;
  clearPassword.closest("label").hidden = !data.passwordSaved;
}

async function loadConnection() {
  retry.hidden = true;
  feedback(connectionNotice, "正在读取连接…");
  try {
    displayConnection(await request("connection"));
    connectionLoaded = true;
    fields.disabled = false;
    save.setAttribute("aria-disabled", "false");
    feedback(connectionNotice, "");
  } catch (error) {
    feedback(connectionNotice, `无法读取连接：${error.message}`, true);
    retry.hidden = false;
  }
}

clearPassword.addEventListener("change", () => {
  password.disabled = clearPassword.checked;
});
retry.addEventListener("click", loadConnection);

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!connectionLoaded || fields.disabled || submitting) return;
  const data = new FormData(form);
  const payload = {
    host: data.get("host").trim(),
    port: Number(data.get("port")),
    database: data.get("database"),
    username: data.get("username"),
    password: data.get("password") || "",
    clearPassword: clearPassword.checked,
  };
  saving = true;
  targetStale = true;
  fields.disabled = true;
  save.setAttribute("aria-disabled", "true");
  save.textContent = "正在保存…";
  target.textContent = "正在保存连接…";
  submitError = "";
  feedback(connectionNotice, "正在保存连接…");
  renderControls();
  try {
    displayConnection(await request("connection", "PUT", payload));
    feedback(connectionNotice, "连接已保存。");
  } catch (error) {
    feedback(connectionNotice, `保存连接失败：${error.message}`, true);
  } finally {
    await refreshing;
    saving = false;
    target.textContent = "正在刷新备份目标…";
    const targetRefresh = refresh();
    await targetRefresh;
    fields.disabled = false;
    save.setAttribute("aria-disabled", "false");
    save.textContent = "保存连接";
  }
});

void loadConnection();
void refresh();
window.setInterval(refresh, 1500);
