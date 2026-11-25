host.app.route("GET", "/hello", function()
  return {
    status = 200,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode({ message = "你好，来自 Lua" }),
  }
end)
