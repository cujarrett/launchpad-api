package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// Guest sandboxes are cleaned up by Temporal, on the platform's own Workflow offering.
//
// Per sandbox: SandboxCleanupWorkflow sleeps until the sandbox expires, then deletes it.
// The timer is durable, so a launchpad-api restart neither loses nor delays it.
// Every minute: a Temporal schedule runs GuestSweepWorkflow. It gives any sandbox
// without a cleanup workflow one, and removes leftovers whose guest.yaml is gone.
const (
	cleanupQueue    = "launchpad-cleanup"
	sweepScheduleID = "launchpad-guest-sweep"
	sweepInterval   = time.Minute
)

type cleanupInput struct {
	Workspace string
	ExpiresAt time.Time
}

func cleanupWorkflowID(workspace string) string { return "cleanup-" + workspace }

// SandboxCleanupWorkflow waits for one sandbox to expire, then deletes its files.
// ArgoCD and Crossplane remove everything the files created.
func SandboxCleanupWorkflow(ctx workflow.Context, in cleanupInput) error {
	if wait := in.ExpiresAt.Sub(workflow.Now(ctx)); wait > 0 {
		if err := workflow.Sleep(ctx, wait); err != nil {
			return err
		}
	}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		// GitHub outages end. Keep retrying until the sandbox is gone.
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: time.Minute},
	})
	return workflow.ExecuteActivity(ctx, "DeleteGuestWorkspace", in.Workspace).Get(ctx, nil)
}

// GuestSweepWorkflow is the safety net the schedule runs.
func GuestSweepWorkflow(ctx workflow.Context) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	return workflow.ExecuteActivity(ctx, "SweepGuests").Get(ctx, nil)
}

type cleanupActivities struct {
	app      *app
	temporal client.Client
}

func (c *cleanupActivities) DeleteGuestWorkspace(ctx context.Context, workspace string) error {
	return c.app.deleteGuestWorkspaceFiles(ctx, workspace)
}

// SweepGuests gives every guest workspace a cleanup workflow, and removes leftovers.
func (c *cleanupActivities) SweepGuests(ctx context.Context) error {
	entries, err := c.app.gh.listDir(ctx, "")
	if err != nil {
		return err
	}
	var failed []string
	for _, e := range entries {
		if e.Type != "dir" || !strings.HasPrefix(e.Name, guestPrefix) {
			continue
		}
		activity.RecordHeartbeat(ctx, e.Name)

		expiry := c.app.fetchGuestExpiry(ctx, e.Name)
		if expiry == nil {
			// Usually a workspace mid-creation. If guest.yaml stays missing across several
			// sweeps, it is a leftover from a delete that failed partway through.
			if misses := c.app.recordStaleGuestMiss(e.Name); misses >= staleGuestSweepThreshold {
				slog.Warn("guest sweep: guest.yaml missing after repeated checks, removing leftover files", "workspace", e.Name, "misses", misses)
				if err := c.app.deleteGuestWorkspaceFiles(ctx, e.Name); err != nil {
					failed = append(failed, e.Name)
					continue
				}
				c.app.clearStaleGuestMisses(e.Name)
			}
			continue
		}
		c.app.clearStaleGuestMisses(e.Name)
		if err := startCleanup(ctx, c.temporal, e.Name, *expiry); err != nil {
			slog.Error("guest sweep: start cleanup", "workspace", e.Name, "err", err)
			failed = append(failed, e.Name)
		}
	}
	if len(failed) > 0 {
		return errors.New("guest sweep: could not handle " + strings.Join(failed, ", "))
	}
	return nil
}

// startCleanup starts a sandbox's cleanup workflow. If one is already running, it is kept.
// A finished one does not block a new run, because a sandbox name can be reused later.
func startCleanup(ctx context.Context, tc client.Client, workspace string, expiresAt time.Time) error {
	_, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       cleanupWorkflowID(workspace),
		TaskQueue:                cleanupQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, SandboxCleanupWorkflow, cleanupInput{Workspace: workspace, ExpiresAt: expiresAt})
	return err
}

// startCleanupWorker connects to Temporal, runs the cleanup worker, and makes sure the sweep
// schedule exists. It returns a nil client when Temporal is not configured, as in local dev.
func (a *app) startCleanupWorker(ctx context.Context) (client.Client, error) {
	address := envOrDefault("TEMPORAL_ADDRESS", "")
	if address == "" {
		slog.Warn("TEMPORAL_ADDRESS not set, guest cleanup is off")
		return nil, nil
	}
	headers, err := newTemporalHeaders(envOrDefault("TEMPORAL_TOKEN_SCOPE", ""))
	if err != nil {
		return nil, err
	}
	// Lazy, so launchpad-api still starts while Temporal is briefly down.
	tc, err := client.NewLazyClient(client.Options{
		HostPort:        address,
		Namespace:       envOrDefault("TEMPORAL_NAMESPACE", "default"),
		HeadersProvider: headers,
		Logger:          slog.Default(),
	})
	if err != nil {
		return nil, err
	}

	acts := &cleanupActivities{app: a, temporal: tc}
	w := worker.New(tc, cleanupQueue, worker.Options{})
	w.RegisterWorkflow(SandboxCleanupWorkflow)
	w.RegisterWorkflow(GuestSweepWorkflow)
	w.RegisterActivityWithOptions(acts.DeleteGuestWorkspace, activity.RegisterOptions{Name: "DeleteGuestWorkspace"})
	w.RegisterActivityWithOptions(acts.SweepGuests, activity.RegisterOptions{Name: "SweepGuests"})
	if err := w.Start(); err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		w.Stop()
		tc.Close()
	}()

	go ensureSweepSchedule(ctx, tc)
	return tc, nil
}

// ensureSweepSchedule creates the sweep schedule, retrying until Temporal answers.
func ensureSweepSchedule(ctx context.Context, tc client.Client) {
	for {
		_, err := tc.ScheduleClient().Create(ctx, client.ScheduleOptions{
			ID:   sweepScheduleID,
			Spec: client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: sweepInterval}}},
			Action: &client.ScheduleWorkflowAction{
				Workflow:  GuestSweepWorkflow,
				TaskQueue: cleanupQueue,
			},
			// Sweep once right away, to cover sandboxes from before this schedule existed.
			TriggerImmediately: true,
		})
		if err == nil || errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
			return
		}
		slog.Warn("guest sweep schedule not created yet, retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// temporalHeaders adds an Entra access token to every Temporal call. The platform's sidecar
// keeps the federated token file fresh, and the Azure SDK trades and caches the token.
type temporalHeaders struct {
	cred  azcore.TokenCredential
	scope string
}

// headersProvider matches the Temporal SDK's unexported HeadersProvider interface.
type headersProvider interface {
	GetHeaders(ctx context.Context) (map[string]string, error)
}

// newTemporalHeaders returns nil with no scope, so a local dev server needs no token.
func newTemporalHeaders(scope string) (headersProvider, error) {
	if scope == "" {
		return nil, nil
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(nil) // reads the AZURE_* env vars
	if err != nil {
		return nil, err
	}
	return temporalHeaders{cred: cred, scope: scope}, nil
}

func (t temporalHeaders) GetHeaders(ctx context.Context) (map[string]string, error) {
	tok, err := t.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{t.scope}})
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + tok.Token}, nil
}
