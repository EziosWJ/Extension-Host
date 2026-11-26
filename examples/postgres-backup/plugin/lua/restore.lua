local pg = require("pg")
local storage = host.storage
local restore = {}

local active = "('preflight','creating','restoring','switching','rolling_back','checking','cleaning')"
restore.active = active

local function update(id, status, err)
  storage.exec("UPDATE restores SET status = ?, error = coalesce(?, '') WHERE id = ?", { status, err or "", id })
  host.app.emit("backup-changed", {})
end

local function inventory(source, job, callback)
  local names = { pg.literal(job.database), pg.literal(job.stage), pg.literal(job.previous), pg.literal(job.displaced) }
  pg.query(source, job.control, "SELECT datname AS name, oid::text AS oid, datistemplate AS template, " ..
    "(pg_has_role(datdba, 'USAGE') OR (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)) AS owned " ..
    "FROM pg_database WHERE datname IN (" .. table.concat(names, ",") .. ")", function(err, rows)
    if err then return callback(err) end
    local found = {}
    for _, row in ipairs(rows) do found[row.name] = row end
    local function oid(name) return found[name] and found[name].oid or "" end
    storage.exec("UPDATE restores SET observed_target = ?, observed_stage = ?, observed_previous = ?, observed_displaced = ? WHERE id = ?",
      { oid(job.database), oid(job.stage), oid(job.previous), oid(job.displaced), job.id })
    callback(nil, found)
  end)
end

local function matches(found, name, oid)
  return oid ~= "" and found[name] and found[name].oid == oid
end

local function target_matches(found, job)
  if job.original_oid == "" then return found[job.database] == nil end
  return matches(found, job.database, job.original_oid)
end

local function guard(name, oid)
  local condition = oid == "" and "IS NOT NULL" or "IS DISTINCT FROM " .. pg.literal(oid)
  return "DO $eh$ BEGIN IF (SELECT oid::text FROM pg_database WHERE datname = " .. pg.literal(name):gsub("%$", "\\044") ..
    ") " .. condition .. " THEN RAISE EXCEPTION 'Database identity changed'; END IF; END $eh$;"
end

local function rename(from, to)
  return "ALTER DATABASE " .. pg.ident(from) .. " RENAME TO " .. pg.ident(to) .. ";"
end

local function cutover(source, job, rollback)
  local id = job.id
  local running = rollback and "rolling_back" or "switching"
  update(id, running)
  inventory(source, job, function(err, found)
    if err then return update(id, "needs_review", err) end
    local current, retained, incoming, current_oid, incoming_oid
    current = job.database
    if rollback then
      retained, incoming = job.displaced, job.previous
      current_oid, incoming_oid = job.stage_oid, job.original_oid
    else
      retained, incoming = job.previous, job.stage
      current_oid, incoming_oid = job.original_oid, job.stage_oid
    end
    if (current_oid == "" and found[current] or current_oid ~= "" and not matches(found, current, current_oid)) or
        not matches(found, incoming, incoming_oid) or found[retained] then
      return update(id, "needs_review", "数据库状态发生变化，无法安全切换。")
    end
    local sql = "BEGIN; SET LOCAL lock_timeout = '5s'; " .. guard(current, current_oid) .. guard(incoming, incoming_oid) ..
      guard(retained, "") .. "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND datid::text IN (" ..
      pg.literal(current_oid) .. ", " .. pg.literal(incoming_oid) .. ");"
    -- Both renames commit together. A reconnect during cutover can cause PostgreSQL to reject the transaction.
    if current_oid ~= "" then sql = sql .. rename(current, retained) end
    sql = sql .. rename(incoming, current) .. "COMMIT;"
    pg.sql(source, job.control, sql, function(switch_error)
      inventory(source, job, function(check_error, state)
        if check_error then return update(id, "needs_review", switch_error or check_error) end
        if matches(state, current, incoming_oid) and (current_oid == "" or matches(state, retained, current_oid)) then
          return update(id, rollback and "rolled_back" or "succeeded")
        end
        update(id, "needs_review", switch_error or "切换后的状态与预期不符。")
      end)
    end)
  end)
end

