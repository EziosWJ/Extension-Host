const apiRoot = new URL("../api/", location.href);
const eventsURL = new URL("../events", location.href);
eventsURL.protocol = location.protocol === "https:" ? "wss:" : "ws:";
const connection = document.querySelector("#connection");
const form = document.querySelector("#connection-form");
const fields = form.querySelector("fieldset");
const save = document.querySelector("#save-connection");
const retry = document.querySelector("#retry-connection");
const connectionNotice = document.querySelector("#connection-notice");
const scheduleDetails = document.querySelector("#schedule");
const scheduleForm = document.querySelector("#schedule-form");
const scheduleFields = scheduleForm.querySelector("fieldset");
const scheduleExpression = scheduleForm.elements.namedItem("expression");
const saveSchedule = document.querySelector("#save-schedule");
const retrySchedule = document.querySelector("#retry-schedule");
const scheduleNotice = document.querySelector("#schedule-notice");
const clearPassword = form.elements.namedItem("clearPassword");
const password = form.elements.namedItem("password");
const connectionFields = ["host", "port", "database", "username"];
const backup = document.querySelector("#submit");
const target = document.querySelector("#target");
const backupState = document.querySelector("#backup-state");
const historyState = document.querySelector("#history-state");
const refreshHistory = document.querySelector("#refresh-history");
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
let refreshQueued = false;
let initialHistoryLoaded = false;
let submitting = false;
let saving = false;
let connectionLoaded = false;
let scheduleLoaded = false;
let savingSchedule = false;
let submitError = "";
let eventsSocket = null;
let reconnectTimer;
let reconnectDelay = 1000;

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
  refreshHistory.setAttribute("aria-disabled", String(Boolean(refreshing) || saving || submitting));
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
  if (refreshing || saving || submitting) {
    refreshQueued = true;
    return refreshing;
  }
  refreshQueued = false;
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
      target.textContent = "无法确认备份目标，请刷新历史。";
      feedback(historyState, `无法刷新备份历史：${error.message}`, true);
    } finally {
      refreshing = null;
      if (refreshQueued && !saving && !submitting) {
        void refresh();
      } else {
        renderControls();
      }
    }
  })();
  renderControls();
  return refreshing;
}

function connectEvents() {
  const socket = new WebSocket(eventsURL);
  eventsSocket = socket;
  socket.addEventListener("open", () => {
    reconnectDelay = 1000;
    // The snapshot also picks up changes made while the event stream was disconnected.
    void refresh();
  });
  socket.addEventListener("message", ({ data }) => {
    if (JSON.parse(data).event === "backups-changed") void refresh();
  });
  socket.addEventListener("close", () => {
    if (eventsSocket !== socket) return;
    eventsSocket = null;
    reconnectTimer = setTimeout(connectEvents, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 30000);
  });
}

refreshHistory.addEventListener("click", () => {
  if (refreshHistory.getAttribute("aria-disabled") !== "true") void refresh();
});

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

async function loadSchedule() {
  retrySchedule.hidden = true;
  feedback(scheduleNotice, "正在读取备份计划…");
  try {
    const data = await request("schedule");
    scheduleExpression.value = data.expression || "";
    scheduleLoaded = true;
    scheduleFields.disabled = false;
    saveSchedule.setAttribute("aria-disabled", "false");
    if (data.error) {
      scheduleDetails.open = true;
      feedback(scheduleNotice, data.error, true);
    } else {
      feedback(scheduleNotice, data.expression ? "" : "未启用定时备份。");
    }
  } catch (error) {
    scheduleDetails.open = true;
    feedback(scheduleNotice, `无法读取备份计划：${error.message}`, true);
    retrySchedule.hidden = false;
  }
}

clearPassword.addEventListener("change", () => {
  password.disabled = clearPassword.checked;
});
retry.addEventListener("click", loadConnection);
retrySchedule.addEventListener("click", loadSchedule);

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

scheduleForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!scheduleLoaded || scheduleFields.disabled || savingSchedule) return;
  savingSchedule = true;
  scheduleFields.disabled = true;
  saveSchedule.setAttribute("aria-disabled", "true");
  saveSchedule.textContent = "正在保存…";
  feedback(scheduleNotice, "正在保存备份计划…");
  try {
    const data = await request("schedule", "PUT", { expression: scheduleExpression.value.trim() });
    scheduleExpression.value = data.expression || "";
    retrySchedule.hidden = true;
    feedback(scheduleNotice, "备份计划已保存。");
  } catch (error) {
    feedback(scheduleNotice, `保存备份计划失败：${error.message}`, true);
  } finally {
    savingSchedule = false;
    scheduleFields.disabled = false;
    saveSchedule.setAttribute("aria-disabled", "false");
    saveSchedule.textContent = "保存计划";
  }
});

void loadConnection();
void loadSchedule();
void refresh();
connectEvents();
window.addEventListener("pagehide", () => {
  clearTimeout(reconnectTimer);
  const socket = eventsSocket;
  eventsSocket = null;
  socket?.close();
});
window.addEventListener("pageshow", (event) => {
  if (event.persisted) connectEvents();
});
