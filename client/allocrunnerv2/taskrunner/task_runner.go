package taskrunner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/boltdb/bolt"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/taskrunner/restarts"
	"github.com/hashicorp/nomad/client/allocrunnerv2/interfaces"
	"github.com/hashicorp/nomad/client/allocrunnerv2/taskrunner/state"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver"
	"github.com/hashicorp/nomad/client/driver/env"
	oldstate "github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/ugorji/go/codec"
	"golang.org/x/crypto/blake2b"
)

const (
	// killBackoffBaseline is the baseline time for exponential backoff while
	// killing a task.
	killBackoffBaseline = 5 * time.Second

	// killBackoffLimit is the limit of the exponential backoff for killing
	// the task.
	killBackoffLimit = 2 * time.Minute

	// killFailureLimit is how many times we will attempt to kill a task before
	// giving up and potentially leaking resources.
	killFailureLimit = 5
)

var (
	// taskRunnerStateAllKey holds all the task runners state. At the moment
	// there is no need to split it
	taskRunnerStateAllKey = []byte("simple-all")
)

type TaskRunner struct {
	// allocID and taskName are immutable so store a copy to access without
	// locks
	allocID  string
	taskName string

	alloc     *structs.Allocation
	allocLock sync.Mutex

	clientConfig *config.Config

	// state captures the state of the task for updating the allocation
	state *structs.TaskState

	// localState captures the node-local state of the task for when the
	// Nomad agent restarts
	localState     *state.LocalState
	localStateLock sync.RWMutex

	// stateDB is for persisting localState
	stateDB *bolt.DB

	// persistedHash is the hash of the last persisted state for skipping
	// unnecessary writes
	persistedHash []byte

	// ctx is the task runner's context and is done whe the task runner
	// should exit. Shutdown hooks are run.
	ctx context.Context

	// ctxCancel is used to exit the task runner's Run loop without
	// stopping the task. Shutdown hooks are run.
	ctxCancel context.CancelFunc

	// Logger is the logger for the task runner.
	logger log.Logger

	// updateCh receives Alloc updates
	updateCh chan *structs.Allocation

	// waitCh is closed when the task runner has transitioned to a terminal
	// state
	waitCh chan struct{}

	// driver is the driver for the task.
	driver driver.Driver

	// handle is the handle to the currently running driver
	handle     driver.DriverHandle
	handleLock sync.Mutex

	// task is the task being run
	task     *structs.Task
	taskLock sync.RWMutex

	// taskDir is the directory structure for this task.
	taskDir *allocdir.TaskDir

	// envBuilder is used to build the task's environment
	envBuilder *env.Builder

	// restartTracker is used to decide if the task should be restarted.
	restartTracker *restarts.RestartTracker

	// runnerHooks are task runner lifecycle hooks that should be run on state
	// transistions.
	runnerHooks []interfaces.TaskHook

	// vaultClient is the client to use to derive and renew Vault tokens
	vaultClient vaultclient.VaultClient

	// vaultToken is the current Vault token. It should be accessed with the
	// getter.
	vaultToken     string
	vaultTokenLock sync.Mutex

	// baseLabels are used when emitting tagged metrics. All task runner metrics
	// will have these tags, and optionally more.
	baseLabels []metrics.Label
}

type Config struct {
	Alloc        *structs.Allocation
	ClientConfig *config.Config
	Task         *structs.Task
	TaskDir      *allocdir.TaskDir
	Logger       log.Logger

	// VaultClient is the client to use to derive and renew Vault tokens
	VaultClient vaultclient.VaultClient

	// LocalState is optionally restored task state
	LocalState *state.LocalState

	// StateDB is used to store and restore state.
	StateDB *bolt.DB
}

