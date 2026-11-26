if not host.process or not host.schedule then
  error("PostgreSQL 备份需要支持 host.process 和 host.schedule 的 Extension Host。")
end

local pg = require("pg")
local restore = require("restore")
local storage = host.storage
local pending = {}
local schedule_errors = {}
local json_headers = { ["Content-Type"] = "application/json; charset=utf-8", ["Cache-Control"] = "no-store" }

local function response(status, data)
  return { status = status, headers = json_headers, body = host.json.encode(data) }
end

local function problem(status, message)
  return response(status, { error = message })
end

local function route(method, path, handler)
  host.app.route(method, path, function(request)
    if method == "GET" then return handler(request) end
    local ok, input = pcall(host.json.decode, request.body)
    if not ok or type(input) ~= "table" then return problem(400, "请求内容无效。") end
    return handler(input)
  end)
end

local function now()
  return os.date("!%Y-%m-%dT%H:%M:%SZ")
end

local function changed()
  host.app.emit("backup-changed", {})
end

local function one(sql, args)
  return storage.query(sql, args)[1]
end

local function source_by_id(id)
  if not pg.text(id) then return nil end
  local row = one("SELECT id, definition FROM sources WHERE id = ?", { id })
  return row and host.json.decode(row.definition)
end

local function save_source(source)
  storage.exec("INSERT INTO sources (id, definition) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET definition = excluded.definition",
    { source.id, host.json.encode(source) })
end

local function public_source(source)
  return { id = source.id, name = source.name, host = source.host, port = source.port, username = source.username,
    database = source.database, service = source.service, passwordSaved = source.password ~= "" }
end

local function backup_directory()
  local path = host.config.get("备份目录")
  return path and path ~= "" and path or host.dataDir
end

local function join(path, file)
  return path:gsub("/+$", "") .. "/" .. file
end

local function integer(value, minimum, maximum)
  return type(value) == "number" and value >= minimum and value <= maximum and value == math.floor(value)
end

local function is_file(path)
  local file = io.open(path, "rb")
  if not file then return false end
  file:close()
  return true
end

local function async(work)
  local active = 0
  for id, result in pairs(pending) do
    if result.expires and result.expires < os.time() then
      pending[id] = nil
    elseif not result.expires then
      active = active + 1
    end
  end
  if active >= 8 then return problem(429, "正在处理较多请求，请稍后重试。") end
  local id = pg.id()
  pending[id] = { status = "running" }
  work(function(err, data)
    pending[id] = { status = err and "failed" or "succeeded", error = err, data = data, expires = os.time() + 120 }
  end)
  return response(202, { operation = id })
end

route("GET", "/operation", function(request)
  local id = request.query.id
  local result = id and pending[id]
  if not result or (result.expires and result.expires < os.time()) then return problem(404, "查询已过期，请重试。") end
  return response(200, { status = result.status, error = result.error, data = result.data })
end)

local database_query = [[
  SELECT datname AS name, pg_get_userbyid(datdba) AS owner
  FROM pg_database WHERE datallowconn AND NOT datistemplate
    AND has_database_privilege(current_user, oid, 'CONNECT') ORDER BY datname
]]

local function normalize_source(input, old)
  if not pg.text(input.name) or #input.name > 160 then return nil, "请填写数据源名称。" end
  local service = input.service or ""
  local password = input.password or ""
  if type(password) ~= "string" or password:find("%z") or type(service) ~= "string" or service:find("%z") then
    return nil, "服务名或密码格式无效。"
  end
  if service == "" and (not pg.text(input.host) or not pg.text(input.username) or not integer(input.port, 1, 65535)) then
    return nil, "请填写主机、有效端口和用户名。"
  end
  local database = input.database or ""
  if not pg.database(database) and (service == "" or database ~= "") then return nil, "请填写有效的入口数据库。" end
  if input.clearPassword == true then
    password = ""
  elseif password == "" and old then
    password = old.password
  end
  return { id = old and old.id or pg.id(), name = input.name, host = service == "" and input.host or "",
    port = service == "" and input.port or 5432, username = service == "" and input.username or "",
    database = database, service = service, password = password }
