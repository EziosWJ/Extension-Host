package lua

import (
	"errors"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"
)

const (
	spawnHandleTypeName      = "host.process.handle"
	spawnStreamBufferLimit   = 256 * 1024
	spawnStreamChunkSize     = 16 * 1024
	spawnStreamFlushInterval = 50 * time.Millisecond
	defaultSpawnGraceMs      = 2000
	maxSpawnGraceMs          = 30000
)

type spawnExitResult struct {
	code               int
	timedOut           bool
	stopped            bool
	stdoutDroppedBytes int64
	stderrDroppedBytes int64
	err                string
}

type spawnedProcess struct {
	runtime *Runtime
	pid     int

	// Accessed strictly on the Lua goroutine.
	onStdout   *glua.LFunction
	onStderr   *glua.LFunction
	onExit     *glua.LFunction
	running    bool
	nextStderr bool

	// Protected by runtime.spawnMu.
	queued bool

	stdout *spawnStream
	stderr *spawnStream

	waitDone chan struct{}
	stopCh   chan time.Duration

	mu         sync.Mutex
	exited     bool
	stopped    bool
	timedOut   bool
	killed     bool
	exitResult *spawnExitResult
}

func registerSpawnHandleMetatable(L *glua.LState) {
	mt := L.NewTypeMetatable(spawnHandleTypeName)
	index := L.NewTable()
	index.RawSetString("pid", L.NewFunction(spawnHandlePID))
	index.RawSetString("running", L.NewFunction(spawnHandleRunning))
	index.RawSetString("stop", L.NewFunction(spawnHandleStop))
	L.SetField(mt, "__index", index)
}

func checkSpawnHandle(L *glua.LState) *spawnedProcess {
	ud := L.CheckUserData(1)
	if p, ok := ud.Value.(*spawnedProcess); ok && p != nil {
		return p
	}
	L.ArgError(1, "process handle expected")
	return nil
}

func spawnHandlePID(L *glua.LState) int {
	p := checkSpawnHandle(L)
	L.Push(glua.LNumber(p.pid))
	return 1
}

func spawnHandleRunning(L *glua.LState) int {
	p := checkSpawnHandle(L)
	L.Push(glua.LBool(p.running))
	return 1
}

func spawnHandleStop(L *glua.LState) int {
	p := checkSpawnHandle(L)
	graceMs := defaultSpawnGraceMs
	if value := L.Get(2); value != glua.LNil {
		number, ok := value.(glua.LNumber)
		if !ok || number < 0 || number > maxSpawnGraceMs ||
			math.Trunc(float64(number)) != float64(number) {
			L.ArgError(2, "graceMs must be an integer between 0 and 30000")
		}
		graceMs = int(number)
	}
	L.Push(glua.LBool(p.requestStop(time.Duration(graceMs) * time.Millisecond)))
	return 1
}

