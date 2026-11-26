local pg = {}

function pg.text(value)
  return type(value) == "string" and value ~= "" and not value:find("%z")
end

function pg.database(value)
  return pg.text(value) and #value <= 63
end

function pg.id()
  local file = assert(io.open("/proc/sys/kernel/random/uuid", "r"))
  local value = file:read("*l")
  file:close()
  return assert(value)
end

function pg.ident(value)
  return '"' .. value:gsub('"', '""') .. '"'
end

function pg.literal(value)
  return "E'" .. value:gsub("\\", "\\\\"):gsub("'", "''") .. "'"
end

function pg.dsn(database)
  return "dbname='" .. database:gsub("\\", "\\\\"):gsub("'", "\\'") .. "'"
end

function pg.environment(source, database)
  local env = { PGCONNECT_TIMEOUT = "8", PGAPPNAME = "Extension Host PostgreSQL Backup", PGOPTIONS = false }
  if source.service ~= "" then
    env.PGSERVICE = source.service
    env.PGHOST, env.PGHOSTADDR, env.PGPORT, env.PGUSER = false, false, false, false
    env.PGPASSWORD = source.password ~= "" and source.password or false
  else
    env.PGSERVICE = false
    env.PGHOST = source.host
    env.PGHOSTADDR = false
    env.PGPORT = tostring(source.port)
    env.PGUSER = source.username
    env.PGPASSWORD = source.password ~= "" and source.password or false
  end
  env.PGDATABASE = database or false
  return env
end

function pg.run(program, args, env, callback)
  local pid, err = host.process.start(program, args, function(result)
    local message = result.error
    if result.exitCode ~= 0 then
      local stderr = result.stderr:match("^%s*(.-)%s*$")
      message = stderr ~= "" and stderr or message or (program .. " 退出码 " .. result.exitCode)
    end
    callback(message)
  end, env)
  if not pid then callback("无法启动 " .. program .. "：" .. err) end
end

function pg.sql(source, database, sql, callback)
  local output = host.dataDir .. "/pg-query-" .. pg.id() .. ".tmp"
  local args = {
    "-X", "-w", "-qAt", "-v", "ON_ERROR_STOP=1", "--output", output, "--command", sql,
  }
  if database then table.insert(args, "--dbname=" .. pg.dsn(database)) end
  pg.run("psql", args, pg.environment(source, database), function(err)
    local data
    if not err then
      local file
      file, err = io.open(output, "rb")
      if file then
        data = file:read("*a")
        file:close()
      end
    end
    os.remove(output)
    callback(err, data)
  end)
end

function pg.query(source, database, sql, callback)
  local statement = "SELECT coalesce(json_agg(row_to_json(result)), '[]'::json) FROM (" .. sql .. ") AS result"
  pg.sql(source, database, statement, function(err, data)
    if err then return callback(err) end
    local ok, rows = pcall(host.json.decode, data)
    if not ok or type(rows) ~= "table" then return callback("无法读取 PostgreSQL 查询结果。") end
    callback(nil, rows)
  end)
end

return pg