func NewTaskRunner(config *Config) (*TaskRunner, error) {
	// Create a context for the runner
	trCtx, trCancel := context.WithCancel(context.Background())

	// Initialize the environment builder
	envBuilder := env.NewBuilder(
		config.ClientConfig.Node,
		config.Alloc,
		config.Task,
		config.ClientConfig.Region,
	)

	tr := &TaskRunner{
		alloc:        config.Alloc,
		allocID:      config.Alloc.ID,
		clientConfig: config.ClientConfig,
		task:         config.Task,
		taskDir:      config.TaskDir,
		taskName:     config.Task.Name,
		envBuilder:   envBuilder,
		vaultClient:  config.VaultClient,
		//XXX Make a Copy to avoid races?
		state:      config.Alloc.TaskStates[config.Task.Name],
		localState: config.LocalState,
		stateDB:    config.StateDB,
		ctx:        trCtx,
		ctxCancel:  trCancel,
		updateCh:   make(chan *structs.Allocation),
		waitCh:     make(chan struct{}),
	}

	// Create the logger based on the allocation ID
	tr.logger = config.Logger.Named("task_runner").With("task", config.Task.Name)

	// Build the restart tracker.
	tg := tr.alloc.Job.LookupTaskGroup(tr.alloc.TaskGroup)
	if tg == nil {
		tr.logger.Error("alloc missing task group")
		return nil, fmt.Errorf("alloc missing task group")
	}
	tr.restartTracker = restarts.NewRestartTracker(tg.RestartPolicy, tr.alloc.Job.Type)

	// Initialize the task state
	tr.initState()

	// Get the driver
	if err := tr.initDriver(); err != nil {
		tr.logger.Error("failed to create driver", "error", err)
		return nil, err
	}

	// Initialize the runners hooks.
	tr.initHooks()

	// Initialize base labels
	tr.initLabels()

	return tr, nil
}

func (tr *TaskRunner) initState() {
	if tr.state == nil {
		tr.state = &structs.TaskState{
			State: structs.TaskStatePending,
		}
	}
	if tr.localState == nil {
		tr.localState = state.NewLocalState()
	}
}

func (tr *TaskRunner) initLabels() {
	alloc := tr.Alloc()
	tr.baseLabels = []metrics.Label{
		{
			Name:  "job",
			Value: alloc.Job.Name,
		},
		{
			Name:  "task_group",
			Value: alloc.TaskGroup,
		},
		{
			Name:  "alloc_id",
			Value: tr.allocID,
		},
		{
			Name:  "task",
			Value: tr.taskName,
		},
	}
}

func (tr *TaskRunner) Run() {
	defer close(tr.waitCh)
	var handle driver.DriverHandle

MAIN:
	for tr.ctx.Err() == nil {
		// Run the prestart hooks
		if err := tr.prestart(); err != nil {
			tr.logger.Error("prestart failed", "error", err)
			tr.restartTracker.SetStartError(err)
			goto RESTART
		}

		if tr.ctx.Err() != nil {
			break MAIN
		}

		// Run the task
		if err := tr.runDriver(); err != nil {
			tr.logger.Error("running driver failed", "error", err)
			tr.restartTracker.SetStartError(err)
			goto RESTART
		}

		// Run the poststart hooks
		if err := tr.poststart(); err != nil {
			tr.logger.Error("poststart failed", "error", err)
		}

		// Grab the handle
		handle = tr.getDriverHandle()

		select {
		case waitRes := <-handle.WaitCh():
			// Clear the handle
			tr.setDriverHandle(nil)

			// Store the wait result on the restart tracker
			tr.restartTracker.SetWaitResult(waitRes)
		case <-tr.ctx.Done():
			tr.logger.Debug("task killed")
		}

		// TODO Need to run exited hooks

	RESTART:
		// Actually restart by sleeping and also watching for destroy events
		restart, restartWait := tr.shouldRestart()
		if !restart {
			break MAIN
		}

		deadline := time.Now().Add(restartWait)
		timer := time.NewTimer(restartWait)
		for time.Now().Before(deadline) {
			select {
			case <-timer.C:
			case <-tr.ctx.Done():
				tr.logger.Debug("task runner cancelled")
				break MAIN
			}
		}
		timer.Stop()
	}

	// Run the stop hooks
	if err := tr.stop(); err != nil {
		tr.logger.Error("stop failed", "error", err)
	}

	tr.logger.Debug("task run loop exiting")
}

