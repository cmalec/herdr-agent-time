package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	saveInterval       = 10 * time.Second
	saveRetryInterval  = 30 * time.Second
	saveLogInterval    = 30 * time.Second
	listLogInterval    = 30 * time.Second
	reportLogInterval  = 5 * time.Second
	pluginLogInterval  = 30 * time.Second
	pluginCheckPeriod  = 30 * time.Second
	pluginMissLimit    = 2
	listFailureLimit   = 300
	shutdownClearLimit = 5 * time.Second
)

// herdrClient is the subset of Client the daemon uses. Tests replace it.
type herdrClient interface {
	ListAgents(context.Context) ([]AgentInfo, error)
	ListPlugins(context.Context) ([]pluginInfo, error)
	Report(context.Context, string, string, time.Duration) error
	Clear(context.Context, string) error
	Close() error
}

// daemonExit describes why the loop stopped and what shutdown work to do.
type daemonExit struct {
	reason      string
	clear       bool
	removeState bool
}

type daemon struct {
	paths       paths
	tracker     *Tracker
	client      herdrClient
	logger      *log.Logger
	now         func() time.Time
	inode       func(string) (uint64, error)
	save        func(string) error
	pluginRoot  string
	watchPlugin bool

	saveInterval      time.Duration
	saveRetryInterval time.Duration
	pluginCheckPeriod time.Duration
	pluginMissLimit   int
	listFailureLimit  int

	lastSave        time.Time
	nextSave        time.Time
	lastSaveLog     time.Time
	lastListLog     time.Time
	lastReportLog   time.Time
	lastPluginLog   time.Time
	lastPluginCheck time.Time
	lastInode       uint64
	dirty           bool
	listFailed      bool
	failures        int
	pluginMisses    int
}

func newDaemon(p paths, tracker *Tracker, client herdrClient, logger *log.Logger) *daemon {
	pluginRoot := ""
	if value := os.Getenv("HERDR_PLUGIN_ROOT"); value != "" {
		pluginRoot = filepath.Clean(value)
	}
	d := &daemon{
		paths:             p,
		tracker:           tracker,
		client:            client,
		logger:            logger,
		now:               time.Now,
		inode:             socketInode,
		save:              tracker.Save,
		pluginRoot:        pluginRoot,
		watchPlugin:       os.Getenv("HERDR_PLUGIN_ID") == pluginID || pluginRoot != "",
		saveInterval:      saveInterval,
		saveRetryInterval: saveRetryInterval,
		pluginCheckPeriod: pluginCheckPeriod,
		pluginMissLimit:   pluginMissLimit,
		listFailureLimit:  listFailureLimit,
	}
	d.lastSave = d.now()
	if d.inode != nil {
		d.lastInode, _ = d.inode(p.socket)
	}
	return d
}

// run processes ticks until ctx is canceled or the loop asks to exit.
func (d *daemon) run(ctx context.Context, ticks <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			d.logger.Printf("daemon stopping")
			return d.finish(ctx, daemonExit{clear: true})
		case _, ok := <-ticks:
			if !ok {
				d.logger.Printf("daemon stopping")
				return d.finish(ctx, daemonExit{clear: true})
			}
			now := d.now()
			if exit := d.tick(ctx, now); exit != nil {
				return d.finish(ctx, *exit)
			}
		}
	}
}

