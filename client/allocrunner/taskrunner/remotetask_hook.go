package taskrunner

import (
	"context"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

//FIXME(schmichael) move and reuse for other hooks that disable themselves?
type noopHook struct {
	name string
}

func (h noopHook) Name() string {
	return h.name
}

var _ interfaces.TaskPrestartHook = (*remoteTaskHook)(nil)
var _ interfaces.TaskPreKillHook = (*remoteTaskHook)(nil)

// remoteTaskHook reattaches to remotely executing tasks.
//
//FIXME(schmichael) super leaky abstraction with taskrunner
type remoteTaskHook struct {
	tr *TaskRunner

	logger hclog.Logger
}

func newRemoteTaskHook(tr *TaskRunner, logger hclog.Logger) interfaces.TaskHook {
	//FIXME(schmichael) determine when driverCaps can be nil, does it need a lock?
	if tr.driverCapabilities == nil || !tr.driverCapabilities.RemoteTasks {
		tr.logger.Trace("not a remote task driver; disabling hook")
		return noopHook{(*remoteTaskHook)(nil).Name()}
	}

	h := &remoteTaskHook{
		tr: tr,
	}
	h.logger = logger.Named(h.Name())
	return h
}

func (h *remoteTaskHook) Name() string {
	return "remote_task"
}

func (h *remoteTaskHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	if h.tr.getDriverHandle() != nil {
		// Driver handle already exists so don't try to load remote
		// task handle
		resp.Done = true
		return nil
	}

	h.tr.stateLock.Lock()
	th := drivers.NewTaskHandleFromState(h.tr.state)
	h.tr.stateLock.Unlock()

	// Task handle will be nil if there was no previous allocation or if
	// this is a destructive update
	if th == nil {
		resp.Done = true
		return nil
	}

	// The task config is unique per invocation so recreate it here
	th.Config = h.tr.buildTaskConfig()

	if err := h.tr.driver.RecoverTask(th); err != nil {
		//FIXME(schmichael) soft error here to let a new instance get
		//started?
		h.logger.Error("error recovering task state", "error", err)
		return nil
	}

	taskInfo, err := h.tr.driver.InspectTask(th.Config.ID)
	if err != nil {
		//FIXME(schmichael) soft error here to let a new instance get
		//started?
		h.logger.Error("error inspecting recovered task state", "error", err)
		return nil
	}

	h.logger.Trace("using remote task handle")

	h.tr.setDriverHandle(NewDriverHandle(h.tr.driver, th.Config.ID, h.tr.Task(), taskInfo.NetworkOverride))

	h.tr.stateLock.Lock()
	h.tr.localState.TaskHandle = th
	h.tr.localState.DriverNetwork = taskInfo.NetworkOverride
	h.tr.stateLock.Unlock()

	h.tr.UpdateState(structs.TaskStateRunning, structs.NewTaskEvent(structs.TaskStarted))

	resp.Done = true
	return nil
}

// PreKilling tells the remote task driver to detach a remote task instead of
// stopping it.
//
//FIXME(schmichael) this is a hacky way to signal "detach" instead of "destroy"
//and requires the driver to keep extra state
func (h *remoteTaskHook) PreKilling(ctx context.Context, req *interfaces.TaskPreKillRequest, resp *interfaces.TaskPreKillResponse) error {
	alloc := h.tr.Alloc()
	switch {
	case alloc.ClientStatus == structs.AllocClientStatusLost:
		// Continue on; lost allocs should just detach
	case alloc.DesiredTransition.ShouldMigrate():
		// Continue on; migrating allocs should just detach
	default:
		// Nothing to do exit early
		return nil
	}

	driverHandle := h.tr.getDriverHandle()
	if driverHandle == nil {
		// Nothing to do exit early
		return nil
	}

	//HACK DetachSignal indicates to the remote task driver that it should
	//detach this remote task and ignore further actions against it.
	driverHandle.SetKillSignal(drivers.DetachSignal)
	return nil
}
