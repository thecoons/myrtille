// Package servicelifecycle starts and stops the service under test itself
// (service.managed.start_command), for projects that want a fresh instance
// per `myrtille run` rather than assuming one is already up. See
// docs/plans/service-lifecycle.md.
package servicelifecycle

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/style"
)

// stopPollInterval paces Stop's "has the port freed up yet" polling —
// deliberately fixed, not configurable: unlike Readiness.Interval (which
// trades off against a real startup budget), this only affects how quickly
// Stop notices a shutdown that already happened, not correctness.
const stopPollInterval = 200 * time.Millisecond

// signalByName maps the config-level signal names (see
// config.validStopSignals, kept in sync) to the actual syscall.Signal Stop
// sends.
var signalByName = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM,
	"INT":  syscall.SIGINT,
	"HUP":  syscall.SIGHUP,
	"QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL,
}

// readinessClientTimeout bounds a single readiness HTTP probe, so a
// connection that hangs (accepts but never responds) can't stall the whole
// poll loop indefinitely — a failed/slow probe is just treated as "not
// ready yet" and retried after Readiness.Interval.
const readinessClientTimeout = 2 * time.Second

// Summary reports what Start/Stop did, for a minimal report section (see
// docs/plans/service-lifecycle.md tranche 5) — mirrors how
// initphase.CommandResult reports init.command.
type Summary struct {
	Command    string
	ReadyAfter time.Duration
	// Stop is nil until Stop has actually run (e.g. if the run panics
	// before its deferred Stop call, though that call itself always runs
	// via defer in practice — see orchestrator.Run).
	Stop *StopResult
}

// Handle represents a running, started service, tracked by the process
// group Start launched it in — not by its direct PID, since the direct
// `sh -c` process may have already exited by the time Start returns (e.g.
// a launcher that backgrounds the real server and exits itself). See
// docs/plans/service-lifecycle.md tranche 0, which confirmed a
// same-process-group child stays killable via the group even after that.
type Handle struct {
	pgid       int
	logPath    string
	keepLog    bool
	command    string
	readyAfter time.Duration
}

// ReadyAfter reports how long Start waited for the readiness probe to
// succeed.
func (h *Handle) ReadyAfter() time.Duration { return h.readyAfter }

// Command returns the service.managed.start_command Start launched.
func (h *Handle) Command() string { return h.command }

// Summary returns a report-ready Summary for h, with Stop left nil —
// the caller fills it in once Stop has actually run.
func (h *Handle) Summary() *Summary {
	return &Summary{Command: h.command, ReadyAfter: h.readyAfter}
}

// Lifecycle starts and stops the service under test around a myrtille run
// — either doing nothing (external: service.managed unset, the service is
// assumed to already be running) or actually launching/stopping it
// (managed: service.managed configured). New selects the right one from
// cfg; orchestrator.Run drives Start/Stop through this interface without
// branching on which kind it got, so a future lifecycle kind (e.g. a
// docker-compose-backed one) only adds a case to New, not a new `if` in
// orchestrator.Run. See docs/plans/service-lifecycle.md's "Extension"
// section.
type Lifecycle interface {
	// Start does whatever this lifecycle needs to bring the service up.
	// Returns (nil, nil) when there's nothing to do (external) — the
	// caller registers a Stop() defer only when Start returns a non-nil
	// Summary, so it only ever cleans up what was actually started.
	Start(stderr io.Writer) (*Summary, error)
	// Stop tears down whatever Start brought up. Only called when Start
	// returned a non-nil Summary.
	Stop() *StopResult
}

// New returns the Lifecycle appropriate for cfg: external when
// cfg.Service.Managed is nil (today's default — a service assumed to
// already be running), managed otherwise.
func New(cfg *config.Config) Lifecycle {
	if cfg.Service.Managed == nil {
		return externalLifecycle{}
	}
	return &managedLifecycle{cfg: cfg}
}

// externalLifecycle is used when service.managed is unset: myrtille never
// touches the service, so both methods are pure no-ops.
type externalLifecycle struct{}

func (externalLifecycle) Start(io.Writer) (*Summary, error) { return nil, nil }
func (externalLifecycle) Stop() *StopResult                 { return nil }

// managedLifecycle wraps Start/Handle.Stop — the tranche 0-6 primitives,
// unchanged in behavior — behind the Lifecycle interface. Start/Stop on
// Handle stay exported and directly usable on their own (existing tests in
// this package call them directly); managedLifecycle is just a thin
// adapter over them for orchestrator.Run's benefit.
type managedLifecycle struct {
	cfg    *config.Config
	handle *Handle
}

