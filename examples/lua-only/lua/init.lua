local app = host.app
local storage = host.storage

local function tick()
  local current = tonumber(storage.get("ticks") or "0") + 1
  storage.set("ticks", current)
  return current
end

app.route("GET", "/ping", function()
  local count = tick()
  return {
    status = 200,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode({ message = "pong", ticks = count }),
  }
end)

app.route("POST", "/emit", function(request)
  local payload = host.json.decode(request.body ~= "" and request.body or "{}")
  app.emit("message", payload)
  return {
    status = 202,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode({ emitted = true }),
  }
end)

app.on("start", function()
  print("lua-only plugin started")
end)
