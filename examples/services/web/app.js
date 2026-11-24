const apiBase = new URL("../api/", window.location.href);

const refreshButton = document.querySelector("#refresh");
const detailButton = document.querySelector("#detail");
const restartButton = document.querySelector("#restart");
const unitInput = document.querySelector("#unit");
const listStatus = document.querySelector("#list-status");
const tableBody = document.querySelector("#services tbody");
const output = document.querySelector("#output");

function show(payload) {
  output.textContent = typeof payload === "string" ? payload : JSON.stringify(payload, null, 2);
}

async function call(path, options) {
  const response = await fetch(new URL(path, apiBase), options);
  const text = await response.text();
  let payload;
  try {
    payload = text === "" ? null : JSON.parse(text);
  } catch {
    payload = { raw: text };
  }
  if (!response.ok) {
    const message = payload && payload.error ? payload.error : response.statusText;
    throw new Error(message);
  }
  return payload;
}

function currentUnit() {
  const unit = unitInput.value.trim();
  if (unit === "") {
    throw new Error("请先输入单元名");
  }
  return unit;
}

function stateBadge(state) {
  const badge = document.createElement("span");
  badge.className = `badge badge-${state || "unknown"}`;
  badge.textContent = state || "—";
  return badge;
}

async function loadList() {
  refreshButton.disabled = true;
  listStatus.textContent = "加载中…";
  try {
    const items = await call("list");
    tableBody.replaceChildren();
    if (!Array.isArray(items) || items.length === 0) {
      listStatus.textContent = "没有已授权且可见的服务单元。";
      return;
    }
    for (const item of items) {
      const row = document.createElement("tr");
      const name = document.createElement("td");
      name.textContent = item.name;
      const description = document.createElement("td");
      description.textContent = item.description || "—";
      const state = document.createElement("td");
      state.append(stateBadge(item.state));
      const boot = document.createElement("td");
      boot.textContent = item.bootState || "—";
      const actions = document.createElement("td");
      actions.textContent = (item.actions || []).join(", ") || "—";
      row.append(name, description, state, boot, actions);
      row.addEventListener("click", () => {
        unitInput.value = item.name;
      });
      tableBody.append(row);
    }
    listStatus.textContent = `共 ${items.length} 个服务。`;
    show(items);
  } catch (error) {
    listStatus.textContent = `加载失败：${error.message}`;
    show(`加载失败：${error.message}`);
  } finally {
    refreshButton.disabled = false;
  }
}

async function showDetail() {
  detailButton.disabled = true;
  try {
    const unit = currentUnit();
    const detail = await call(`detail?unit=${encodeURIComponent(unit)}`);
    show(detail);
  } catch (error) {
    show(`查看详情失败：${error.message}`);
  } finally {
    detailButton.disabled = false;
  }
}

async function restartUnit() {
  restartButton.disabled = true;
  try {
    const unit = currentUnit();
    const result = await call(`restart?unit=${encodeURIComponent(unit)}`, { method: "POST" });
    show(result);
    await loadList();
  } catch (error) {
    show(`重启失败：${error.message}`);
  } finally {
    restartButton.disabled = false;
  }
}

refreshButton.addEventListener("click", loadList);
detailButton.addEventListener("click", showDetail);
restartButton.addEventListener("click", restartUnit);
unitInput.addEventListener("keydown", (event) => {
  if (event.key === "Enter") {
    showDetail();
  }
});

loadList();