func (m *managedLifecycle) Start(stderr io.Writer) (*Summary, error) {
	style.New(stderr).Step("starting service...")
	h, err := Start(m.cfg, stderr)
	if err != nil {
		return nil, err
	}
	m.handle = h
	return h.Summary(), nil
}

func (m *managedLifecycle) Stop() *StopResult {
	return m.handle.Stop(m.cfg)
}

// Start runs cfg.Service.Managed.StartCommand via `sh -c`, inheriting the
// process environment (same rule as init.command/k6.script), in its own
// process group (see Handle), then polls
// cfg.Service.Managed.Readiness.URL — resolved against cfg.Service.BaseURL
// — until it responds with a status below 400, the process exits on its
// own first, or Readiness.Timeout elapses. Callers must only call Start
// when cfg.Service.Managed is non-nil (see orchestrator.Run) — it
// dereferences it directly, unchecked.
//
// The command's own stdout/stderr are captured to a file rather than
// streamed live through stderr: the service runs in parallel with the rest
// of the pipeline (init/k6), so an interleaved stream would be unreadable.
// On failure, the last lines of that captured log are included in the
// returned error to help diagnose why the service never came up. Without
// Service.Managed.LogFile configured, that capture file is a throwaway
// temp file, removed once Stop runs; with it set, Start writes there
// instead (created fresh — truncated, not appended, so the file always
// reflects only the most recent run) and Stop leaves it in place.
func Start(cfg *config.Config, stderr io.Writer) (*Handle, error) {
	m := cfg.Service.Managed

	readyURL, err := resolveReadinessURL(cfg.Service.BaseURL, m.Readiness.URL)
	if err != nil {
		return nil, fmt.Errorf("resolving service.managed.readiness.url: %w", err)
	}

	// Refuse to start a second instance on top of whatever's already
	// answering readyURL — deliberately no attempt to guess whether it's
	// "ours" (see docs/plans/service-lifecycle.md Décisions): if
	// something already responds successfully, fail loudly rather than
	// silently reusing or clobbering it.
	preflightClient := &http.Client{Timeout: readinessClientTimeout}
	if isReady(preflightClient, readyURL) {
		return nil, fmt.Errorf("service.managed.readiness.url (%s) is already responding; refusing to start a second instance via service.managed.start_command", readyURL)
	}

	logPath := cfg.ServiceLogFilePath()
	keepLog := logPath != ""

	var logFile *os.File
	if keepLog {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating service.managed.log_file directory: %w", err)
		}
		logFile, err = os.Create(logPath)
		if err != nil {
			return nil, fmt.Errorf("creating service.managed.log_file: %w", err)
		}
		style.New(stderr).Info("service log: %s", logPath)
	} else {
		logFile, err = os.CreateTemp("", "myrtille-service-log-*.txt")
		if err != nil {
			return nil, fmt.Errorf("creating service log file: %w", err)
		}
		logPath = logFile.Name()
	}

	cmd := exec.Command("sh", "-c", m.StartCommand)
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		if !keepLog {
			os.Remove(logPath)
		}
		return nil, fmt.Errorf("starting service.managed.start_command: %w", err)
	}
	pgid := cmd.Process.Pid

	// cmd.Wait() must run concurrently with the readiness poll below,
	// never blocking it — the direct sh process may itself exit almost
	// immediately (e.g. after backgrounding the real server), which is
	// expected and not itself a failure; see tranche 0.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := m.Readiness.Timeout.Duration()
	interval := m.Readiness.Interval.Duration()
	start := time.Now()
	deadline := start.Add(timeout)
	client := &http.Client{Timeout: readinessClientTimeout}

	for {
		if isReady(client, readyURL) {
			logFile.Close()
			return &Handle{pgid: pgid, logPath: logPath, keepLog: keepLog, command: m.StartCommand, readyAfter: time.Since(start)}, nil
		}

		select {
		case werr := <-exited:
			if werr != nil {
				// A real failure (non-zero exit, signal, exec error):
				// fail fast rather than waiting out the full timeout.
				logFile.Close()
				tail := tailFile(logPath)
				if !keepLog {
					os.Remove(logPath)
				}
				return nil, fmt.Errorf("service exited before becoming ready (%v); last log lines:\n%s", werr, tail)
			}
			// A clean exit (code 0) before readiness is the expected
			// shape for a launcher that backgrounds the real server and
			// exits itself — not a failure (see tranche 0). Stop
			// selecting on this now-drained channel and keep polling
			// readiness purely over HTTP until Readiness.Timeout.
			exited = nil
		default:
		}

		if time.Now().After(deadline) {
			logFile.Close()
			tail := tailFile(logPath)
			// The service never came up: best-effort kill so a stuck
			// start_command doesn't linger after the run gives up on it.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			if !keepLog {
				os.Remove(logPath)
			}
			return nil, fmt.Errorf("service did not become ready within %s; last log lines:\n%s", timeout, tail)
		}

		time.Sleep(interval)
	}
}

