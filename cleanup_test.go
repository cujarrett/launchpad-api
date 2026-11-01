package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func newCleanupEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	acts := &cleanupActivities{}
	env.RegisterActivityWithOptions(acts.DeleteGuestWorkspace, activity.RegisterOptions{Name: "DeleteGuestWorkspace"})
	env.RegisterActivityWithOptions(acts.SweepGuests, activity.RegisterOptions{Name: "SweepGuests"})
	return env
}

func TestCleanupWaitsForExpiryThenDeletes(t *testing.T) {
	env := newCleanupEnv(t)
	start := env.Now()
	var deletedAt time.Time
	env.OnActivity("DeleteGuestWorkspace", mock.Anything, "guest-happy-otter").Return(func(context.Context, string) error {
		deletedAt = env.Now()
		return nil
	}).Once()

	env.ExecuteWorkflow(SandboxCleanupWorkflow, cleanupInput{Workspace: "guest-happy-otter", ExpiresAt: start.Add(10 * time.Minute)})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if got := deletedAt.Sub(start); got < 10*time.Minute {
		t.Errorf("deleted after %s, want no earlier than the 10 minute expiry", got)
	}
}

func TestCleanupDeletesAtOnceWhenAlreadyExpired(t *testing.T) {
	env := newCleanupEnv(t)
	env.OnActivity("DeleteGuestWorkspace", mock.Anything, "guest-old-otter").Return(nil).Once()

	env.ExecuteWorkflow(SandboxCleanupWorkflow, cleanupInput{Workspace: "guest-old-otter", ExpiresAt: env.Now().Add(-time.Hour)})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.AssertExpectations(t)
}

func TestCleanupRetriesUntilTheDeleteWorks(t *testing.T) {
	env := newCleanupEnv(t)
	calls := 0
	env.OnActivity("DeleteGuestWorkspace", mock.Anything, "guest-busy-otter").Return(func(context.Context, string) error {
		calls++
		if calls < 3 {
			return errors.New("github is down")
		}
		return nil
	})

	env.ExecuteWorkflow(SandboxCleanupWorkflow, cleanupInput{Workspace: "guest-busy-otter", ExpiresAt: env.Now()})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("delete ran %d times, want 3", calls)
	}
}

func TestSweepWorkflowRunsTheSweep(t *testing.T) {
	env := newCleanupEnv(t)
	env.OnActivity("SweepGuests", mock.Anything).Return(nil).Once()
	env.ExecuteWorkflow(GuestSweepWorkflow)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.AssertExpectations(t)
}
