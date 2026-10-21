package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tickInterval = time.Second
	saveInterval = 10 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: herdr-agent-time <ensure|daemon|stop|status|configure>")
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
	case "configure":
		err = configureSidebar()
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
	dir    string
	socket string
	lock   string
	pid    string
	state  string
	log    string
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
		dir:    dir,
		socket: socket,
		lock:   filepath.Join(dir, "daemon-"+id+".lock"),
		pid:    filepath.Join(dir, "daemon-"+id+".pid"),
		state:  filepath.Join(dir, "state-"+id+".json"),
		log:    filepath.Join(dir, "daemon-"+id+".log"),
	}, nil
}

func acquireLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func lockHeld(path string) bool {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

func ensureDaemon() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	if lockHeld(p.lock) {
		fmt.Println("agent-time daemon already running")
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(executable, "daemon")
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
	fmt.Printf("agent-time daemon started (pid %d)\n", pid)
	return nil
}

func stopDaemon() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	if !lockHeld(p.lock) {
		_ = os.Remove(p.pid)
		fmt.Println("agent-time daemon is not running")
		return nil
	}
	pid, err := readPID(p.pid)
	if err != nil {
		return fmt.Errorf("read pid: %w", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !lockHeld(p.lock) {
			fmt.Printf("agent-time daemon stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon pid %d did not stop", pid)
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
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func printStatus() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	if lockHeld(p.lock) {
		pid, _ := readPID(p.pid)
		fmt.Printf("daemon: running (pid %d)\n", pid)
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
	for paneID, st := range tracker.Panes {
		duration := st.DurationMS
		if st.Running && st.TurnStartMS > 0 {
			duration = time.Now().UnixMilli() - st.TurnStartMS
		}
		fmt.Printf("%s agent=%s status=%s running=%t completed=%t duration=%s\n",
			paneID, st.Agent, st.Status, st.Running, st.Completed, formatDuration(duration))
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
	defer lock.Close()

	if err := os.WriteFile(p.pid, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(p.pid)

	logger, logFile, err := openLog(p)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger.Printf("daemon start pid=%d socket=%s", os.Getpid(), p.socket)

	tracker := NewTracker()
	if err := tracker.Load(p.state); err != nil {
		logger.Printf("load state: %v", err)
	}
	client := NewClient(p.socket, logger)
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	lastSave := time.Now()
	dirty := false
	lastInode, _ := socketInode(p.socket)
	failures := 0
	var lastListLog time.Time
	var lastReportLog time.Time

	for {
		select {
		case <-ctx.Done():
			logger.Printf("daemon stopping")
			for paneID, st := range tracker.Panes {
				if st.HadToken {
					if err := client.Clear(paneID); err != nil {
						logger.Printf("clear %s: %v", paneID, err)
					}
				}
			}
			if err := tracker.Save(p.state); err != nil {
				logger.Printf("save state: %v", err)
			}
			return nil

		case now := <-ticker.C:
			if inode, err := socketInode(p.socket); err == nil {
				if lastInode != 0 && inode != lastInode {
					logger.Printf("Herdr socket changed; reconciling state")
					tracker.Reinitialize()
				}
				lastInode = inode
			}

			agents, err := client.ListAgents()
			if err != nil {
				failures++
				if failures == 1 || now.Sub(lastListLog) >= 30*time.Second {
					logger.Printf("agent.list: %v", err)
					lastListLog = now
				}
				if failures >= 300 {
					logger.Printf("agent.list failed %d times; exiting", failures)
					return nil
				}
				continue
			}
			failures = 0

			decisions, stateChanged := tracker.ObserveAll(agents, now)
			if stateChanged {
				dirty = true
			}
			for _, decision := range decisions {
				var reportErr error
				if decision.Clear {
					reportErr = client.Clear(decision.PaneID)
				} else {
					reportErr = client.Report(decision.PaneID, decision.Value, decision.TTL)
				}
				if reportErr != nil {
					if now.Sub(lastReportLog) >= 5*time.Second {
						logger.Printf("report %s: %v", decision.PaneID, reportErr)
						lastReportLog = now
					}
					continue
				}
				if decision.Clear {
					if tracker.MarkCleared(decision.PaneID) {
						dirty = true
					}
				} else if tracker.MarkReported(decision.PaneID, decision.Value) {
					dirty = true
				}
			}

			if dirty || now.Sub(lastSave) >= saveInterval {
				if err := tracker.Save(p.state); err != nil {
					logger.Printf("save state: %v", err)
				} else {
					dirty = false
					lastSave = now
				}
			}
		}
	}
}