func (tr *TaskRunner) shouldRestart() (bool, time.Duration) {
	// Determine if we should restart
	state, when := tr.restartTracker.GetState()
	reason := tr.restartTracker.GetReason()
	switch state {
	case structs.TaskKilled:
		// The task was killed. Nothing to do
		return false, 0
	case structs.TaskNotRestarting, structs.TaskTerminated:
		tr.logger.Info("not restarting task", "reason", reason)
		if state == structs.TaskNotRestarting {
			tr.SetState(structs.TaskStateDead, structs.NewTaskEvent(structs.TaskNotRestarting).SetRestartReason(reason).SetFailsTask())
		}
		return false, 0
	case structs.TaskRestarting:
		tr.logger.Info("restarting task", "reason", reason, "delay", when)
		tr.SetState(structs.TaskStatePending, structs.NewTaskEvent(structs.TaskRestarting).SetRestartDelay(when).SetRestartReason(reason))
		return true, 0
	default:
		tr.logger.Error("restart tracker returned unknown state", "state", state)
		return true, when
	}
}

// runDriver runs the driver and waits for it to exit
func (tr *TaskRunner) runDriver() error {
	// Run prestart
	ctx := driver.NewExecContext(tr.taskDir, tr.envBuilder.Build())
	_, err := tr.driver.Prestart(ctx, tr.task)
	if err != nil {
		tr.logger.Error("driver pre-start failed", "error", err)
		return err
	}

	// Create a new context for Start since the environment may have been updated.
	ctx = driver.NewExecContext(tr.taskDir, tr.envBuilder.Build())

	// Start the job
	sresp, err := tr.driver.Start(ctx, tr.task)
	if err != nil {
		tr.logger.Warn("driver start failed", "error", err)
		return err
	}

	// Grab the handle
	tr.setDriverHandle(sresp.Handle)

	//XXX need to capture the driver network

	// Emit an event that we started
	tr.SetState(structs.TaskStateRunning, structs.NewTaskEvent(structs.TaskStarted))
	return nil
}

// initDriver creates the driver for the task
func (tr *TaskRunner) initDriver() error {
	// Create a task-specific event emitter callback to expose minimal
	// state to drivers
	eventEmitter := func(m string, args ...interface{}) {
		msg := fmt.Sprintf(m, args...)
		tr.logger.Debug("driver event", "event", msg)
		tr.SetState("", structs.NewTaskEvent(structs.TaskDriverMessage).SetDriverMessage(msg))
	}

	alloc := tr.Alloc()
	driverCtx := driver.NewDriverContext(
		alloc.Job.Name,
		alloc.TaskGroup,
		tr.taskName,
		tr.allocID,
		tr.clientConfig,               // XXX Why does it need this
		tr.clientConfig.Node,          // XXX THIS I NEED TO FIX
		tr.logger.StandardLogger(nil), // XXX Should pass this through
		eventEmitter)

	driver, err := driver.NewDriver(tr.task.Driver, driverCtx)
	if err != nil {
		return err
	}

	tr.driver = driver
	return nil
}

// handleDestroy kills the task handle. In the case that killing fails,
// handleDestroy will retry with an exponential backoff and will give up at a
// given limit. It returns whether the task was destroyed and the error
// associated with the last kill attempt.
func (tr *TaskRunner) handleDestroy(handle driver.DriverHandle) (destroyed bool, err error) {
	// Cap the number of times we attempt to kill the task.
	for i := 0; i < killFailureLimit; i++ {
		if err = handle.Kill(); err != nil {
			// Calculate the new backoff
			backoff := (1 << (2 * uint64(i))) * killBackoffBaseline
			if backoff > killBackoffLimit {
				backoff = killBackoffLimit
			}

			tr.logger.Error("failed to kill task", "backoff", backoff, "error", err)
			time.Sleep(backoff)
		} else {
			// Kill was successful
			return true, nil
		}
	}
	return
}