func (r *Runtime) spawnProcess(L *glua.LState) int {
	L.CheckType(1, glua.LTString)
	program := L.CheckString(1)
	args := processArguments(L, L.CheckTable(2))

	var (
		cwd      string
		envTable *glua.LTable
		stdin    *strings.Reader
		timeout  time.Duration
		onStdout *glua.LFunction
		onStderr *glua.LFunction
		onExit   *glua.LFunction
	)
	if options := L.OptTable(3, nil); options != nil {
		if value := options.RawGetString("cwd"); value != glua.LNil {
			cwd = parseProcessCWD(L, 3, value)
		}
		if value := options.RawGetString("env"); value != glua.LNil {
			envTable = parseProcessEnvTable(L, 3, value)
		}
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
		if value := options.RawGetString("onStdout"); value != glua.LNil {
			fn, ok := value.(*glua.LFunction)
			if !ok {
				L.ArgError(3, "onStdout must be a function")
			}
			onStdout = fn
		}
		if value := options.RawGetString("onStderr"); value != glua.LNil {
			fn, ok := value.(*glua.LFunction)
			if !ok {
				L.ArgError(3, "onStderr must be a function")
			}
			onStderr = fn
		}
		if value := options.RawGetString("onExit"); value != glua.LNil {
			fn, ok := value.(*glua.LFunction)
			if !ok {
				L.ArgError(3, "onExit must be a function")
			}
			onExit = fn
		}
	}

	cmd := exec.Command(program, args...)
	configureProcessGroup(cmd)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if envTable != nil {
		cmd.Env = processEnvironment(L, 3, cmd.Environ(), envTable)
	}
	cmd.WaitDelay = processWaitDelay
	if stdin != nil {
		cmd.Stdin = stdin
	}

	if r.execCtx.Err() != nil || r.ctx.Err() != nil {
		L.Push(glua.LNil)
		L.Push(glua.LString(ErrStopped.Error()))
		return 2
	}

	proc := &spawnedProcess{
		runtime:  r,
		onStdout: onStdout,
		onStderr: onStderr,
		onExit:   onExit,
		running:  true,
		waitDone: make(chan struct{}),
		stopCh:   make(chan time.Duration, 1),
	}
	if onStdout != nil {
		proc.stdout = newSpawnStream(func() { r.enqueueSpawn(proc) })
		cmd.Stdout = proc.stdout
	}
	if onStderr != nil {
		proc.stderr = newSpawnStream(func() { r.enqueueSpawn(proc) })
		cmd.Stderr = proc.stderr
	}

	if err := cmd.Start(); err != nil {
		L.Push(glua.LNil)
		L.Push(glua.LString(err.Error()))
		return 2
	}
	proc.pid = cmd.Process.Pid

	r.processes.Add(1)
	go proc.run(cmd, timeout)

	ud := L.NewUserData()
	ud.Value = proc
	L.SetMetatable(ud, L.GetTypeMetatable(spawnHandleTypeName))
	L.Push(ud)
	return 1
}

func (p *spawnedProcess) requestStop(grace time.Duration) bool {
	if !p.running {
		return false
	}
	p.mu.Lock()
	if p.exited || p.stopped || p.timedOut || p.killed {
		p.mu.Unlock()
		return false
	}
	p.stopped = true
	_ = signalProcessGroup(p.pid, grace == 0)
	p.mu.Unlock()
	p.stopCh <- grace
	return true
}

func (p *spawnedProcess) run(cmd *exec.Cmd, timeout time.Duration) {
	defer p.runtime.processes.Done()

	superviseDone := make(chan struct{})
	go func() {
		defer close(superviseDone)
		p.supervise(timeout)
	}()

	err := cmd.Wait()

	p.mu.Lock()
	p.exited = true
	stopped := p.stopped
	timedOut := p.timedOut && err != nil
	p.mu.Unlock()

	close(p.waitDone)
	<-superviseDone

	var stdoutDropped, stderrDropped int64
	if p.stdout != nil {
		stdoutDropped = p.stdout.close()
	}
	if p.stderr != nil {
		stderrDropped = p.stderr.close()
	}

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	var errStr string
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		errStr = err.Error()
	}

	res := &spawnExitResult{
		code:               code,
		timedOut:           timedOut,
		stopped:            stopped,
		stdoutDroppedBytes: stdoutDropped,
		stderrDroppedBytes: stderrDropped,
		err:                errStr,
	}
	p.mu.Lock()
	p.exitResult = res
	p.mu.Unlock()

	p.runtime.enqueueSpawn(p)
}

