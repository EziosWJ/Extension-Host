package lua

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/EziosWJ/Extension-Host/hostsys/services"
	"github.com/EziosWJ/Extension-Host/internal/serviceaccess"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

var moduleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)

func (r *Runtime) setup(L *glua.LState) error {
	glua.OpenBase(L)
	glua.OpenPackage(L)
	glua.OpenCoroutine(L)
	glua.OpenTable(L)
	glua.OpenIo(L)
	glua.OpenOs(L)
	glua.OpenString(L)
	glua.OpenMath(L)

	L.SetField(L.GetGlobal("os"), "exit", L.NewFunction(func(L *glua.LState) int {
		code := L.OptInt(1, 0)
		r.exitRequested = true
		r.opts.Logger.Info("Lua requested plugin exit", "code", code)
		r.cancelExecution()
		L.RaiseError("plugin requested exit")
		return 0
	}))
	removeField(L, "package", "loadlib")
	removeField(L, "package", "loaders")
	r.installPrint(L)
	r.installRequire(L)

	host := L.NewTable()
	host.RawSetString("dataDir", glua.LString(r.opts.DataDir))
	host.RawSetString("apiVersion", glua.LNumber(1))
	host.RawSetString("app", r.appModule(L))
	host.RawSetString("storage", r.storageModule(L))
	host.RawSetString("process", r.processModule(L))
	host.RawSetString("schedule", r.scheduleModule(L))
	host.RawSetString("config", r.configModule(L))
	host.RawSetString("json", r.jsonModule(L))
	host.RawSetString("http", unavailableModule(L, "host.http", "request"))
	host.RawSetString("ws", unavailableModule(L, "host.ws", "subscribe"))
	host.RawSetString("services", r.servicesModule(L))
	L.SetGlobal("host", host)
	return nil
}

func (r *Runtime) appModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("route", L.NewFunction(func(L *glua.LState) int {
		method := strings.ToUpper(L.CheckString(1))
		routePath := L.CheckString(2)
		handler := L.CheckFunction(3)
		if !validMethod(method) {
			L.RaiseError("host.app.route: invalid method %q", method)
			return 0
		}
		if !validRoutePath(routePath) {
			L.RaiseError("host.app.route: path %q must be a clean absolute path without a query", routePath)
			return 0
		}
		key := method + " " + routePath
		if _, exists := r.routes[key]; exists {
			L.RaiseError("host.app.route: duplicate route %s %s", method, routePath)
			return 0
		}
		r.routes[key] = handler
		if r.routeMethods[routePath] == nil {
			r.routeMethods[routePath] = make(map[string]bool)
		}
		r.routeMethods[routePath][method] = true
		return 0
	}))
	module.RawSetString("on", L.NewFunction(func(L *glua.LState) int {
		event := L.CheckString(1)
		handler := L.CheckFunction(2)
		if event != "start" && event != "stop" {
			L.RaiseError("host.app.on: unsupported lifecycle event %q (use start or stop)", event)
			return 0
		}
		if _, exists := r.hooks[event]; exists {
			L.RaiseError("host.app.on: duplicate %s hook", event)
			return 0
		}
		r.hooks[event] = handler
		return 0
	}))
	module.RawSetString("emit", L.NewFunction(func(L *glua.LState) int {
		event := L.CheckString(1)
		if strings.TrimSpace(event) == "" {
			L.RaiseError("host.app.emit: event name is required")
			return 0
		}
		data, err := toGo(L.Get(2))
		if err != nil {
			L.RaiseError("host.app.emit: %v", err)
			return 0
		}
		if r.opts.Emit == nil {
			L.RaiseError("host.app.emit: event delivery is not available")
			return 0
		}
		if err := r.opts.Emit(event, data); err != nil {
			L.RaiseError("host.app.emit: %v", err)
			return 0
		}
		return 0
	}))
	return module
}

