package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tickInterval = time.Second
	stopTimeout  = 10 * time.Second
	startTimeout = 3 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: herdr-agent-time <ensure|daemon|stop|status>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "ensure", "start":
		err = ensureDaemon()
	case "daemon":
		err = runDaemon()
	case "stop":
		err = stopDaemon()
	case "status":
		err = printStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-agent-time:", err)
		os.Exit(1)
	}
}

type paths struct {
	dir      string
	socket   string
	lock     string
	pid      string
	identity string
	state    string
	log      string
}

func resolvePaths() (paths, error) {
	socket := os.Getenv("HERDR_SOCKET_PATH")
	if socket == "" {
		return paths{}, errors.New("HERDR_SOCKET_PATH is not set; run this inside Herdr")
	}
	dir := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if dir == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			dir = filepath.Join(xdg, "herdr-agent-time")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return paths{}, err
			}
			dir = filepath.Join(home, ".local", "state", "herdr-agent-time")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return paths{}, err
	}
	sum := sha256.Sum256([]byte(socket))
	id := hex.EncodeToString(sum[:8])
	return paths{
		dir:      dir,
		socket:   socket,
		lock:     filepath.Join(dir, "daemon-"+id+".lock"),
		pid:      filepath.Join(dir, "daemon-"+id+".pid"),
		identity: filepath.Join(dir, "daemon-"+id+".json"),
		state:    filepath.Join(dir, "state-"+id+".json"),
		log:      filepath.Join(dir, "daemon-"+id+".log"),
	}, nil
}

func acquireLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockHeld(path string) bool {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

// daemonIdentity records which binary owns the running daemon. ensure uses it
// to replace a daemon after a plugin update, relink, or local rebuild.
type daemonIdentity struct {
	PID           int    `json:"pid"`
	Executable    string `json:"executable"`
	SHA256        string `json:"sha256"`
	PluginRoot    string `json:"plugin_root,omitempty"`
	StartedUnixMS int64  `json:"started_unix_ms"`
}

func currentIdentity() (daemonIdentity, error) {
	executable, err := os.Executable()
	if err != nil {
		return daemonIdentity{}, err
	}
	executable = filepath.Clean(executable)
	hash, err := fileSHA256(executable)
	if err != nil {
		return daemonIdentity{}, err
	}
	root := ""
	if value := os.Getenv("HERDR_PLUGIN_ROOT"); value != "" {
		root = filepath.Clean(value)
	}
	return daemonIdentity{
		PID:           os.Getpid(),
		Executable:    executable,
		SHA256:        hash,
		PluginRoot:    root,
		StartedUnixMS: time.Now().UnixMilli(),
	}, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (id daemonIdentity) matches(other daemonIdentity) bool {
	if id.SHA256 == "" || id.SHA256 != other.SHA256 || !samePath(id.Executable, other.Executable) {
		return false
	}
	if id.PluginRoot != "" && other.PluginRoot != "" && !samePath(id.PluginRoot, other.PluginRoot) {
		return false
	}
	return true
}

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		return filepath.Clean(path)
	}
	return resolve(left) == resolve(right)
}

func readIdentity(path string) (daemonIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonIdentity{}, err
	}
	var identity daemonIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return daemonIdentity{}, err
	}
	if identity.PID <= 0 || identity.Executable == "" || identity.SHA256 == "" {
		return daemonIdentity{}, errors.New("incomplete daemon identity")
	}
	return identity, nil
}

