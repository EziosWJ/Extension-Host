local app = host.app
local services = host.services

local function respond(status, payload)
  return {
    status = status,
    headers = { ["content-type"] = "application/json; charset=utf-8" },
    body = host.json.encode(payload),
  }
end

local function unitFrom(request)
  local unit = request.query.unit
  if unit == nil or unit == "" then
    return nil
  end
  return unit
end

app.route("GET", "/list", function()
  return respond(200, services.list())
end)

app.route("GET", "/detail", function(request)
  local unit = unitFrom(request)
  if unit == nil then
    return respond(400, { error = "unit is required" })
  end
  return respond(200, services.get(unit))
end)

app.route("POST", "/restart", function(request)
  local unit = unitFrom(request)
  if unit == nil then
    return respond(400, { error = "unit is required" })
  end
  services.restart(unit)
  return respond(200, { restarted = true, unit = unit })
end)

app.on("start", function()
  print("services plugin started")
end)
