package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/samimishal/fleetplane/internal/api"
	"github.com/samimishal/fleetplane/pkg/apiclient"
)

func poolsCmd(r *root) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pools",
		Short: "List and manage pools",
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := r.client().ListPools(cmd.Context())
			if err != nil {
				return err
			}
			if r.output == "json" {
				return printJSON(cmd, list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSPEC")
			for _, p := range list.Items {
				fmt.Fprintf(w, "%s\t%s\t%s\n", p.Metadata.ID, p.Metadata.Name, compactJSON(p.Spec))
			}
			return w.Flush()
		},
	}
	var file string
	apply := &cobra.Command{
		Use:   "apply -f FILE",
		Short: "Create or update a pool from a JSON manifest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var m apiclient.PoolManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("manifest: %w", err)
			}
			pool, err := r.client().ApplyPool(cmd.Context(), m)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s): applied\n", pool.Metadata.Name, pool.Metadata.ID)
			return nil
		},
	}
	apply.Flags().StringVarP(&file, "file", "f", "", "manifest file (required)")
	_ = apply.MarkFlagRequired("file")

	reconcile := &cobra.Command{
		Use:   "reconcile ID",
		Short: "Trigger reconciliation for one pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().ReconcilePool(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: reconciling\n", args[0])
			return nil
		},
	}
	get := &cobra.Command{
		Use:   "get ID",
		Short: "Show one pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pool, err := r.client().GetPool(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, pool)
		},
	}
	cmd.AddCommand(apply, reconcile, get)
	return cmd
}

func operationsCmd(r *root) *cobra.Command {
	return &cobra.Command{
		Use:   "operations [ID]",
		Short: "List open operations, or show one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				op, err := r.client().GetOperation(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return printJSON(cmd, op)
			}
			ops, err := r.client().ListOperations(cmd.Context())
			if err != nil {
				return err
			}
			if r.output == "json" {
				return printJSON(cmd, ops)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tSTATE\tRESOURCE\tATTEMPT\tERROR")
			for _, op := range ops {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", op.ID, op.Kind, op.State, op.ResourceID, op.Attempt, op.ErrorClass)
			}
			return w.Flush()
		},
	}
}

func eventsCmd(r *root) *cobra.Command {
	var since, after string
	cmd := &cobra.Command{
		Use:   "events",
		Short: "List audit events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			events, err := r.client().ListEvents(cmd.Context(), after, since, 100)
			if err != nil {
				return err
			}
			if r.output == "json" {
				return printJSON(cmd, events)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tTIME\tTYPE\tOUTCOME\tRESOURCE\tACTOR")
			for _, ev := range events {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					ev.ID, ev.TS.Format("15:04:05"), ev.Type, ev.Outcome, ev.ResourceID, ev.Actor)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "look-back window, e.g. 1h")
	cmd.Flags().StringVar(&after, "after", "", "cursor: return events after this evt_ id")
	return cmd
}

func providersCmd(r *root) *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "Show provider instance health (07 §8)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := r.client().ListProviders(cmd.Context())
			if err != nil {
				return err
			}
			if r.output == "json" {
				return printJSON(cmd, items)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "INSTANCE\tDRIVER\tSTATE\tFAILURES\tLAST ERROR")
			for _, p := range items {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", p.Instance, p.Driver, p.State, p.ConsecutiveFailures, p.LastError)
			}
			return w.Flush()
		},
	}
}

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage API tokens (local generation, ADR-008)"}
	var name string
	var perms []string
	newCmd := &cobra.Command{
		Use:   "new",
		Short: "Generate a token: prints the secret ONCE plus the config snippet",
		RunE: func(cmd *cobra.Command, _ []string) error {
			set, err := api.NewPermSet(perms)
			if err != nil {
				return err
			}
			plaintext, rec, err := api.GenerateToken(name, set)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "token: %s\n\nadd to fleetplane config:\n\nauth:\n  tokens:\n    - id: %s\n      name: %s\n      sha256: %x\n      permissions: [%s]\n",
				plaintext, rec.ID, rec.Name, rec.SHA256, strings.Join(perms, ", "))
			return nil
		},
	}
	newCmd.Flags().StringVar(&name, "name", "default", "token name (shown in audit events)")
	newCmd.Flags().StringSliceVar(&perms, "perm", []string{"admin"}, "permissions (07 §3)")
	cmd.AddCommand(newCmd)
	return cmd
}

func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}
