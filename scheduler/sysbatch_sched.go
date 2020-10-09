package scheduler

import (
	"fmt"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// maxSysBatchScheduleAttempts is used to limit the number of times
	// we will attempt to schedule if we continue to hit conflicts for
	// sysbatch jobs.
	maxSysBatchScheduleAttempts = 5
)

// SysBatchScheduler is used for 'system batch' jobs. This scheduler is intended
// for running short-lived jobs on every node. It can be described as a hybrid
// between batch and system jobs. Common use case: run commands like 'apt update'
// or 'docker gc' cluster-wide.
type SysBatchScheduler struct {
	logger  log.Logger
	state   State
	planner Planner

	eval       *structs.Evaluation
	job        *structs.Job
	plan       *structs.Plan
	planResult *structs.PlanResult
	ctx        *EvalContext
	stack      *SystemStack

	nodes     []*structs.Node
	nodesByDC map[string]int

	failedTGAllocs map[string]*structs.AllocMetric
	queuedAllocs   map[string]int
}

func NewSysBatchScheduler(logger log.Logger, state State, planner Planner) Scheduler {
	return &SysBatchScheduler{
		logger: logger.Named("sysbatch_sched"),
	}
}

func (s *SysBatchScheduler) Process(eval *structs.Evaluation) error {
	// Store the evaluation
	s.eval = eval

	// Update our logger with the eval's information
	s.logger = s.logger.With("eval_id", eval.ID, "job_id", eval.JobID, "namespace", eval.Namespace)

	// Verify the evaluation trigger reason is understood
	if err := s.verifyTriggerBy(); err != nil {
		return err
	}

	// Retry up to the maxSysBatchScheduleAttempts and reset if progress is made.
	progress := func() bool { return progressMade(s.planResult) }
	if err := retryMax(maxSysBatchScheduleAttempts, s.process, progress); err != nil {
		if statusErr, ok := err.(*SetStatusError); ok {
			return setStatus(
				s.logger,
				s.planner,
				s.eval,
				nil,                  // next eval
				nil,                  // spawned block
				nil,                  // tg metrics
				statusErr.EvalStatus, // status
				err.Error(),          // description
				nil,                  // queued allocs
				"",                   // deployment id
			)
		}
		return err
	}

	// Update the status to complete.
	return setStatus(
		s.logger,
		s.planner,
		s.eval,
		nil,                        // next eva
		nil,                        // spawned block
		nil,                        // tg metrics
		structs.EvalStatusComplete, // status
		"",                         // description
		nil,                        // queued allocs
		"",                         // deployment id
	)
}

// process is wrapped in retryMax to iteratively run the handler until we have
// no further work or we have made the maximum number of attempts.
func (s *SysBatchScheduler) process() (bool, error) {
	// Lookup the Job by ID
	var err error
	ws := memdb.NewWatchSet()
	s.job, err = s.state.JobByID(ws, s.eval.Namespace, s.eval.JobID)
	if err != nil {
		return false, fmt.Errorf("failed to get job '%s': %v", s.eval.JobID, err)
	}

	numTaskGroups := 0
	if !s.job.Stopped() {
		numTaskGroups = len(s.job.TaskGroups)
	}
	s.queuedAllocs = make(map[string]int, numTaskGroups)

	// Get the ready nodes in the required datacenters.
	// todo: probably need to remove nodes where job is complete
	if !s.job.Stopped() {
		s.nodes, s.nodesByDC, err = readyNodesInDCs(s.state, s.job.Datacenters)
		if err != nil {
			return false, fmt.Errorf("failed to get ready nodes: %v", err)
		}
	}

	// Create a plan
	s.plan = s.eval.MakePlan(s.job)

	// Reset failed allocations
	s.failedTGAllocs = nil

	// Create an evaluation context
	s.ctx = NewEvalContext(s.state, s.plan, s.logger)

	s.stack = NewSystemStack(s.ctx)
	if !s.job.Stopped() {
		s.stack.SetJob(s.job)
	}

	// Compute target job allocations
	if err := s.computeJobAllocs(); err != nil {
		s.logger.Error("failed to compute job allocations", "error", err)
		return false, err
	}

	return false, nil
}

func (s *SysBatchScheduler) verifyTriggerBy() error {
	switch s.eval.TriggeredBy {
	case structs.EvalTriggerJobRegister,
		structs.EvalTriggerJobDeregister:
		// todo: understand & add more
	default:
		description := fmt.Sprintf(
			"scheduler cannot handle '%s' evaluation reason",
			s.eval.TriggeredBy,
		)
		return setStatus(
			s.logger,
			s.planner,
			s.eval,
			nil,                      // next eval
			nil,                      // spawned blocked
			nil,                      // tg metrics
			structs.EvalStatusFailed, // status
			description,              // description
			nil,                      // queued allocs
			"",                       // deployment id
		)
	}
	return nil
}

// computeJobAllocs is used to reconcile differences between the job,
// existing allocations and node status to update the allocations.
func (s *SysBatchScheduler) computeJobAllocs() error {
	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	allocs, err := s.state.AllocsByJob(ws, s.eval.Namespace, s.eval.JobID, true)
	if err != nil {
		return fmt.Errorf("failed to get allocs for job '%s': %v",
			s.eval.JobID, err)
	}

	// Determine the tainted nodes containing job allocs
	tainted, err := taintedNodes(s.state, allocs)
	if err != nil {
		return fmt.Errorf("failed to get tainted nodes for job '%s': %v",
			s.eval.JobID, err)
	}

	// Update the allocations which are in pending/running state on tainted
	// nodes to lost.
	updateNonTerminalAllocsToLost(s.plan, tainted, allocs)

	// ooh big split now

	// YOU ARE HERE
	return nil
}
