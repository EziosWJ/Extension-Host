package lua

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

const (
	processOutputLimit    = 64 * 1024
	maxProcessOutputLimit = 4 * 1024 * 1024
	processWaitDelay      = time.Second
	defaultExecTimeout    = 30 * time.Second
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
	registerSpawnHandleMetatable(L)

	module := L.NewTable()
	module.RawSetString("start", L.NewFunction(func(L *glua.LState) int {
		program := L.CheckString(1)
		arguments := L.CheckTable(2)
		callback := L.CheckFunction(3)
		args := processArguments(L, arguments)

		stderr := &processOutput{}
		cmd := exec.CommandContext(r.ctx, program, args...)
		configureExecCancellation(cmd)
		if env := L.OptTable(4, nil); env != nil {
			cmd.Env = processEnvironment(L, 4, cmd.Environ(), env)
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
	module.RawSetString("spawn", L.NewFunction(r.spawnProcess))
	module.RawSetString("lookPath", L.NewFunction(lookPath))
	return module
}

func lookPath(L *glua.LState) int {
	name, ok := L.Get(1).(glua.LString)
	if !ok || name == "" || strings.ContainsRune(string(name), 0) {
		L.ArgError(1, "name must be a non-empty string without NUL bytes")
		return 0
	}

	path, err := exec.LookPath(string(name))
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		L.Push(glua.LNil)
		L.Push(glua.LString(err.Error()))
		return 2
	}
	L.Push(glua.LString(path))
	return 1
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

func parseProcessCWD(L *glua.LState, argIndex int, value glua.LValue) string {
	text, ok := value.(glua.LString)
	if !ok || text == "" || strings.ContainsRune(string(text), 0) {
		L.ArgError(argIndex, "cwd must be a non-empty string without NUL bytes")
	}
	return string(text)
}

func parseProcessTimeout(L *glua.LState, argIndex int, value glua.LValue) time.Duration {
	number, ok := value.(glua.LNumber)
	if !ok || number < 1 || number > glua.LNumber(math.MaxInt64/int64(time.Millisecond)) ||
		math.Trunc(float64(number)) != float64(number) {
		L.ArgError(argIndex, "timeoutMs must be a positive integer representable as a duration")
	}
	return time.Duration(number) * time.Millisecond
}

func parseProcessEnvTable(L *glua.LState, argIndex int, value glua.LValue) *glua.LTable {
	table, ok := value.(*glua.LTable)
	if !ok {
		L.ArgError(argIndex, "env must be a table")
	}
	return table
}

func parseProcessOutputLimit(L *glua.LState, argIndex int, value glua.LValue) int {
	number, ok := value.(glua.LNumber)
	if !ok || number < 1 || number > maxProcessOutputLimit ||
		math.Trunc(float64(number)) != float64(number) {
		L.ArgError(argIndex, "maxOutputBytes must be an integer between 1 and 4194304")
	}
	return int(number)
}

func (r *Runtime) execProcess(L *glua.LState) int {
	L.CheckType(1, glua.LTString)
	program := L.CheckString(1)
	args := processArguments(L, L.CheckTable(2))
	timeout := defaultExecTimeout
	outputLimit := processOutputLimit
	var stdin *strings.Reader
	var cwd string
	var envTable *glua.LTable
	if options := L.OptTable(3, nil); options != nil {
		if value := options.RawGetString("stdin"); value != glua.LNil {
			text, ok := value.(glua.LString)
			if !ok {
				L.ArgError(3, "stdin must be a string")
			}
			stdin = strings.NewReader(string(text))
		}
		if value := options.RawGetString("timeoutMs"); value != glua.LNil {
			timeout = parseProcessTimeout(L, 3, value)
		}
		if value := options.RawGetString("cwd"); value != glua.LNil {
			cwd = parseProcessCWD(L, 3, value)
		}
		if value := options.RawGetString("env"); value != glua.LNil {
			envTable = parseProcessEnvTable(L, 3, value)
		}
		if value := options.RawGetString("maxOutputBytes"); value != glua.LNil {
			outputLimit = parseProcessOutputLimit(L, 3, value)
		}
	}

	ctx, cancel := context.WithTimeout(r.operationContext(L), timeout)
	background := false
	defer func() {
		if !background {
			cancel()
		}
	}()
	cmd := exec.CommandContext(ctx, program, args...)
	configureExecCancellation(cmd)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if envTable != nil {
		cmd.Env = processEnvironment(L, 3, cmd.Environ(), envTable)
	}
	cmd.WaitDelay = processWaitDelay
	stdout := processOutput{limit: outputLimit}
	stderr := processOutput{limit: outputLimit}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if req, ok := r.canYieldRequest(L); ok {
		background = true
		r.startAsync(req, func() luaResults {
			defer cancel()
			return runExec(ctx, cmd, &stdout, &stderr)
		})
		return L.Yield(r.asyncYieldSentinel)
	}
	r.processes.Add(1)
	defer r.processes.Done()
	return pushResults(L, runExec(ctx, cmd, &stdout, &stderr)(L))
}

func runExec(ctx context.Context, cmd *exec.Cmd, stdout, stderr *processOutput) luaResults {
	if err := cmd.Start(); err != nil {
		return func(*glua.LState) []glua.LValue {
			return []glua.LValue{glua.LNil, glua.LString(err.Error())}
		}
	}
	err := cmd.Wait()
	timedOut := err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)

	return func(L *glua.LState) []glua.LValue {
		result := L.NewTable()
		result.RawSetString("exitCode", glua.LNumber(cmd.ProcessState.ExitCode()))
		result.RawSetString("stdout", glua.LString(stdout.text.String()))
		result.RawSetString("stderr", glua.LString(stderr.text.String()))
		result.RawSetString("stdoutTruncated", glua.LBool(stdout.truncated))
		result.RawSetString("stderrTruncated", glua.LBool(stderr.truncated))
		result.RawSetString("timedOut", glua.LBool(timedOut))
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			result.RawSetString("error", glua.LString(err.Error()))
		}
		return []glua.LValue{result}
	}
}

func processEnvironment(L *glua.LState, argIndex int, inherited []string, changes *glua.LTable) []string {
	overrides := make(map[string]glua.LValue)
	changes.ForEach(func(key, value glua.LValue) {
		name, ok := key.(glua.LString)
		if !ok || name == "" || strings.ContainsAny(string(name), "=\x00") {
			L.ArgError(argIndex, "invalid environment variable name")
		}
		text, isString := value.(glua.LString)
		if value != glua.LFalse && (!isString || strings.ContainsRune(string(text), 0)) {
			L.ArgError(argIndex, "environment values must be strings or false")
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
	limit     int
	text      strings.Builder
	truncated bool
}

func (s *processOutput) Write(chunk []byte) (int, error) {
	limit := s.limit
	if limit <= 0 {
		limit = processOutputLimit
	}
	remaining := limit - s.text.Len()
	if remaining < 0 {
		remaining = 0
	}
	size := min(len(chunk), remaining)
	_, _ = s.text.Write(chunk[:size])
	s.truncated = s.truncated || size < len(chunk)
	return len(chunk), nil
}