func (p *spawnedProcess) supervise(timeout time.Duration) {
	var timeoutCh <-chan time.Time
	var timeoutTimer *time.Timer
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		defer timeoutTimer.Stop()
		timeoutCh = timeoutTimer.C
	}

	var graceCh <-chan time.Time
	var graceTimer *time.Timer
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()

	for {
		select {
		case <-p.waitDone:
			return
		case <-p.runtime.ctx.Done():
			p.mu.Lock()
			if !p.exited {
				p.killed = true
				_ = signalProcessGroup(p.pid, true)
			}
			p.mu.Unlock()
			return
		case <-timeoutCh:
			timeoutCh = nil
			p.mu.Lock()
			if !p.exited && !p.stopped && !p.killed {
				p.timedOut = true
				_ = signalProcessGroup(p.pid, true)
			}
			p.mu.Unlock()
		case grace := <-p.stopCh:
			if timeoutTimer != nil {
				timeoutTimer.Stop()
				timeoutCh = nil
			}
			if grace > 0 {
				graceTimer = time.NewTimer(grace)
				graceCh = graceTimer.C
			}
		case <-graceCh:
			graceCh = nil
			p.mu.Lock()
			if !p.exited {
				_ = signalProcessGroup(p.pid, true)
			}
			p.mu.Unlock()
		}
	}
}

func (p *spawnedProcess) hasReadyStream(force bool) bool {
	if p.stdout != nil && p.stdout.isReady(force) {
		return true
	}
	if p.stderr != nil && p.stderr.isReady(force) {
		return true
	}
	return false
}

func (r *Runtime) enqueueSpawn(p *spawnedProcess) {
	r.spawnMu.Lock()
	if !p.queued {
		p.queued = true
		r.spawnQueue = append(r.spawnQueue, p)
	}
	r.spawnMu.Unlock()
	select {
	case r.spawnNotify <- struct{}{}:
	default:
	}
}

func (r *Runtime) dequeueSpawn() *spawnedProcess {
	r.spawnMu.Lock()
	if len(r.spawnQueue) == 0 {
		r.spawnMu.Unlock()
		return nil
	}
	p := r.spawnQueue[0]
	copy(r.spawnQueue, r.spawnQueue[1:])
	r.spawnQueue[len(r.spawnQueue)-1] = nil
	r.spawnQueue = r.spawnQueue[:len(r.spawnQueue)-1]
	p.queued = false
	more := len(r.spawnQueue) > 0
	r.spawnMu.Unlock()
	if more {
		select {
		case r.spawnNotify <- struct{}{}:
		default:
		}
	}
	return p
}

func (r *Runtime) handleSpawnEvent(L *glua.LState) {
	p := r.dequeueSpawn()
	if p == nil || !p.running {
		return
	}

	p.mu.Lock()
	exitRes := p.exitResult
	p.mu.Unlock()
	exited := exitRes != nil

	delivered := false
	var moreStdout, moreStderr bool
	tryStdout := func() {
		if p.stdout == nil {
			return
		}
		chunk, more := p.stdout.takeChunk(exited)
		moreStdout = more
		if chunk != "" {
			delivered = true
			r.invokeSpawnStreamCallback(L, p.onStdout, "stdout", p.pid, chunk)
		}
	}
	tryStderr := func() {
		if p.stderr == nil {
			return
		}
		chunk, more := p.stderr.takeChunk(exited)
		moreStderr = more
		if chunk != "" {
			delivered = true
			r.invokeSpawnStreamCallback(L, p.onStderr, "stderr", p.pid, chunk)
		}
	}

	if p.nextStderr {
		tryStderr()
		if !delivered {
			tryStdout()
		}
	} else {
		tryStdout()
		if !delivered {
			tryStderr()
		}
	}
	p.nextStderr = !p.nextStderr

	if r.exitRequested {
		return
	}

	if delivered {
		if moreStdout || moreStderr || exited || p.hasReadyStream(false) {
			r.enqueueSpawn(p)
		}
		return
	}

	if exited {
		p.running = false
		if p.onExit != nil {
			r.invokeSpawnExitCallback(L, p, *exitRes)
		}
	}
}

func (r *Runtime) invokeSpawnStreamCallback(L *glua.LState, fn *glua.LFunction, stream string, pid int, chunk string) {
	if fn == nil {
		return
	}
	if err := L.CallByParam(glua.P{Fn: fn, NRet: 0, Protect: true}, glua.LString(chunk)); err != nil && !r.exitRequested {
		r.opts.Logger.Error("spawn stream callback failed", "stream", stream, "pid", pid, "error", err)
	}
}