function restore.start(source, job, archive)
  local id = job.id
  inventory(source, job, function(err, found)
    if err then return update(id, "failed", err) end
    local target = found[job.database]
    if (job.mode == "replace") ~= (target ~= nil) then
      return update(id, "failed", target and "目标数据库已存在。" or "要替换的目标数据库不存在。")
    end
    if target and target.template then return update(id, "failed", "不能替换 PostgreSQL 模板数据库。") end
    if target and not target.owned then return update(id, "failed", "目标连接用户缺少目标数据库的所有者权限。") end
    if found[job.stage] or found[job.previous] or found[job.displaced] then
      return update(id, "failed", "恢复用的数据库名称已存在。")
    end
    storage.exec("UPDATE restores SET original_oid = ? WHERE id = ?", { target and target.oid or "", id })
    job.original_oid = target and target.oid or ""
    update(id, "creating")
    pg.sql(source, job.control, "CREATE DATABASE " .. pg.ident(job.stage) .. " TEMPLATE template0", function(create_err)
      if create_err then return update(id, "needs_review", create_err) end
      inventory(source, job, function(check_err, state)
        if check_err or not state[job.stage] then
          return update(id, "needs_review", check_err or "无法确认临时数据库。")
        end
        job.stage_oid = state[job.stage].oid
        storage.exec("UPDATE restores SET stage_oid = ? WHERE id = ?", { job.stage_oid, id })
        update(id, "restoring")
        local args = { "--no-password", "--exit-on-error", "--single-transaction", "--dbname",
          pg.dsn(job.stage) }
        if job.portable == 1 then
          table.insert(args, "--no-owner")
          table.insert(args, "--no-acl")
        end
        table.insert(args, archive)
        pg.run("pg_restore", args, pg.environment(source, job.stage), function(restore_err)
          if restore_err then return update(id, "failed", restore_err) end
          storage.exec("UPDATE restores SET restored = 1 WHERE id = ?", { id })
          if job.mode == "new" then return cutover(source, job, false) end
          update(id, "ready")
        end)
      end)
    end)
  end)
end

local cleanup_columns = {
  previous = { name = "previous", oid = "original_oid", flag = "cleaned_previous", status = "succeeded" },
  displaced = { name = "displaced", oid = "stage_oid", flag = "cleaned_displaced", status = "rolled_back" },
  stage = { name = "stage", oid = "stage_oid", flag = "cleaned_stage" },
}

local function cleanup_status(job, kind, removed)
  return kind.status or (job.restored == 1 and (removed and "cancelled" or "ready") or "failed")
end

local function finish_cleanup(job, kind, removed, err)
  storage.exec("UPDATE restores SET " .. kind.flag .. " = ?, cleanup_kind = '', status = ?, error = ? WHERE id = ?",
    { removed and 1 or 0, cleanup_status(job, kind, removed), err or "", job.id })
  host.app.emit("backup-changed", {})
end

function restore.check(source, job)
  update(job.id, "checking")
  inventory(source, job, function(err, found)
    if err then return update(job.id, "needs_review", err) end
    local cleaning = cleanup_columns[job.cleanup_kind]
    if cleaning then
      local name, oid = job[cleaning.name], job[cleaning.oid]
      if not found[name] then return finish_cleanup(job, cleaning, true) end
      if matches(found, name, oid) then return finish_cleanup(job, cleaning, false, "上次清理中断，可以重试。") end
      return update(job.id, "needs_review", "待清理数据库的身份发生变化。")
    end
    if matches(found, job.database, job.stage_oid) and
        (job.original_oid == "" or matches(found, job.previous, job.original_oid) or job.cleaned_previous == 1) then
      return update(job.id, "succeeded")
    end
    if matches(found, job.database, job.original_oid) and
        (matches(found, job.displaced, job.stage_oid) or job.cleaned_displaced == 1) then
      return update(job.id, "rolled_back")
    end
    if job.restored == 1 and target_matches(found, job) and matches(found, job.stage, job.stage_oid) then
      return update(job.id, "ready")
    end
    local stage_missing = not found[job.stage] and not found[job.previous] and not found[job.displaced]
    if job.original_oid == "" and job.stage_oid == "" and stage_missing then
      return update(job.id, "failed", "恢复在创建临时库前中断。")
    end
    if not target_matches(found, job) or found[job.previous] or found[job.displaced] or
        (found[job.stage] and not matches(found, job.stage, job.stage_oid)) then
      return update(job.id, "needs_review", "数据库状态与恢复记录不符，请由数据库管理员核对。")
    end
    update(job.id, "failed", "恢复没有完成，已核对数据库状态。")
  end)
end

function restore.switch(source, job)
  cutover(source, job, false)
end

function restore.rollback(source, job)
  cutover(source, job, true)
end

function restore.clean(source, job)
  local key = job.status == "succeeded" and "previous" or (job.status == "rolled_back" and "displaced" or "stage")
  local kind = cleanup_columns[key]
  local name, oid = job[kind.name], job[kind.oid]
  storage.exec("UPDATE restores SET cleanup_kind = ? WHERE id = ?", { key, job.id })
  update(job.id, "cleaning")
  inventory(source, job, function(err, found)
    if err then return update(job.id, "needs_review", err) end
    if not found[name] and oid ~= "" then return finish_cleanup(job, kind, true) end
    if not matches(found, name, oid) then return update(job.id, "needs_review", "无法确认要清理的数据库身份。") end
    pg.sql(source, job.control, "DROP DATABASE " .. pg.ident(name), function(drop_error)
      inventory(source, job, function(check_error, state)
        if check_error then return update(job.id, "needs_review", drop_error or check_error) end
        if not state[name] then return finish_cleanup(job, kind, true) end
        if matches(state, name, oid) then
          return finish_cleanup(job, kind, false, drop_error or "数据库仍存在，可以重试清理。")
        end
        update(job.id, "needs_review", "待清理数据库的身份发生变化。")
      end)
    end)
  end)
end

return restore
