local app = host.app
local storage = host.storage
local config = host.config
local util = require("util")

local function recordVisit()
  local current = tonumber(storage.get("visits") or "0")
  local nextCount = current + 1
  storage.set("visits", nextCount)
  return nextCount
end

app.route("GET", "/hello", function(request)
  local count = recordVisit()
  local greeting = config.get("greeting") or "你好"
  local payload = {
    message = greeting .. "，来自 Lua",
    greeting = greeting,
    visits = count,
    visitor = request.query.name or "",
    secretConfigured = config.get("token") ~= nil,
    note = util.describe(count),
  }
  app.emit("visit", { count = count, greeting = greeting })
  return {
    status = 200,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode(payload),
  }
end)

app.route("GET", "/stats", function()
  local rows = host.storage.query("SELECT key, value FROM kv ORDER BY key")
  return {
    status = 200,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode({ rows = rows }),
  }
end)

app.on("start", function()
  print("hello plugin started; visits=" .. tostring(storage.get("visits") or "0"))
end)

app.on("stop", function()
  print("hello plugin stopping")
end)