end

route("GET", "/sources", function()
  local sources = host.json.decode("[]")
  for i, row in ipairs(storage.query("SELECT definition FROM sources ORDER BY rowid")) do
    sources[i] = public_source(host.json.decode(row.definition))
  end
  return response(200, { sources = sources })
end)

local function source_busy(id, include_pending)
  local pending_status = include_pending and " OR status IN ('ready','needs_review')" or ""
  return one("SELECT id FROM backups WHERE source_id = ? AND status = 'running' UNION ALL SELECT id FROM restores " ..
    "WHERE source_id = ? AND (status IN " .. restore.active .. pending_status .. ") LIMIT 1", { id, id })
end

local function source_tasks(id)
  return storage.query("SELECT name, database_name AS database FROM schedules WHERE source_id = ? ORDER BY rowid", { id })
end

route("PUT", "/sources", function(input)
  local old = source_by_id(input.id)
  if input.id and not old then return problem(404, "找不到该数据源。") end
  local source, err = normalize_source(input, old)
  if not source then return problem(400, err) end
  local endpoint_change = old and (old.host ~= source.host or old.port ~= source.port or old.service ~= source.service)
  local connection_change = endpoint_change or (old and (old.database ~= source.database or old.username ~= source.username or old.password ~= source.password))
  if old and not connection_change then
    save_source(source)
    changed()
    return response(200, public_source(source))
  end
  if old and source_busy(old.id, endpoint_change) then return problem(409, "请先处理数据源正在执行或待确认的操作。") end
  if endpoint_change and #source_tasks(old.id) > 0 and input.confirmEndpoint ~= true then
    return problem(409, "该数据源有定时任务。请查看受影响的任务并确认连接变更。")
  end
  return async(function(done)
    pg.query(source, source.database ~= "" and source.database or nil, database_query, function(probe_error)
      if probe_error then return done(probe_error) end
      if old then
        local current = source_by_id(old.id)
        for key, value in pairs(old) do
          if not current or current[key] ~= value then return done("数据源已更新，请重新打开编辑窗口。") end
        end
        if source_busy(old.id, endpoint_change) then return done("请先处理数据源正在执行或待确认的操作。") end
        if endpoint_change and #source_tasks(old.id) > 0 and input.confirmEndpoint ~= true then
          return done("数据源已有定时任务，请确认变更。")
        end
      end
      save_source(source)
      changed()
      done(nil, public_source(source))
    end)
  end)
end)

route("DELETE", "/sources", function(input)
  local source = source_by_id(input.id)
  if not source then return problem(404, "找不到该数据源。") end
  if #source_tasks(source.id) > 0 then return problem(409, "请先删除或改绑该数据源的定时任务。") end
  if source_busy(source.id, true) then return problem(409, "请先处理数据源正在执行或待确认的操作。") end
  storage.exec("DELETE FROM sources WHERE id = ?", { source.id })
  changed()
  return response(200, {})
end)

route("POST", "/probe", function(input)
  local source = source_by_id(input.source)
  if not source then return problem(404, "找不到该数据源。") end
  if input.database ~= nil and not pg.database(input.database) then return problem(400, "数据库名称无效。") end
  local db = input.database or (source.database ~= "" and source.database or nil)
  local sql = input.database and [[
    SELECT nspname AS name, pg_get_userbyid(nspowner) AS owner FROM pg_namespace
    WHERE nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
      AND nspname !~ '^pg_(toast_)?temp_' ORDER BY nspname
  ]] or database_query
  return async(function(done)
    pg.query(source, db, sql, function(err, rows) done(err, rows and { rows = rows }) end)
  end)
end)

