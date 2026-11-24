package lua

import (
	"errors"
	"os/exec"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

const processStderrLimit = 64 * 1024

type processExit struct {
	callback  *glua.LFunction
	pid       int
	code      int
	stderr    string
	truncated bool
	err       string
}

func (r *Runtime) processModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("start", L.NewFunction(func(L *glua.LState) int {
		program := L.CheckString(1)
		arguments := L.CheckTable(2)
		callback := L.CheckFunction(3)
		args := make([]string, arguments.Len())
		for index := range args {
			value, ok := arguments.RawGetInt(index + 1).(glua.LString)
			if !ok {
				L.ArgError(2, "expected an array of strings")
				return 0
			}
			args[index] = string(value)
		}
		var count int
		arguments.ForEach(func(_, _ glua.LValue) { count++ })
		if count != len(args) {
			L.ArgError(2, "expected an array of strings")
			return 0
		}

		stderr := &processStderr{}
		cmd := exec.CommandContext(r.ctx, program, args...)
		if env := L.OptTable(4, nil); env != nil {
			cmd.Env = processEnvironment(L, cmd.Environ(), env)
		}
		cmd.Stderr = stderr
		cmd.WaitDelay = time.Second
		if err := cmd.Start(); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		r.processes.Add(1)
		go r.waitProcess(cmd, stderr, callback)
		L.Push(glua.LNumber(cmd.Process.Pid))
		return 1
	}))
	return module
}

func processEnvironment(L *glua.LState, inherited []string, changes *glua.LTable) []string {
	overrides := make(map[string]glua.LValue)
	changes.ForEach(func(key, value glua.LValue) {
		name, ok := key.(glua.LString)
		if !ok || name == "" || strings.ContainsAny(string(name), "=\x00") {
			L.ArgError(4, "invalid environment variable name")
		}
		text, isString := value.(glua.LString)
		if value != glua.LFalse && (!isString || strings.ContainsRune(string(text), 0)) {
			L.ArgError(4, "environment values must be strings or false")
		}
		overrides[string(name)] = value
	})

	env := make([]string, 0, len(inherited)+len(overrides))
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if _, changed := overrides[name]; !changed {
			env = append(env, entry)
		}
	}
	for name, value := range overrides {
		if text, ok := value.(glua.LString); ok {
			env = append(env, name+"="+string(text))
		}
	}
	return env
}

func (r *Runtime) waitProcess(cmd *exec.Cmd, stderr *processStderr, callback *glua.LFunction) {
	defer r.processes.Done()
	err := cmd.Wait()
	exit := processExit{
		callback:  callback,
		pid:       cmd.Process.Pid,
		code:      cmd.ProcessState.ExitCode(),
		stderr:    stderr.text.String(),
		truncated: stderr.truncated,
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		exit.err = err.Error()
	}
	select {
	case r.processExits <- exit:
	case <-r.ctx.Done():
	}
}

func (r *Runtime) reportProcessExit(L *glua.LState, exit processExit) {
	result := L.NewTable()
	result.RawSetString("pid", glua.LNumber(exit.pid))
	result.RawSetString("exitCode", glua.LNumber(exit.code))
	result.RawSetString("stderr", glua.LString(exit.stderr))
	if exit.truncated {
		result.RawSetString("stderrTruncated", glua.LTrue)
	}
	if exit.err != "" {
		result.RawSetString("error", glua.LString(exit.err))
	}
	if err := L.CallByParam(glua.P{Fn: exit.callback, NRet: 0, Protect: true}, result); err != nil && !r.exitRequested {
		r.opts.Logger.Error("process exit callback failed", "pid", exit.pid, "error", err)
	}
}

type processStderr struct {
	text      strings.Builder
	truncated bool
}

func (s *processStderr) Write(chunk []byte) (int, error) {
	size := min(len(chunk), processStderrLimit-s.text.Len())
	_, _ = s.text.Write(chunk[:size])
	s.truncated = s.truncated || size < len(chunk)
	return len(chunk), nil
}