func (r *Runtime) storageModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		db, ok := r.business(L, "host.storage.get")
		if !ok {
			return 0
		}
		key := L.CheckString(1)
		value, found, err := db.Get(r.ctx, key)
		if err != nil {
			L.RaiseError("host.storage.get: %v", err)
			return 0
		}
		if !found {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(glua.LString(value))
		return 1
	}))
	module.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		db, ok := r.business(L, "host.storage.set")
		if !ok {
			return 0
		}
		key := L.CheckString(1)
		value, err := storageValue(L.Get(2))
		if err != nil {
			L.RaiseError("host.storage.set: %v", err)
			return 0
		}
		if err := db.Set(r.ctx, key, value); err != nil {
			L.RaiseError("host.storage.set: %v", err)
			return 0
		}
		return 0
	}))
	module.RawSetString("delete", L.NewFunction(func(L *glua.LState) int {
		db, ok := r.business(L, "host.storage.delete")
		if !ok {
			return 0
		}
		key := L.CheckString(1)
		if err := db.Delete(r.ctx, key); err != nil {
			L.RaiseError("host.storage.delete: %v", err)
			return 0
		}
		return 0
	}))
	module.RawSetString("query", L.NewFunction(func(L *glua.LState) int {
		db, ok := r.business(L, "host.storage.query")
		if !ok {
			return 0
		}
		statement := L.CheckString(1)
		args, err := sqlArgs(L.OptTable(2, nil))
		if err != nil {
			L.RaiseError("host.storage.query: %v", err)
			return 0
		}
		result, err := db.Query(r.ctx, statement, args)
		if err != nil {
			L.RaiseError("host.storage.query: %v", err)
			return 0
		}
		rows := newArrayTable(L)
		for _, row := range result.Rows {
			rowTable := L.NewTable()
			for i, column := range result.Columns {
				rowTable.RawSetString(column, toLua(L, row[i]))
			}
			rows.Append(rowTable)
		}
		L.Push(rows)
		return 1
	}))
	module.RawSetString("exec", L.NewFunction(func(L *glua.LState) int {
		db, ok := r.business(L, "host.storage.exec")
		if !ok {
			return 0
		}
		statement := L.CheckString(1)
		args, err := sqlArgs(L.OptTable(2, nil))
		if err != nil {
			L.RaiseError("host.storage.exec: %v", err)
			return 0
		}
		affected, err := db.Exec(r.ctx, statement, args)
		if err != nil {
			L.RaiseError("host.storage.exec: %v", err)
			return 0
		}
		L.Push(glua.LNumber(affected))
		return 1
	}))
	return module
}

func (r *Runtime) business(L *glua.LState, caller string) (*store.Business, bool) {
	if r.opts.Storage == nil {
		L.RaiseError("%s is not available: the plugin has no business database", caller)
		return nil, false
	}
	return r.opts.Storage, true
}

func (r *Runtime) configModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		key := L.CheckString(1)
		if r.opts.Config == nil {
			L.Push(glua.LNil)
			return 1
		}
		value, ok := r.opts.Config()[key]
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(toLua(L, value))
		return 1
	}))
	module.RawSetString("all", L.NewFunction(func(L *glua.LState) int {
		values := map[string]any{}
		if r.opts.Config != nil {
			values = r.opts.Config()
		}
		L.Push(toLua(L, values))
		return 1
	}))
	return module
}

func (r *Runtime) jsonModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		value, err := toGo(L.Get(1))
		if err != nil {
			L.RaiseError("host.json.encode: %v", err)
			return 0
		}
		data, err := json.Marshal(value)
		if err != nil {
			L.RaiseError("host.json.encode: %v", err)
			return 0
		}
		L.Push(glua.LString(data))
		return 1
	}))
	module.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)
		decoder := json.NewDecoder(bytes.NewBufferString(text))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			L.RaiseError("host.json.decode: %v", err)
			return 0
		}
		L.Push(toLua(L, value))
		return 1
	}))
	return module
}

func (r *Runtime) servicesModule(L *glua.LState) *glua.LTable {
	options := r.opts.Services
	module := L.NewTable()
	module.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		items, err := options.List(r.ctx)
		if err != nil {
			L.RaiseError("%s", serviceError("host.services.list", err))
			return 0
		}
		result := newArrayTable(L)
		for _, item := range items {
			result.Append(serviceTable(L, item))
		}
		L.Push(result)
		return 1
	}))
	module.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		unit := L.CheckString(1)
		detail, err := options.Detail(r.ctx, unit)
		if err != nil {
			L.RaiseError("%s", serviceError("host.services.get", err))
			return 0
		}
		L.Push(serviceDetailTable(L, detail))
		return 1
	}))
	module.RawSetString("restart", L.NewFunction(func(L *glua.LState) int {
		unit := L.CheckString(1)
		if err := options.Restart(r.ctx, unit); err != nil {
			L.RaiseError("%s", serviceError("host.services.restart", err))
			return 0
		}
		L.Push(glua.LTrue)
		return 1
	}))
	return module
}

func serviceTable(L *glua.LState, service services.Service) *glua.LTable {
	table := L.NewTable()
	table.RawSetString("name", glua.LString(service.Name))
	table.RawSetString("description", glua.LString(service.Description))
	table.RawSetString("state", glua.LString(string(service.State)))
	table.RawSetString("stateDetail", glua.LString(service.StateDetail))
	table.RawSetString("bootState", glua.LString(string(service.BootState)))
	actions := newArrayTable(L)
	for _, action := range service.Actions {
		actions.Append(glua.LString(string(action)))
	}
	table.RawSetString("actions", actions)
	return table
}