local function busy(source, database)
  local running = one("SELECT count(*) AS total FROM (SELECT source_id, database_name FROM backups WHERE status = 'running' " ..
    "UNION ALL SELECT source_id, database_name FROM restores WHERE status IN " .. restore.active .. ")")
  if one("SELECT id FROM backups WHERE source_id = ? AND database_name = ? AND status = 'running' UNION ALL " ..
      "SELECT id FROM restores WHERE source_id = ? AND database_name = ? AND status IN " .. restore.active .. " LIMIT 1",
      { source, database, source, database }) then
    return "该数据库已有任务在运行。"
  end
  if running.total >= (host.config.get("并行任务上限") or 2) then return "已达到并行任务上限。" end
end

local function finish_backup(id, status, err)
  storage.exec("UPDATE backups SET status = ?, error = ?, finished_at = ? WHERE id = ?", { status, err or "", now(), id })
  changed()
end

local function backup(source, database, schedule, trigger)
  local blocked = busy(source.id, database)
  if blocked and trigger ~= "scheduled" then return nil, blocked end
  local id = pg.id()
  local name = id .. ".dump"
  local final = join(backup_directory(), name)
  local partial = final .. ".partial"
  local status = blocked and "skipped" or "running"
  local target = source.name .. " / " .. (database ~= "" and database or "服务默认数据库")
  storage.exec([[INSERT INTO backups
    (id, target, status, started_at, source_id, database_name, schedule_id, trigger, file_name, archive_path)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)]],
    { id, target, status, now(), source.id, database, schedule or "", trigger, name, final })
  if blocked then
    finish_backup(id, "skipped", blocked)
  else
    local function finish(err)
      if not err then
        local renamed, rename_error = os.rename(partial, final)
        if not renamed then err = rename_error end
      end
      if err then os.remove(partial) end
      finish_backup(id, err and "failed" or "succeeded", err)
    end
    pg.run("install", { "-m", "600", "--", "/dev/null", partial }, nil, function(err)
      if err then return finish(err) end
      local args = { "--format=custom", "--no-password", "--file", partial }
      if database ~= "" then table.insert(args, "--dbname=" .. pg.dsn(database)) end
      pg.run("pg_dump", args, pg.environment(source, database ~= "" and database or nil), finish)
    end)
    changed()
  end
  return { id = id }
end

route("POST", "/backups", function(input)
  local source = source_by_id(input.source)
  if not source or not pg.database(input.database) then return problem(400, "请选择数据源和数据库。") end
  local job, err = backup(source, input.database, nil, "manual")
  return job and response(202, job) or problem(409, err)
end)

route("GET", "/backups", function()
  return response(200, { jobs = storage.query([[
    SELECT id, target, status, started_at, finished_at, error, source_id, database_name, schedule_id,
      trigger, CASE WHEN status = 'succeeded' THEN archive_path ELSE '' END AS archive_path
    FROM backups ORDER BY rowid DESC
  ]]) })
end)

route("DELETE", "/backups", function(input)
  if not pg.text(input.id) then return problem(400, "请选择备份。") end
  local job = one("SELECT * FROM backups WHERE id = ?", { input.id })
  if not job or job.status ~= "succeeded" then return problem(404, "找不到可删除的备份文件。") end
  if one("SELECT id FROM restores WHERE backup_id = ? AND status IN " .. restore.active, { job.id }) then
    return problem(409, "该归档正在用于恢复。")
  end
  local removed, err, code = os.remove(job.archive_path)
  if not removed and code ~= 2 then
    -- GopherLua may omit errno from os.remove; use the filesystem error reported by the host.
    if not err or not err:find("no such file or directory", 1, true) then return problem(422, "无法删除备份文件：" .. tostring(err)) end
  end
  storage.exec("UPDATE backups SET status = 'deleted' WHERE id = ?", { job.id })
  changed()
  return response(200, {})
end)