func writeIdentity(path string, identity daemonIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, writeErr := out.Write(data)
	if writeErr == nil {
		writeErr = out.Sync()
	}
	if closeErr := out.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func terminatePID(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitLockFree(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !lockHeld(path) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitLockHeld(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if lockHeld(path) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ensureDaemon() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	current, err := currentIdentity()
	if err != nil {
		return fmt.Errorf("identify executable: %w", err)
	}

	replaced := false
	if lockHeld(p.lock) {
		existing, identityErr := readIdentity(p.identity)
		if identityErr == nil && existing.matches(current) {
			fmt.Println("agent-time daemon already running")
			return nil
		}
		pid := 0
		if identityErr == nil {
			pid = existing.PID
		}
		if pid <= 0 {
			pid, _ = readPID(p.pid)
		}
		if pid <= 0 {
			return fmt.Errorf("agent-time daemon is running but its pid is unknown; run %q to stop it", "herdr-agent-time stop")
		}
		if err := terminatePID(pid); err != nil {
			return fmt.Errorf("stop old daemon pid %d: %w", pid, err)
		}
		if !waitLockFree(p.lock, stopTimeout) {
			return fmt.Errorf("old daemon pid %d did not stop", pid)
		}
		replaced = true
	}

	logFile, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(current.Executable, "daemon")
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	if !waitLockHeld(p.lock, startTimeout) {
		if !processAlive(pid) {
			return fmt.Errorf("daemon pid %d exited during startup; see %s", pid, p.log)
		}
		return fmt.Errorf("daemon pid %d did not acquire the lock; see %s", pid, p.log)
	}
	if identity, err := readIdentity(p.identity); err == nil && identity.PID != pid {
		fmt.Printf("agent-time daemon already running (pid %d)\n", identity.PID)
		return nil
	}
	if replaced {
		fmt.Printf("agent-time daemon restarted (pid %d)\n", pid)
	} else {
		fmt.Printf("agent-time daemon started (pid %d)\n", pid)
	}
	return nil
}

// clearOrphanedTokens removes tokens that a crashed daemon left behind. It is
// best effort: the server may be gone, and the state file is still removed.
func clearOrphanedTokens(p paths) {
	tracker := NewTracker()
	if err := tracker.Load(p.state); err != nil || len(tracker.Panes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownClearLimit)
	defer cancel()
	client := NewClient(p.socket)
	defer func() { _ = client.Close() }()
	for paneID, state := range tracker.Panes {
		if !state.HadToken {
			continue
		}
		if err := client.Clear(ctx, paneID); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-agent-time: clear orphaned token %s: %v\n", paneID, err)
		}
	}
}

func stopDaemon() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	running := lockHeld(p.lock)
	if running {
		pid := 0
		if identity, err := readIdentity(p.identity); err == nil {
			pid = identity.PID
		}
		if pid <= 0 {
			pid, err = readPID(p.pid)
			if err != nil {
				return fmt.Errorf("read pid: %w", err)
			}
		}
		if err := terminatePID(pid); err != nil {
			return fmt.Errorf("signal pid %d: %w", pid, err)
		}
		if !waitLockFree(p.lock, stopTimeout) {
			return fmt.Errorf("daemon pid %d did not stop", pid)
		}
		fmt.Printf("agent-time daemon stopped (pid %d)\n", pid)
	} else {
		fmt.Println("agent-time daemon is not running")
	}

	clearOrphanedTokens(p)
	if err := removeFile(p.state); err != nil {
		return fmt.Errorf("clear timer state: %w", err)
	}
	if !running {
		_ = removeFile(p.identity)
		_ = removeFile(p.pid)
	}
	if running {
		fmt.Println("agent-time timer state cleared")
	}
	return nil
}

func socketInode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("socket stat is not a syscall.Stat_t")
	}
	return stat.Ino, nil
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}
	return pid, nil
}

// statusDuration returns the value to display and whether a running value is
// stale because no daemon is updating it.
func statusDuration(state *PaneState, daemonRunning bool, now time.Time) (time.Duration, bool) {
	if !state.Running {
		return time.Duration(state.DurationMS) * time.Millisecond, false
	}
	start := time.UnixMilli(state.TurnStartMS)
	end := now
	stale := false
	if !daemonRunning {
		end = time.UnixMilli(state.LastSeenMS)
		if end.Before(start) {
			end = start
		}
		stale = true
	}
	duration := end.Sub(start)
	if duration < 0 {
		duration = 0
	}
	return duration, stale
}

func printStatus() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	running := lockHeld(p.lock)
	if running {
		pid := 0
		if identity, err := readIdentity(p.identity); err == nil {
			pid = identity.PID
		}
		if pid <= 0 {
			if value, err := readPID(p.pid); err == nil {
				pid = value
			}
		}
		if pid > 0 {
			fmt.Printf("daemon: running (pid %d)\n", pid)
		} else {
			fmt.Println("daemon: running (pid unknown)")
		}
	} else {
		fmt.Println("daemon: not running")
	}
	fmt.Printf("socket: %s\nstate:  %s\n", p.socket, p.state)

	tracker := NewTracker()
	if err := tracker.Load(p.state); err != nil {
		return err
	}
	if len(tracker.Panes) == 0 {
		fmt.Println("panes: none")
		return nil
	}
	paneIDs := make([]string, 0, len(tracker.Panes))
	for paneID := range tracker.Panes {
		paneIDs = append(paneIDs, paneID)
	}
	slices.Sort(paneIDs)
	now := time.Now()
	for _, paneID := range paneIDs {
		state := tracker.Panes[paneID]
		duration, stale := statusDuration(state, running, now)
		fmt.Printf("%s agent=%s status=%s running=%t completed=%t stale=%t duration=%s\n",
			paneID, state.Agent, state.Status, state.Running, state.Completed, stale, formatDuration(duration.Milliseconds()))
	}
	return nil
}

func openLog(p paths) (*log.Logger, *os.File, error) {
	file, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return log.New(file, "", log.LstdFlags), file, nil
}

func runDaemon() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	lock, err := acquireLock(p.lock)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil
		}
		return err
	}
	defer func() { _ = lock.Close() }()

	// A previous daemon may have crashed before removing these files. Clear
	// them before writing this daemon's identity so stop and ensure cannot
	// signal a stale PID.
	_ = removeFile(p.identity)
	_ = removeFile(p.pid)

	pid := os.Getpid()
	if err := writeFileAtomic(p.pid, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	defer func() { _ = removeFile(p.pid) }()

	logger, logFile, err := openLog(p)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	logger.Printf("daemon start pid=%d socket=%s", pid, p.socket)

	identity, identityErr := currentIdentity()
	if identityErr != nil {
		logger.Printf("identity: %v", identityErr)
	} else if err := writeIdentity(p.identity, identity); err != nil {
		logger.Printf("write identity: %v", err)
	}
	defer func() { _ = removeFile(p.identity) }()

	tracker := NewTracker()
	if err := tracker.Load(p.state); err != nil {
		logger.Printf("load state: %v", err)
	}
	client := NewClient(p.socket)
	defer func() { _ = client.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	daemon := newDaemon(p, tracker, client, logger)
	return daemon.run(ctx, ticker.C)
}
