package lua

import (
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

type scheduleEntry struct {
	name    string
	fn      *glua.LFunction
	stopped chan struct{}
}

type scheduleFire struct {
	entry *scheduleEntry
	done  chan error
}

func (r *Runtime) scheduleModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		nameValue, ok := L.Get(1).(glua.LString)
		if !ok {
			L.Push(glua.LNil)
			L.Push(glua.LString("host.schedule.register: task name must be a string"))
			return 2
		}
		expressionValue, ok := L.Get(2).(glua.LString)
		if !ok {
			L.Push(glua.LNil)
			L.Push(glua.LString("host.schedule.register: expression must be a string"))
			return 2
		}
		fn, ok := L.Get(3).(*glua.LFunction)
		if !ok {
			L.Push(glua.LNil)
			L.Push(glua.LString("host.schedule.register: callback must be a function"))
			return 2
		}
		name := strings.TrimSpace(string(nameValue))
		expression := strings.TrimSpace(string(expressionValue))
		if name == "" {
			L.Push(glua.LNil)
			L.Push(glua.LString("host.schedule.register: task name is required"))
			return 2
		}
		if err := r.scheduler.Validate(expression); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		if old, ok := r.schedules[name]; ok {
			close(old.stopped)
			delete(r.schedules, name)
		}
		entry := &scheduleEntry{name: name, fn: fn, stopped: make(chan struct{})}
		if err := r.scheduler.Register(name, expression, func() error { return r.invokeSchedule(entry) }); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		r.schedules[name] = entry
		L.Push(glua.LTrue)
		return 1
	}))
	module.RawSetString("validate", L.NewFunction(func(L *glua.LState) int {
		if err := r.scheduler.Validate(L.CheckString(1)); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LTrue)
		return 1
	}))
	module.RawSetString("next", L.NewFunction(func(L *glua.LState) int {
		next := r.scheduler.Next(L.CheckString(1))
		if next.IsZero() {
			L.Push(glua.LNil)
		} else {
			L.Push(glua.LString(next.UTC().Format(time.RFC3339)))
		}
		return 1
	}))
	module.RawSetString("unregister", L.NewFunction(func(L *glua.LState) int {
		nameValue, ok := L.Get(1).(glua.LString)
		if !ok {
			L.Push(glua.LFalse)
			return 1
		}
		name := strings.TrimSpace(string(nameValue))
		entry, ok := r.schedules[name]
		if !ok {
			L.Push(glua.LFalse)
			return 1
		}
		r.scheduler.Unregister(name)
		close(entry.stopped)
		delete(r.schedules, name)
		L.Push(glua.LTrue)
		return 1
	}))
	return module
}

// invokeSchedule asks the runtime loop to run entry's callback. It runs on a
// cron goroutine, so it never touches Lua state or the schedules map directly.
func (r *Runtime) invokeSchedule(entry *scheduleEntry) error {
	fire := scheduleFire{entry: entry, done: make(chan error, 1)}
	select {
	case r.scheduleFires <- fire:
	case <-r.ctx.Done():
		return nil
	case <-entry.stopped:
		return nil
	}
	select {
	case err := <-fire.done:
		return err
	case <-r.ctx.Done():
		return nil
	}
}

// handleScheduleFire runs one scheduled callback on the runtime loop goroutine.
func (r *Runtime) handleScheduleFire(L *glua.LState, fire scheduleFire) {
	entry, ok := r.schedules[fire.entry.name]
	if !ok || entry != fire.entry {
		fire.done <- nil
		return
	}
	err := L.CallByParam(glua.P{Fn: entry.fn, NRet: 0, Protect: true})
	if err != nil && !r.exitRequested {
		r.opts.Logger.Error("scheduled task callback failed", "task", entry.name, "error", err)
	}
	fire.done <- err
}

// clearSchedules removes every registration during runtime teardown. It runs
// on the runtime loop goroutine, so each stopped channel is closed exactly once.
func (r *Runtime) clearSchedules() {
	for name, entry := range r.schedules {
		r.scheduler.Unregister(name)
		close(entry.stopped)
		delete(r.schedules, name)
	}
}