local function register(schedule)
  local name = "backup-" .. schedule.id
  if schedule.enabled == 0 then
    host.schedule.unregister(name)
    return true
  end
  return host.schedule.register(name, schedule.expression, function()
    local current = one("SELECT * FROM schedules WHERE id = ? AND enabled = 1", { schedule.id })
    if not current then return end
    local source = source_by_id(current.source_id)
    if source then backup(source, current.database_name, current.id, "scheduled") end
  end)
end

route("GET", "/schedules", function()
  local rows = storage.query([[SELECT s.*,
      (SELECT CASE WHEN status = 'deleted' THEN 'succeeded' ELSE status END FROM backups WHERE schedule_id = s.id ORDER BY rowid DESC LIMIT 1) AS last_status,
      (SELECT started_at FROM backups WHERE schedule_id = s.id ORDER BY rowid DESC LIMIT 1) AS last_at
    FROM schedules s ORDER BY s.rowid]])
  for _, row in ipairs(rows) do
    row.error = schedule_errors[row.id]
    if row.enabled == 1 and host.schedule.next then row.next_at = host.schedule.next("backup-" .. row.id) end
  end
  return response(200, { schedules = rows })
end)

route("PUT", "/schedules", function(input)
  if not pg.text(input.name) or #input.name > 160 or not source_by_id(input.source) or
      not pg.database(input.database) or not pg.text(input.expression) or #input.expression > 256 then
    return problem(400, "请填写任务名称、数据源、数据库和有效的计划表达式。")
  end
  if input.id and (not pg.text(input.id) or not one("SELECT id FROM schedules WHERE id = ?", { input.id })) then
    return problem(404, "找不到定时任务。")
  end
  local schedule = { id = input.id or pg.id(), expression = input.expression, enabled = input.enabled == false and 0 or 1 }
  local valid, err
  if host.schedule.validate then
    valid, err = host.schedule.validate(schedule.expression)
  else
    local validation = "validate-" .. pg.id()
    valid, err = host.schedule.register(validation, schedule.expression, function() end)
    if valid then host.schedule.unregister(validation) end
  end
  if not valid then return problem(400, err) end
  local ok, register_error = register(schedule)
  if not ok then return problem(400, register_error) end
  storage.exec([[INSERT INTO schedules (id, name, source_id, database_name, expression, enabled)
      VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, source_id = excluded.source_id,
      database_name = excluded.database_name, expression = excluded.expression, enabled = excluded.enabled]],
    { schedule.id, input.name, input.source, input.database, schedule.expression, schedule.enabled })
  schedule_errors[schedule.id] = nil
  changed()
  return response(200, { id = schedule.id })
end)

route("DELETE", "/schedules", function(input)
  if not pg.text(input.id) then return problem(400, "请选择定时任务。") end
  storage.exec("DELETE FROM schedules WHERE id = ?", { input.id })
  host.schedule.unregister("backup-" .. input.id)
  schedule_errors[input.id] = nil
  changed()
  return response(200, {})
end)

route("POST", "/run-schedule", function(input)
  if not pg.text(input.id) then return problem(400, "请选择定时任务。") end
  local schedule = one("SELECT * FROM schedules WHERE id = ?", { input.id })
  local source = schedule and source_by_id(schedule.source_id)
  if not source then return problem(404, "找不到定时任务或数据源。") end
  local job, err = backup(source, schedule.database_name, schedule.id, "schedule_manual")
  return job and response(202, job) or problem(409, err)
end)

local function endpoint(source)
  return host.json.encode({ source.host, source.port, source.service })
end

route("GET", "/restores", function()
  return response(200, { jobs = storage.query("SELECT * FROM restores ORDER BY rowid DESC") })
end)