// persistLocalState persists local state to disk synchronously.
func (tr *TaskRunner) persistLocalState() error {
	// buffer for writing to boltdb
	var buf bytes.Buffer

	// Hash for skipping unnecessary writes
	h, err := blake2b.New(32, nil)
	if err != nil {
		// Programming error that should never happen!
		return err
	}

	// Multiplex writes to both
	w := io.MultiWriter(h, &buf)

	// Encode as msgpack value
	tr.localStateLock.Lock()
	err = codec.NewEncoder(w, structs.MsgpackHandle).Encode(&tr.localState)
	tr.localStateLock.Unlock()
	if err != nil {
		return fmt.Errorf("failed to serialize snapshot: %v", err)
	}

	// If the hashes are equal, skip the write
	hashVal := h.Sum(nil)
	if bytes.Equal(hashVal, tr.persistedHash) {
		return nil
	}

	// Start the transaction.
	return tr.stateDB.Batch(func(tx *bolt.Tx) error {
		// Grab the task bucket
		//XXX move into new state pkg
		taskBkt, err := oldstate.GetTaskBucket(tx, tr.allocID, tr.taskName)
		if err != nil {
			return fmt.Errorf("failed to retrieve allocation bucket: %v", err)
		}

		if err := oldstate.PutData(taskBkt, taskRunnerStateAllKey, buf.Bytes()); err != nil {
			return fmt.Errorf("failed to write task_runner state: %v", err)
		}

		// Store the hash that was persisted
		tx.OnCommit(func() {
			tr.persistedHash = hashVal
		})

		return nil
	})
}

// SetState sets the task runners allocation state.
func (tr *TaskRunner) SetState(state string, event *structs.TaskEvent) {
	// Ensure the event is populated with human readable strings
	event.PopulateEventDisplayMessage()

	task := tr.state

	// Update the state of the task
	if state != "" {
		task.State = state
	}

	// Handle the event
	if event == nil {
		if event.FailsTask {
			task.Failed = true
		}

		if event.Type == structs.TaskRestarting {
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "restart"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "restart"}, 1)
			//}
			task.Restarts++
			task.LastRestart = time.Unix(0, event.Time)
		}
		appendTaskEvent(task, event)
	}

	// Handle the state transistion.
	switch state {
	case structs.TaskStateRunning:
		// Capture the start time if it is just starting
		if task.State != structs.TaskStateRunning {
			task.StartedAt = time.Now().UTC()
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "running"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "running"}, 1)
			//}
		}
	case structs.TaskStateDead:
		// Capture the finished time if not already set
		if task.FinishedAt.IsZero() {
			task.FinishedAt = time.Now().UTC()
		}

		// Emitting metrics to indicate task complete and failures
		if task.Failed {
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "failed"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "failed"}, 1)
			//}
		} else {
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "complete"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "complete"}, 1)
			//}
		}
	}

	// Create a copy and notify the alloc runner of the transition
	//FIXME
	//if err := tr.allocRunner.StateUpdated(tr.state.Copy()); err != nil {
	//tr.logger.Error("failed to save state", "error", err)
	//}
}

func (tr *TaskRunner) EmitEvent(event *structs.TaskEvent) {
	tr.SetState("", event)
}

// WaitCh is closed when TaskRunner.Run exits.
func (tr *TaskRunner) WaitCh() <-chan struct{} {
	return tr.waitCh
}

// Update the running allocation with a new version received from the server.
//
// This method is safe for calling concurrently with Run() and does not modify
// the passed in allocation.
func (tr *TaskRunner) Update(update *structs.Allocation) {
	select {
	case tr.updateCh <- update:
	case <-tr.WaitCh():
		//XXX Do we log here like we used to? If we're just
		//shutting down it's not an error to drop the update as
		//it will be applied on startup
	}
}

// appendTaskEvent updates the task status by appending the new event.
func appendTaskEvent(state *structs.TaskState, event *structs.TaskEvent) {
	capacity := 10
	if state.Events == nil {
		state.Events = make([]*structs.TaskEvent, 0, capacity)
	}

	// If we hit capacity, then shift it.
	if len(state.Events) == capacity {
		old := state.Events
		state.Events = make([]*structs.TaskEvent, 0, capacity)
		state.Events = append(state.Events, old[1:]...)
	}

	state.Events = append(state.Events, event)
}
