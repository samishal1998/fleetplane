package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func acquireCmd(r *root) *cobra.Command {
	var class, ttl, maxWait, idemKey string
	var cpu, memoryMiB int64
	var exclusive bool
	cmd := &cobra.Command{
		Use:   "acquire",
		Short: "Acquire capacity: reuse an existing machine or create one (08 §3)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := apiclient.AcquireRequest{Class: class, Exclusive: exclusive}
			constraints := map[string]map[string]int64{}
			if cpu > 0 {
				constraints["cpu"] = map[string]int64{"min": cpu}
			}
			if memoryMiB > 0 {
				constraints["memoryMiB"] = map[string]int64{"min": memoryMiB}
			}
			if len(constraints) > 0 {
				b, _ := json.Marshal(constraints)
				req.Constraints = b
			}
			if ttl != "" {
				req.Lease = &apiclient.LeaseRequest{TTL: ttl}
			}
			req.MaxWait = maxWait
			acq, err := r.client().Acquire(cmd.Context(), req, idemKey)
			if err != nil {
				return err
			}
			// The accept body reflects the journaled state; show current.
			if cur, err := r.client().GetAcquisition(cmd.Context(), acq.ID); err == nil {
				acq = cur
			}
			if r.output == "json" {
				return printJSON(cmd, acq)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s\nstate: %s\n", acq.ID, orDash(acq.ResourceID), acq.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&class, "class", "", "resource class (needed to create new capacity)")
	cmd.Flags().Int64Var(&cpu, "cpu", 0, "minimum CPUs")
	cmd.Flags().Int64Var(&memoryMiB, "memory-mib", 0, "minimum memory in MiB")
	cmd.Flags().BoolVar(&exclusive, "exclusive", false, "whole-machine lease (implied when no constraints given)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "lease TTL, e.g. 90m")
	cmd.Flags().StringVar(&maxWait, "max-wait", "", "queue budget, e.g. 10m: wait for existing capacity before scaling up (needs server >= v0.4)")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "idempotency key")
	return cmd
}

func releaseCmd(r *root) *cobra.Command {
	return &cobra.Command{
		Use:   "release ACQ_ID",
		Short: "Release an acquisition (retry-safe)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().Release(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: released\n", args[0])
			return nil
		},
	}
}

func watchCmd(r *root) *cobra.Command {
	var interval, timeout time.Duration
	cmd := &cobra.Command{
		Use:   "watch ID",
		Short: "Poll an acquisition (acq_…) or operation (op_…) until terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			last := ""
			for {
				state, done, ok, err := watchOnce(ctx, r.client(), id)
				if err != nil {
					return err
				}
				if state != last {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", id, state)
					last = state
				}
				if done {
					if !ok {
						return &watchFailed{id: id, state: state}
					}
					return nil
				}
				select {
				case <-ctx.Done():
					return &watchFailed{id: id, state: "timeout waiting (last: " + last + ")"}
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "give up after")
	return cmd
}

// watchFailed maps to exit code 6 (ADR-006).
type watchFailed struct{ id, state string }

func (w *watchFailed) Error() string { return w.id + ": " + w.state }

func watchOnce(ctx context.Context, c *apiclient.Client, id string) (state string, done, ok bool, err error) {
	switch {
	case strings.HasPrefix(id, "acq_"):
		acq, err := c.GetAcquisition(ctx, id)
		if err != nil {
			return "", false, false, err
		}
		switch acq.State {
		case "bound":
			return fmt.Sprintf("bound -> %s (lease %s)", acq.ResourceID, acq.LeaseID), true, true, nil
		case "failed", "expired", "released":
			return acq.State, true, false, nil
		default:
			return acq.State, false, false, nil
		}
	case strings.HasPrefix(id, "op_"):
		op, err := c.GetOperation(ctx, id)
		if err != nil {
			return "", false, false, err
		}
		switch op.State {
		case "succeeded":
			return op.State, true, true, nil
		case "failed", "aborted":
			return op.State, true, false, nil
		default:
			return op.State, false, false, nil
		}
	default:
		return "", false, false, fmt.Errorf("watch supports acq_… and op_… ids, got %q", id)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