route("POST", "/restores", function(input)
  local source = source_by_id(input.source)
  local archive = pg.text(input.backup) and one("SELECT * FROM backups WHERE id = ? AND status = 'succeeded'", { input.backup })
  if not source or not pg.database(input.database) or (input.mode ~= "new" and input.mode ~= "replace") then
    return problem(400, "请选择目标数据源、数据库和恢复方式。")
  end
  if not archive or not is_file(archive.archive_path) then return problem(404, "备份文件不存在或无法读取。") end
  local control = input.control
  if control == nil or control == "" then control = source.database ~= "" and source.database or "postgres" end
  if not pg.database(control) or control == input.database then
    return problem(400, "请指定一个可以连接且与目标库不同的管理数据库。")
  end
  local blocked = busy(source.id, input.database)
  if blocked then return problem(409, blocked) end
  if one("SELECT id FROM restores WHERE source_id = ? AND database_name = ? AND status IN ('ready','needs_review')", { source.id, input.database }) then
    return problem(409, "请先处理该数据库待确认的恢复记录。")
  end
  local id = pg.id()
  local suffix = id:gsub("-", "")
  storage.exec([[INSERT INTO restores (id, backup_id, source_id, source_name, endpoint, database_name, control,
      mode, portable, status, started_at, stage, previous, displaced, backup_at)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'preflight', ?, ?, ?, ?, ?)]],
    { id, archive.id, source.id, source.name, endpoint(source), input.database, control, input.mode,
      input.portable == true and 1 or 0, now(), "eh_stage_" .. suffix, "eh_previous_" .. suffix,
      "eh_displaced_" .. suffix, archive.started_at })
  local job = one("SELECT *, database_name AS database FROM restores WHERE id = ?", { id })
  changed()
  restore.start(source, job, archive.archive_path)
  return response(202, { id = id })
end)

route("POST", "/restore-action", function(input)
  if not pg.text(input.id) then return problem(400, "请选择恢复记录。") end
  local job = one("SELECT *, database_name AS database FROM restores WHERE id = ?", { input.id })
  local source = job and source_by_id(job.source_id)
  if not source or endpoint(source) ~= job.endpoint then return problem(409, "恢复记录对应的数据源已删除或连接地址已更改。") end
  local action
  if input.action == "switch" and job.status == "ready" then action = restore.switch
  elseif input.action == "rollback" and job.status == "succeeded" and job.mode == "replace" and job.cleaned_previous == 0 then action = restore.rollback
  elseif input.action == "check" and job.status == "needs_review" then action = restore.check
  elseif input.action == "clean" and ((job.status == "succeeded" and job.mode == "replace" and job.cleaned_previous == 0) or
      (job.status == "rolled_back" and job.cleaned_displaced == 0) or
      ((job.status == "failed" or job.status == "ready") and job.stage_oid ~= "" and job.cleaned_stage == 0)) then
    action = restore.clean
  end
  if not action then return problem(409, "当前恢复状态不支持该操作。") end
  if input.action ~= "check" and input.confirm ~= job.database then return problem(400, "请输入目标数据库名称以确认操作。") end
  local blocked = busy(source.id, job.database)
  if blocked then return problem(409, blocked) end
  action(source, job)
  return response(202, { id = job.id })
end)

local function migrate_backups()
  local columns = {}
  for _, column in ipairs(storage.query("PRAGMA table_info(backups)")) do
    columns[column.name] = true
  end
  for _, name in ipairs({ "source_id", "database_name", "schedule_id", "trigger", "archive_path", "finished_at" }) do
    if not columns[name] then
      storage.exec("ALTER TABLE backups ADD COLUMN " .. name .. " TEXT NOT NULL DEFAULT ''")
    end
  end

  if storage.get("legacy-migrated") then return end

  local connection = storage.get("connection")
  local expression = storage.get("schedule")
  local source
  if connection then
    local old = host.json.decode(connection)
    old.name = "原有连接"
    source = normalize_source(old)
  elseif expression or not columns.archive_path then
    local service = os.getenv("PGSERVICE")
    if pg.text(service) then source = normalize_source({ name = service, service = service }) end
  end
  if source then
    source.id = "legacy-connection"
    save_source(source)
    if expression and expression ~= "" then
      storage.exec([[INSERT OR IGNORE INTO schedules (id, name, source_id, database_name, expression, enabled)
        VALUES ('legacy-schedule', ?, ?, ?, ?, 1)]], { "定时备份", source.id, source.database, expression })
    end
  end
  for _, job in ipairs(storage.query("SELECT id, file_name FROM backups WHERE trigger = ''")) do
    local filename = job.file_name or (job.id .. ".dump")
    storage.exec("UPDATE backups SET archive_path = ?, trigger = 'legacy' WHERE id = ?",
      { join(host.dataDir, filename), job.id })
  end
  storage.set("legacy-migrated", true)
