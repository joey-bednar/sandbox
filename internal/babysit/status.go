package babysit

import "github.com/joey-bednar/sandbox/internal/gitlab"

// Phase is where a Spec is in its lifecycle.
type Phase string

const (
	PhaseWaiting    Phase = "waiting"            // nothing to trigger from yet
	PhaseTriggering Phase = "triggering"         // starting the trigger job
	PhaseRunning    Phase = "running"            // holding clean-up, retrying failures
	PhaseCleanup    Phase = "cleaning up"        // clean-up released, waiting for it
	PhasePushing    Phase = "pushing new commit" // repushing after a no-retry failure
	PhasePassed     Phase = "passed"
	PhaseFailed     Phase = "failed"
)

// Status is a snapshot of one Spec's progress, emitted on every poll for display.
type Status struct {
	Phase    Phase
	Main     *gitlab.Pipeline // main pipeline the trigger job lives in
	Trigger  *gitlab.Job      // the trigger job, while Phase is PhaseTriggering
	Pipeline *gitlab.Pipeline // the triggered pipeline, once it exists
	// HoldStage is the stage babysit holds back; Holding reports whether
	// its jobs are currently held (canceled on purpose) rather than released.
	HoldStage string
	Holding   bool
	Retries   map[string]int // retries so far, by job name
	Pushes    int            // new commits pushed for this Spec
	Err       error          // set when Phase is PhaseFailed
}