// tick performs one poll, metadata update, and persistence pass. A non-nil
// result stops the loop.
func (d *daemon) tick(ctx context.Context, now time.Time) *daemonExit {
	if d.inode != nil {
		if inode, err := d.inode(d.paths.socket); err == nil {
			if d.lastInode != 0 && inode != d.lastInode {
				d.logger.Printf("Herdr socket changed; reconnecting")
				_ = d.client.Close()
				d.tracker.Reinitialize()
			}
			d.lastInode = inode
		}
	}

	if d.watchPlugin && (d.pluginCheckPeriod <= 0 || d.lastPluginCheck.IsZero() ||
		now.Sub(d.lastPluginCheck) >= d.pluginCheckPeriod) {
		d.lastPluginCheck = now
		if exit := d.checkPlugin(ctx, now); exit != nil {
			return exit
		}
	}

	agents, err := d.client.ListAgents(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		d.listFailed = true
		d.failures++
		if d.failures == 1 || now.Sub(d.lastListLog) >= listLogInterval {
			d.logger.Printf("agent.list: %v", err)
			d.lastListLog = now
		}
		if d.failures >= d.listFailureLimit {
			return &daemonExit{
				reason: fmt.Sprintf("agent.list failed %d times; exiting", d.failures),
			}
		}
		return nil
	}
	d.failures = 0
	if d.listFailed {
		d.tracker.Reinitialize()
		d.listFailed = false
	}

	decisions, stateChanged := d.tracker.ObserveAll(agents, now)
	if stateChanged {
		d.dirty = true
	}
	for _, decision := range decisions {
		var reportErr error
		if decision.Clear {
			reportErr = d.client.Clear(ctx, decision.PaneID)
		} else {
			reportErr = d.client.Report(ctx, decision.PaneID, decision.Value, decision.TTL)
		}
		if reportErr != nil {
			if now.Sub(d.lastReportLog) >= reportLogInterval {
				d.logger.Printf("report %s: %v", decision.PaneID, reportErr)
				d.lastReportLog = now
			}
			continue
		}
		if decision.Clear {
			if d.tracker.MarkCleared(decision.PaneID) {
				d.dirty = true
			}
		} else if d.tracker.MarkReported(decision.PaneID, decision.Value) {
			d.dirty = true
		}
	}

	d.saveIfDue(now)
	return nil
}

func (d *daemon) checkPlugin(ctx context.Context, now time.Time) *daemonExit {
	plugins, err := d.client.ListPlugins(ctx)
	if err != nil {
		if ctx.Err() == nil && (d.lastPluginLog.IsZero() || now.Sub(d.lastPluginLog) >= pluginLogInterval) {
			d.logger.Printf("plugin.list: %v", err)
			d.lastPluginLog = now
		}
		return nil
	}
	var found *pluginInfo
	for index := range plugins {
		if plugins[index].PluginID == pluginID {
			found = &plugins[index]
			break
		}
	}
	if found == nil {
		d.pluginMisses++
		if d.pluginMisses >= d.pluginMissLimit {
			return &daemonExit{
				reason:      "plugin is no longer installed; exiting",
				clear:       true,
				removeState: true,
			}
		}
		return nil
	}
	d.pluginMisses = 0
	if !found.Enabled {
		return &daemonExit{
			reason:      "plugin is disabled; exiting",
			clear:       true,
			removeState: true,
		}
	}
	if d.pluginRoot != "" && found.PluginRoot != "" && !samePath(d.pluginRoot, found.PluginRoot) {
		return &daemonExit{
			reason: "plugin installation changed; exiting for restart",
		}
	}
	return nil
}

func (d *daemon) saveIfDue(now time.Time) {
	periodic := d.tracker.hasRunning() && now.Sub(d.lastSave) >= d.saveInterval
	if !d.dirty && !periodic {
		return
	}
	if !d.nextSave.IsZero() && now.Before(d.nextSave) {
		return
	}
	if err := d.save(d.paths.state); err != nil {
		d.nextSave = now.Add(d.saveRetryInterval)
		if d.lastSaveLog.IsZero() || now.Sub(d.lastSaveLog) >= saveLogInterval {
			d.logger.Printf("save state: %v", err)
			d.lastSaveLog = now
		}
		return
	}
	d.dirty = false
	d.lastSave = now
	d.nextSave = time.Time{}
}

func (d *daemon) finish(ctx context.Context, exit daemonExit) error {
	if exit.reason != "" {
		d.logger.Printf("%s", exit.reason)
	}
	if exit.clear {
		d.clearTokens(ctx)
	}
	if exit.removeState {
		if err := removeFile(d.paths.state); err != nil {
			d.logger.Printf("remove state: %v", err)
		}
		return nil
	}
	if err := d.save(d.paths.state); err != nil {
		d.logger.Printf("save state: %v", err)
	}
	return nil
}

func (d *daemon) clearTokens(ctx context.Context) {
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownClearLimit)
	defer cancel()
	for paneID, state := range d.tracker.Panes {
		if !state.HadToken {
			continue
		}
		if err := d.client.Clear(clearCtx, paneID); err != nil {
			d.logger.Printf("clear %s: %v", paneID, err)
		}
	}
}