end

host.app.on("start", function()
  storage.exec("CREATE TABLE IF NOT EXISTS sources (id TEXT PRIMARY KEY, definition TEXT NOT NULL)")
  storage.exec([[CREATE TABLE IF NOT EXISTS schedules (id TEXT PRIMARY KEY, name TEXT NOT NULL, source_id TEXT NOT NULL,
    database_name TEXT NOT NULL, expression TEXT NOT NULL, enabled INTEGER NOT NULL)]])
  storage.exec([[CREATE TABLE IF NOT EXISTS restores (
    id TEXT PRIMARY KEY, backup_id TEXT NOT NULL, source_id TEXT NOT NULL, source_name TEXT NOT NULL, endpoint TEXT NOT NULL,
    database_name TEXT NOT NULL, control TEXT NOT NULL, mode TEXT NOT NULL, portable INTEGER NOT NULL,
    status TEXT NOT NULL, started_at TEXT NOT NULL, backup_at TEXT NOT NULL, stage TEXT NOT NULL, previous TEXT NOT NULL,
    displaced TEXT NOT NULL, original_oid TEXT NOT NULL DEFAULT '', stage_oid TEXT NOT NULL DEFAULT '',
    restored INTEGER NOT NULL DEFAULT 0, cleaned_previous INTEGER NOT NULL DEFAULT 0, cleaned_displaced INTEGER NOT NULL DEFAULT 0,
    cleaned_stage INTEGER NOT NULL DEFAULT 0, cleanup_kind TEXT NOT NULL DEFAULT '', observed_target TEXT NOT NULL DEFAULT '', observed_stage TEXT NOT NULL DEFAULT '',
    observed_previous TEXT NOT NULL DEFAULT '', observed_displaced TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT ''
  )]])
  storage.exec([[CREATE TABLE IF NOT EXISTS backups (
    id TEXT PRIMARY KEY, target TEXT NOT NULL, status TEXT NOT NULL, started_at TEXT NOT NULL, file_name TEXT, error TEXT,
    source_id TEXT NOT NULL DEFAULT '', database_name TEXT NOT NULL DEFAULT '', schedule_id TEXT NOT NULL DEFAULT '',
    trigger TEXT NOT NULL DEFAULT '', archive_path TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT ''
  )]])
  migrate_backups()
  for _, job in ipairs(storage.query("SELECT id, archive_path FROM backups WHERE status = 'running'")) do
    if is_file(job.archive_path) then
      finish_backup(job.id, "succeeded")
    else
      os.remove(job.archive_path .. ".partial")
      finish_backup(job.id, "failed", "备份在插件停止时中断。")
    end
  end
  storage.exec("UPDATE restores SET status = 'needs_review', error = '操作在插件停止时中断，正在重新核对。' WHERE status IN " .. restore.active)
  for _, job in ipairs(storage.query("SELECT *, database_name AS database FROM restores WHERE status = 'needs_review'")) do
    local source = source_by_id(job.source_id)
    if source and endpoint(source) == job.endpoint then restore.check(source, job) end
  end
  for _, schedule in ipairs(storage.query("SELECT * FROM schedules WHERE enabled = 1")) do
    local ok, err = register(schedule)
    if not ok then schedule_errors[schedule.id] = err end
  end
end)
