local M = {}

function M.describe(count)
  if count == 1 then
    return "第一次访问"
  end
  return "累计访问 " .. count .. " 次"
end

return M
