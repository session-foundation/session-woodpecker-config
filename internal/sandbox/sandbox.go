// Package sandbox runs config evaluation in a resource-limited child process.
//
// Configs come from arbitrary commits, so evaluation has to be bounded, and it cannot be done
// in-process: go-jsonnet has no way to interrupt an evaluation, and a runaway one could exhaust the
// service's memory.  The child is the service's own binary, re-executed with ChildArg.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/session-foundation/session-woodpecker-config/internal/eval"
)

// ChildArg is the hidden first argument that makes the binary run as an evaluation child; main
// must hand such invocations to RunChild before doing anything else.
const ChildArg = "__eval-child"

// maxOutput bounds how much evaluated output the parent will accept from a child.
const maxOutput = 32 << 20

// Lang selects the evaluator.
type Lang string

const (
	Jsonnet  Lang = "jsonnet"
	Starlark Lang = "starlark"
)

// Job is one config file to evaluate.
type Job struct {
	Lang     Lang   `json:"lang"`
	Filename string `json:"filename"`
	Source   string `json:"source"`
	// Context is passed to the config as ctx; nil passes nothing (for jsonnet) or None.
	Context any `json:"ctx,omitempty"`
}

type childResult struct {
	Result any    `json:"result"`
	Error  string `json:"error,omitempty"`
}

// ConfigError is a failure caused by the config itself, whose message is meant for the config's
// author, as opposed to a failure of the service.
type ConfigError struct {
	Msg string
}

func (e *ConfigError) Error() string { return e.Msg }

// Limits bounds each evaluation.
type Limits struct {
	Timeout time.Duration
	// Memory is the most memory the child may use, in bytes.
	Memory uint64
}

// Runner runs evaluations in child processes, at most a fixed number at a time.
type Runner struct {
	exe    string
	limits Limits
	slots  chan struct{}
}

// NewRunner creates a Runner that allows up to concurrency simultaneous children.
func NewRunner(limits Limits, concurrency int) (*Runner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding own executable: %w", err)
	}
	return &Runner{exe: exe, limits: limits, slots: make(chan struct{}, concurrency)}, nil
}

// Eval evaluates job in a child process and returns the resulting JSON-compatible value.
func (r *Runner) Eval(ctx context.Context, job Job) (any, error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	input, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, r.limits.Timeout)
	defer cancel()
	// The CPU limit counts every thread, so it only means something if the child's parallelism is
	// bounded too.  Evaluation itself is single-threaded; the second thread is for the GC.
	const childProcs = 2
	cpuSeconds := uint64(childProcs*r.limits.Timeout.Seconds()) + 1
	cmd := exec.CommandContext(ctx, r.exe, ChildArg,
		strconv.FormatUint(r.limits.Memory, 10), strconv.FormatUint(cpuSeconds, 10))
	cmd.Env = []string{"GOMAXPROCS=" + strconv.Itoa(childProcs)}
	cmd.Stdin = bytes.NewReader(input)
	stdout := &limitedBuffer{limit: maxOutput}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, &ConfigError{fmt.Sprintf("evaluation timed out after %s", r.limits.Timeout)}
	case stdout.overflow:
		return nil, &ConfigError{fmt.Sprintf("evaluation produced more than %d MiB of output", maxOutput>>20)}
	case err != nil:
		return nil, childFailure(err, stderr.String())
	}

	var res childResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("decoding child output: %w", err)
	}
	if res.Error != "" {
		return nil, &ConfigError{res.Error}
	}
	return res.Result, nil
}

// childFailure interprets a child that died without reporting a result.  Hitting a resource limit
// is the config's fault; anything else is ours.
func childFailure(err error, stderr string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(unix.WaitStatus); ok && ws.Signaled() && ws.Signal() == unix.SIGXCPU {
			return &ConfigError{"evaluation exceeded its CPU time limit"}
		}
		if exitErr.ExitCode() == memoryExitCode {
			return &ConfigError{"evaluation exceeded its memory limit"}
		}
		// Memory can still occasionally outrun watchMemory to the rlimit backstop, which the Go
		// runtime reports in a few different ways; deep jsonnet recursion grows the Go stack and can
		// instead hit the runtime's maximum stack size.
		for _, msg := range []string{"fatal error: runtime: out of memory", "fatal error: runtime: cannot allocate memory", "fatal error: stack overflow"} {
			if strings.Contains(stderr, msg) {
				return &ConfigError{"evaluation exceeded its memory limit"}
			}
		}
	}
	return fmt.Errorf("evaluation child failed: %w: %s", err, strings.TrimSpace(stderr))
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

// memoryExitCode is the child's exit status when it exceeds its memory limit.
const memoryExitCode = 3

// watchMemory exits the process once the memory the Go runtime is using exceeds limit.
func watchMemory(limit uint64) {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	for range time.Tick(10 * time.Millisecond) {
		metrics.Read(samples)
		if samples[0].Value.Uint64()-samples[1].Value.Uint64() > limit {
			os.Exit(memoryExitCode)
		}
	}
}

// RunChild is the child side of Runner.Eval: args are the arguments following ChildArg.  It returns
// the process exit code.
func RunChild(args []string) int {
	if err := runChild(args, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runChild(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: " + ChildArg + " MEMORY CPU-SECONDS")
	}
	memory, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid memory limit: %w", err)
	}
	cpu, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid CPU limit: %w", err)
	}
	// The memory limit is enforced by watchMemory, because the Go runtime does not reliably survive
	// hitting a hard limit: Go 1.27's garbage collector can crash with a nil dereference when an
	// allocation fails.  The rlimit is only a backstop, set well above the limit so that it is
	// reached only if memory grows faster than watchMemory samples it.
	//
	// RLIMIT_AS would also count the address space the Go runtime reserves up front, which makes
	// even trivial allocations fail; RLIMIT_DATA only counts writable memory actually mapped.
	if err := unix.Setrlimit(unix.RLIMIT_DATA, &unix.Rlimit{Cur: 2 * memory, Max: 2 * memory}); err != nil {
		return fmt.Errorf("setting memory limit: %w", err)
	}
	// Make the garbage collector work hard before the limit is reached.
	debug.SetMemoryLimit(int64(memory / 10 * 9))
	go watchMemory(memory)
	// Reaching the soft limit sends SIGXCPU, which the parent reports as such; reaching the hard
	// limit sends an anonymous SIGKILL, so the hard limit is only a backstop.
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &unix.Rlimit{Cur: cpu, Max: cpu + 1}); err != nil {
		return fmt.Errorf("setting CPU limit: %w", err)
	}

	var job Job
	if err := json.NewDecoder(in).Decode(&job); err != nil {
		return fmt.Errorf("decoding job: %w", err)
	}

	var res childResult
	var evalErr error
	switch job.Lang {
	case Jsonnet:
		res.Result, evalErr = eval.Jsonnet(job.Filename, job.Source, job.Context)
	case Starlark:
		res.Result, evalErr = eval.Starlark(job.Filename, job.Source, job.Context)
	default:
		return fmt.Errorf("unknown language %q", job.Lang)
	}
	if evalErr != nil {
		res.Result, res.Error = nil, evalErr.Error()
	}
	return json.NewEncoder(out).Encode(res)
}
