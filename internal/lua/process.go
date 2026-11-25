package lua

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

const (
	processOutputLimit = 64 * 1024
	processWaitDelay   = time.Second
	defaultExecTimeout = 30 * time.Second
)

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
		args := processArguments(L, arguments)

		stderr := &processOutput{}
		cmd := exec.CommandContext(r.ctx, program, args...)
		if env := L.OptTable(4, nil); env != nil {
			cmd.Env = processEnvironment(L, cmd.Environ(), env)
		}
		cmd.Stderr = stderr
		cmd.WaitDelay = processWaitDelay
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
	module.RawSetString("exec", L.NewFunction(r.execProcess))
	return module
}

func processArguments(L *glua.LState, arguments *glua.LTable) []string {
	args := make([]string, arguments.Len())
	for index := range args {
		value, ok := arguments.RawGetInt(index + 1).(glua.LString)
		if !ok {
			L.ArgError(2, "expected an array of strings")
		}
		args[index] = string(value)
	}
	var count int
	arguments.ForEach(func(_, _ glua.LValue) { count++ })
	if count != len(args) {
		L.ArgError(2, "expected an array of strings")
	}
	return args
}

func (r *Runtime) execProcess(L *glua.LState) int {
	L.CheckType(1, glua.LTString)
	program := L.CheckString(1)
	args := processArguments(L, L.CheckTable(2))
	timeout := defaultExecTimeout
	var stdin *strings.Reader
	if options := L.OptTable(3, nil); options != nil {
		if value := options.RawGetString("stdin"); value != glua.LNil {
			text, ok := value.(glua.LString)
			if !ok {
				L.ArgError(3, "stdin must be a string")
			}
			stdin = strings.NewReader(string(text))
		}
		if value := options.RawGetString("timeoutMs"); value != glua.LNil {
			number, ok := value.(glua.LNumber)
			if !ok || number < 1 || number > glua.LNumber(math.MaxInt64/int64(time.Millisecond)) ||
				math.Trunc(float64(number)) != float64(number) {
				L.ArgError(3, "timeoutMs must be a positive integer representable as a duration")
			}
			timeout = time.Duration(number) * time.Millisecond
		}
	}

	ctx, cancel := context.WithTimeout(r.execCtx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	configureExecCancellation(cmd)
	cmd.WaitDelay = processWaitDelay
	var stdout, stderr processOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if err := cmd.Start(); err != nil {
		L.Push(glua.LNil)
		L.Push(glua.LString(err.Error()))
		return 2
	}
	r.processes.Add(1)
	defer r.processes.Done()
	err := cmd.Wait()

	result := L.NewTable()
	result.RawSetString("exitCode", glua.LNumber(cmd.ProcessState.ExitCode()))
	result.RawSetString("stdout", glua.LString(stdout.text.String()))
	result.RawSetString("stderr", glua.LString(stderr.text.String()))
	result.RawSetString("stdoutTruncated", glua.LBool(stdout.truncated))
	result.RawSetString("stderrTruncated", glua.LBool(stderr.truncated))
	result.RawSetString("timedOut", glua.LBool(err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)))
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		result.RawSetString("error", glua.LString(err.Error()))
	}
	L.Push(result)
	return 1
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

func (r *Runtime) waitProcess(cmd *exec.Cmd, stderr *processOutput, callback *glua.LFunction) {
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

type processOutput struct {
	text      strings.Builder
	truncated bool
}

func (s *processOutput) Write(chunk []byte) (int, error) {
	size := min(len(chunk), processOutputLimit-s.text.Len())
	_, _ = s.text.Write(chunk[:size])
	s.truncated = s.truncated || size < len(chunk)
	return len(chunk), nil
}