func isReady(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode < 400
}

// resolveReadinessURL resolves readinessURL against baseURL the way a
// browser resolves a link: an absolute readinessURL (its own scheme) is
// used as-is, a path-only one (e.g. "/healthz") is joined onto baseURL.
func resolveReadinessURL(baseURL, readinessURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parsing service.base_url: %w", err)
	}
	ref, err := url.Parse(readinessURL)
	if err != nil {
		return "", fmt.Errorf("parsing service.readiness.url: %w", err)
	}
	return base.ResolveReference(ref).String(), nil
}

// tailFile returns the last lines of the file at path (empty/unreadable
// files return a placeholder), for a readiness-failure error message.
func tailFile(path string) string {
	const maxLines = 40
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "(no service output captured)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

// StopResult reports how Stop went, for a minimal report summary (see
// docs/plans/service-lifecycle.md tranche 5).
type StopResult struct {
	Signal string
	// Clean is true when the service's port stopped accepting connections
	// within StopTimeout after Signal was sent.
	Clean bool
	// Err is set only when sending Signal itself failed (e.g. the process
	// group was already gone) — never set just because the service didn't
	// stop within StopTimeout (that's Clean == false, not an error: Stop
	// is best-effort, like RunTeardown, and must never fail the run).
	Err error
}

// Stop sends cfg.Service.Managed.StopSignal to the whole process group
// Start launched (not just the direct PID — see Handle), then waits up to
// cfg.Service.Managed.StopTimeout for Service.BaseURL's port to stop
// accepting connections at all, as the most direct available signal that
// the process actually exited (deliberately not reusing the readiness
// HTTP check: a service mid-shutdown might still answer with a non-2xx
// status while still very much alive, which would report a stop as
// "clean" too early). Best-effort — like RunTeardown, it never returns an
// error for "didn't stop in time", only for a failure to send the signal
// at all. Removes the log file Start captured, regardless of outcome —
// unless Service.Managed.LogFile was configured, in which case it's left
// in place for the user to inspect after the run. Callers must only call
// Stop when cfg.Service.Managed is non-nil, same as Start.
func (h *Handle) Stop(cfg *config.Config) *StopResult {
	m := cfg.Service.Managed

	if !h.keepLog {
		defer os.Remove(h.logPath)
	}

	result := &StopResult{Signal: m.StopSignal}

	sig, ok := signalByName[m.StopSignal]
	if !ok {
		result.Err = fmt.Errorf("unknown stop signal %q", m.StopSignal)
		return result
	}
	if err := syscall.Kill(-h.pgid, sig); err != nil {
		result.Err = fmt.Errorf("sending %s to process group: %w", m.StopSignal, err)
		return result
	}

	base, err := url.Parse(cfg.Service.BaseURL)
	if err != nil {
		result.Err = fmt.Errorf("parsing service.base_url: %w", err)
		return result
	}
	hostport := base.Host

	deadline := time.Now().Add(m.StopTimeout.Duration())
	for {
		if portFree(hostport) {
			result.Clean = true
			return result
		}
		if time.Now().After(deadline) {
			return result
		}
		time.Sleep(stopPollInterval)
	}
}

// portFree reports whether hostport ("host:port") is no longer accepting
// TCP connections — a plain connect probe, not an HTTP request: during
// shutdown a service might still accept but respond slowly/oddly, and
// what Stop actually wants to know is "has the process released the
// port", not "does it still answer HTTP requests".
func portFree(hostport string) bool {
	conn, err := net.DialTimeout("tcp", hostport, readinessClientTimeout)
	if err != nil {
		return true
	}
	conn.Close()
	return false
}