func (r *Runtime) invokeSpawnExitCallback(L *glua.LState, p *spawnedProcess, exit spawnExitResult) {
	result := L.NewTable()
	result.RawSetString("pid", glua.LNumber(p.pid))
	result.RawSetString("exitCode", glua.LNumber(exit.code))
	result.RawSetString("timedOut", glua.LBool(exit.timedOut))
	result.RawSetString("stopped", glua.LBool(exit.stopped))
	result.RawSetString("stdoutDroppedBytes", glua.LNumber(exit.stdoutDroppedBytes))
	result.RawSetString("stderrDroppedBytes", glua.LNumber(exit.stderrDroppedBytes))
	if exit.err != "" {
		result.RawSetString("error", glua.LString(exit.err))
	}
	if err := L.CallByParam(glua.P{Fn: p.onExit, NRet: 0, Protect: true}, result); err != nil && !r.exitRequested {
		r.opts.Logger.Error("spawn exit callback failed", "pid", p.pid, "error", err)
	}
}

type spawnStream struct {
	mu       sync.Mutex
	buf      []byte
	dropped  int64
	timer    *time.Timer
	timerGen uint64
	flushed  bool
	closed   bool
	onReady  func()
}

func newSpawnStream(onReady func()) *spawnStream {
	return &spawnStream{onReady: onReady}
}

func (s *spawnStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	notify := false
	s.mu.Lock()
	avail := spawnStreamBufferLimit - len(s.buf)
	if avail > 0 {
		n := min(len(p), avail)
		s.buf = append(s.buf, p[:n]...)
		if n < len(p) {
			s.dropped += int64(len(p) - n)
		}
	} else {
		s.dropped += int64(len(p))
	}
	if len(s.buf) >= spawnStreamChunkSize {
		s.flushed = true
		s.stopTimerLocked()
		notify = true
	} else if len(s.buf) > 0 && !s.flushed && s.timer == nil && !s.closed {
		s.timerGen++
		gen := s.timerGen
		s.timer = time.AfterFunc(spawnStreamFlushInterval, func() {
			s.flushTimerFired(gen)
		})
	}
	s.mu.Unlock()
	if notify {
		s.onReady()
	}
	return len(p), nil
}

func (s *spawnStream) flushTimerFired(gen uint64) {
	notify := false
	s.mu.Lock()
	if s.timerGen == gen {
		s.timer = nil
		if len(s.buf) > 0 {
			s.flushed = true
			notify = true
		}
	}
	s.mu.Unlock()
	if notify {
		s.onReady()
	}
}

func (s *spawnStream) isReady(force bool) bool {
	s.mu.Lock()
	ready := len(s.buf) > 0 && (force || s.flushed || len(s.buf) >= spawnStreamChunkSize)
	s.mu.Unlock()
	return ready
}

func (s *spawnStream) takeChunk(force bool) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.buf) == 0 || (!force && !s.flushed && len(s.buf) < spawnStreamChunkSize) {
		return "", false
	}

	n := min(len(s.buf), spawnStreamChunkSize)
	chunk := string(s.buf[:n])
	rem := len(s.buf) - n
	if rem == 0 {
		s.buf = s.buf[:0]
		if cap(s.buf) > spawnStreamChunkSize*2 {
			s.buf = nil
		}
		s.flushed = false
		s.stopTimerLocked()
		return chunk, false
	}
	copy(s.buf, s.buf[n:])
	s.buf = s.buf[:rem]
	s.flushed = true
	s.stopTimerLocked()
	return chunk, true
}

func (s *spawnStream) close() int64 {
	s.mu.Lock()
	s.closed = true
	s.stopTimerLocked()
	if len(s.buf) > 0 {
		s.flushed = true
	}
	dropped := s.dropped
	s.mu.Unlock()
	return dropped
}

func (s *spawnStream) stopTimerLocked() {
	s.timerGen++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}
