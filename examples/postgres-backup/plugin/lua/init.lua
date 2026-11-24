if not host.process then
  error("PostgreSQL 备份需要支持 host.process 的 Extension Host。")
end

local json_headers = {
  ["content-type"] = "application/json; charset=utf-8",
  ["cache-control"] = "no-store",
}
local fields = "id, target, status, started_at AS startedAt, file_name AS fileName, error"

local function response(status, data)
  return { status = status, headers = json_headers, body = host.json.encode(data) }
end

local function saved_connection()
  local value = host.storage.get("connection")
  return value and host.json.decode(value) or nil
end

local function public_connection(connection)
  return {
    host = connection and connection.host or "127.0.0.1",
    port = connection and connection.port or 5432,
    database = connection and connection.database or "",
    username = connection and connection.username or "",
    passwordSaved = connection ~= nil and connection.password ~= "",
  }
end

local function valid_text(value)
  return type(value) == "string" and value:find("%S") and not value:find("%z")
end

host.app.route("GET", "/connection", function()
  return response(200, public_connection(saved_connection()))
end)

host.app.route("PUT", "/connection", function(request)
  local ok, input = pcall(host.json.decode, request.body)
  if not ok or type(input) ~= "table" then
    return response(400, { error = "无法读取连接设置。" })
  end
  if not valid_text(input.host) or not valid_text(input.database) or not valid_text(input.username) then
    return response(400, { error = "请填写主机、数据库和用户。" })
  end
  if type(input.port) ~= "number" or input.port % 1 ~= 0 or input.port < 1 or input.port > 65535 then
    return response(400, { error = "端口必须是 1 至 65535 之间的整数。" })
  end
  local password = input.password
  if password ~= nil and (type(password) ~= "string" or password:find("%z")) then
    return response(400, { error = "密码格式无效。" })
  end
  if input.clearPassword == true then
    password = ""
  elseif not password or password == "" then
    local previous = saved_connection()
    password = previous and previous.password or ""
  end
  local connection = {
    host = input.host,
    port = input.port,
    database = input.database,
    username = input.username,
    password = password,
  }
  host.storage.set("connection", host.json.encode(connection))
  return response(200, public_connection(connection))
end)

local function backup_target()
  local connection = saved_connection()
  if not connection then
    return os.getenv("PGSERVICE") or ""
  end
  return connection.database .. " · " .. connection.host .. ":" .. connection.port, {
    PGSERVICE = false,
    PGHOSTADDR = false,
    PGHOST = connection.host,
    PGPORT = tostring(connection.port),
    PGDATABASE = connection.database,
    PGUSER = connection.username,
    PGPASSWORD = connection.password ~= "" and connection.password or false,
  }
end

local function now()
  return os.date("!%Y-%m-%dT%H:%M:%SZ")
end

local function task(id)
  return host.storage.query("SELECT " .. fields .. " FROM backups WHERE id = ?", { id })[1]
end

local function finish(id, file_name, message)
  if message then
    host.storage.exec("UPDATE backups SET status = 'failed', error = ? WHERE id = ?", { message, id })
  else
    host.storage.exec("UPDATE backups SET status = 'succeeded', file_name = ? WHERE id = ?", { file_name, id })
  end
end

local function archive_paths(id)
  local name = id .. ".dump"
  local final = host.dataDir .. "/" .. name
  return name, final, final .. ".partial"
end

local function finish_dump(id, name, final, partial, result)
  local message = result.error
  if result.exitCode ~= 0 then
    local stderr = result.stderr:match("^%s*(.-)%s*$")
    message = stderr ~= "" and stderr or message or ("pg_dump 退出码 " .. result.exitCode)
  end
  if not message then
    local published, err = os.rename(partial, final)
    if not published then
      message = "无法保存备份文件：" .. err
    end
  end
  if message then
    os.remove(partial)
  end
  finish(id, name, message)
end

host.app.on("start", function()
  host.storage.exec([[
    CREATE TABLE IF NOT EXISTS backups (
      id TEXT PRIMARY KEY,
      target TEXT NOT NULL,
      status TEXT NOT NULL,
      started_at TEXT NOT NULL,
      file_name TEXT,
      error TEXT
    )
  ]])
  for _, job in ipairs(host.storage.query("SELECT id FROM backups WHERE status = 'running'")) do
    local name, final, partial = archive_paths(job.id)
    local archive = io.open(final, "rb")
    if archive then
      archive:close()
      finish(job.id, name)
    else
      os.remove(partial)
      finish(job.id, nil, "备份在插件停止时中断。")
    end
  end
end)

host.app.route("GET", "/backups", function()
  local target = backup_target()
  return response(200, {
    target = target,
    configured = target ~= "",
    jobs = host.storage.query("SELECT " .. fields .. " FROM backups ORDER BY rowid DESC"),
  })
end)

host.app.route("POST", "/backups", function()
  local running = host.storage.query("SELECT " .. fields .. " FROM backups WHERE status = 'running' LIMIT 1")[1]
  if running then
    return response(202, running)
  end

  local target, env = backup_target()
  if target == "" then
    return response(503, { error = "请先保存数据库连接。" })
  end

  local source = io.open("/proc/sys/kernel/random/uuid", "r")
  if not source then
    return response(500, { error = "无法生成任务 ID。" })
  end
  local id = source:read("*l")
  source:close()
  if not id then
    return response(500, { error = "无法生成任务 ID。" })
  end

  host.storage.exec(
    "INSERT INTO backups (id, target, status, started_at) VALUES (?, ?, 'running', ?)",
    { id, target, now() }
  )
  local name, final, partial = archive_paths(id)
  local pid, err = host.process.start("pg_dump", { "--format=custom", "--no-password", "--file", partial }, function(result)
    finish_dump(id, name, final, partial, result)
  end, env)
  if not pid then
    finish(id, nil, "无法启动 pg_dump：" .. err)
    return response(200, task(id))
  end
  return response(202, task(id))
end)