func serviceDetailTable(L *glua.LState, detail services.ServiceDetail) *glua.LTable {
	table := L.NewTable()
	table.RawSetString("name", glua.LString(detail.Name))
	table.RawSetString("state", glua.LString(string(detail.State)))
	table.RawSetString("stateDetail", glua.LString(detail.StateDetail))
	table.RawSetString("bootState", glua.LString(string(detail.BootState)))
	if detail.MainPID != nil {
		table.RawSetString("mainPid", glua.LNumber(*detail.MainPID))
	}
	if detail.MemoryBytes != nil {
		table.RawSetString("memoryBytes", glua.LNumber(*detail.MemoryBytes))
	}
	if detail.CPUUsageNs != nil {
		table.RawSetString("cpuUsageNs", glua.LNumber(*detail.CPUUsageNs))
	}
	table.RawSetString("activeEnterTimestamp", glua.LString(detail.ActiveEnterTimestamp))
	table.RawSetString("fragmentPath", glua.LString(detail.FragmentPath))
	return table
}

func serviceError(caller string, err error) string {
	var accessErr *serviceaccess.Error
	if errors.As(err, &accessErr) {
		return accessErr.Message
	}
	var serviceErr *services.Error
	if errors.As(err, &serviceErr) {
		return fmt.Sprintf("%s: %s: %s", caller, serviceErr.Code, serviceErr.Message)
	}
	return fmt.Sprintf("%s: %v", caller, err)
}

func unavailableModule(L *glua.LState, module, function string) *glua.LTable {
	table := L.NewTable()
	table.RawSetString(function, L.NewFunction(unavailable(module, function)))
	return table
}

func unavailable(module, function string) glua.LGFunction {
	return func(L *glua.LState) int {
		L.RaiseError("%s.%s is not authorized: no capability grant is configured", module, function)
		return 0
	}
}

func (r *Runtime) installPrint(L *glua.LState) {
	L.SetGlobal("print", L.NewFunction(func(L *glua.LState) int {
		parts := make([]string, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts[i-1] = L.ToStringMeta(L.Get(i)).String()
		}
		r.opts.Logger.Info(strings.Join(parts, "\t"))
		return 0
	}))
}

func removeField(L *glua.LState, module, field string) {
	table, ok := L.GetGlobal(module).(*glua.LTable)
	if !ok {
		return
	}
	table.RawSetString(field, glua.LNil)
}

func (r *Runtime) installRequire(L *glua.LState) {
	L.SetGlobal("require", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		if !moduleNamePattern.MatchString(name) {
			L.RaiseError("require: invalid module name %q", name)
			return 0
		}
		if value, ok := r.modules[name]; ok {
			L.Push(value)
			return 1
		}
		if r.loading[name] {
			L.RaiseError("require: circular dependency on %q", name)
			return 0
		}
		relative := strings.ReplaceAll(name, ".", "/")
		candidates := []string{relative + ".lua", relative + "/init.lua"}
		roots := []string{r.entryDir}
		if r.entryDir != r.opts.PackageRoot {
			roots = append(roots, r.opts.PackageRoot)
		}
		for _, root := range roots {
			for _, candidate := range candidates {
				file := filepath.Join(root, filepath.FromSlash(candidate))
				if !withinRoot(r.opts.PackageRoot, file) {
					continue
				}
				info, err := os.Stat(file)
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				r.loading[name] = true
				value, err := r.loadModuleFile(L, name, file)
				delete(r.loading, name)
				if err != nil {
					L.RaiseError("require %q: %v", name, err)
					return 0
				}
				r.modules[name] = value
				if loaded := packageLoaded(L); loaded != nil {
					loaded.RawSetString(name, value)
				}
				L.Push(value)
				return 1
			}
		}
		L.RaiseError("require: module %q was not found next to the entry script or in the package root", name)
		return 0
	}))
}

func (r *Runtime) loadModuleFile(L *glua.LState, name, file string) (glua.LValue, error) {
	fn, err := L.LoadFile(file)
	if err != nil {
		return nil, err
	}
	if err := L.CallByParam(glua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
		return nil, err
	}
	value := L.Get(-1)
	L.Pop(1)
	if value == glua.LNil {
		value = glua.LTrue
	}
	return value, nil
}

func packageLoaded(L *glua.LState) *glua.LTable {
	packageTable, ok := L.GetGlobal("package").(*glua.LTable)
	if !ok {
		return nil
	}
	loaded, ok := packageTable.RawGetString("loaded").(*glua.LTable)
	if !ok {
		return nil
	}
	return loaded
}

func validMethod(method string) bool {
	if method == "" {
		return false
	}
	for _, character := range method {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func validRoutePath(routePath string) bool {
	if !strings.HasPrefix(routePath, "/") || strings.ContainsAny(routePath, "?#\\ ") {
		return false
	}
	return path.Clean(routePath) == routePath
}
