const apiURL = new URL("../api/hello", window.location.href);
const eventsURL = new URL("../events", window.location.href);
eventsURL.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";

const message = document.querySelector("#message");
const visits = document.querySelector("#visits");
const note = document.querySelector("#note");
const secret = document.querySelector("#secret");
const connection = document.querySelector("#connection");
const events = document.querySelector("#events");
const callButton = document.querySelector("#call");
const clearButton = document.querySelector("#clear");

let socket = null;
let retryTimer = null;
let seenEvent = false;

function setConnection(state, label) {
  connection.className = `badge badge-${state}`;
  connection.textContent = label;
}

function addEvent(payload) {
  if (!seenEvent) {
    events.replaceChildren();
    seenEvent = true;
  }
  const item = document.createElement("li");
  const time = document.createElement("time");
  time.textContent = new Date().toLocaleTimeString();
  const code = document.createElement("code");
  code.textContent = JSON.stringify(payload);
  item.append(time, code);
  events.prepend(item);
  while (events.children.length > 40) {
    events.lastElementChild.remove();
  }
}

function connect() {
  clearTimeout(retryTimer);
  setConnection("idle", "连接中");
  socket = new WebSocket(eventsURL);
  socket.addEventListener("open", () => setConnection("live", "事件流已连接"));
  socket.addEventListener("message", (event) => {
    try {
      addEvent(JSON.parse(event.data));
    } catch {
      addEvent({ raw: event.data });
    }
  });
  socket.addEventListener("close", () => {
    setConnection("error", "事件流已断开");
    retryTimer = setTimeout(connect, 2000);
  });
  socket.addEventListener("error", () => socket.close());
}

async function callLua() {
  callButton.disabled = true;
  try {
    const response = await fetch(apiURL);
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error || response.statusText);
    }
    message.textContent = payload.message;
    visits.textContent = payload.visits;
    note.textContent = payload.note;
    secret.textContent = payload.secretConfigured ? "已设置（值不显示）" : "未设置";
  } catch (error) {
    message.textContent = `调用失败：${error.message}`;
  } finally {
    callButton.disabled = false;
  }
}

callButton.addEventListener("click", callLua);
clearButton.addEventListener("click", () => {
  seenEvent = false;
  events.replaceChildren(Object.assign(document.createElement("li"), {
    className: "empty",
    textContent: "等待事件…",
  }));
});
window.addEventListener("pagehide", () => {
  clearTimeout(retryTimer);
  if (socket) socket.close();
});

connect();
callLua();
