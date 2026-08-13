package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/samimishal/fleetplane/pkg/apiclient"
)

func resourcesCmd(r *root) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resources",
		Short: "List and manage resources",
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := r.client().ListResources(cmd.Context())
			if err != nil {
				return err
			}
			if r.output == "json" {
				return printJSON(cmd, list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tPROVIDER\tPHASE\tEXTERNAL\tNAME")
			for _, it := range list.Items {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					it.Metadata.ID, it.Spec.Kind, it.Spec.Provider,
					it.Status.Phase, it.Status.ExternalID, it.Metadata.Name)
			}
			return w.Flush()
		},
	}

	get := &cobra.Command{
		Use:   "get ID",
		Short: "Show one resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := r.client().GetResource(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}

	var delIdemKey string
	del := &cobra.Command{
		Use:   "delete ID",
		Short: "Delete a resource (drain-safe: refused while leased)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().DeleteResource(cmd.Context(), args[0], delIdemKey); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: deleting\n", args[0])
			return nil
		},
	}
	del.Flags().StringVar(&delIdemKey, "idempotency-key", "", "idempotency key")

	var createFile, createIdemKey string
	create := &cobra.Command{
		Use:   "create -f FILE",
		Short: "Create a resource from a JSON manifest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(createFile)
			if err != nil {
				return err
			}
			var req apiclient.CreateResourceRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return fmt.Errorf("manifest: %w", err)
			}
			res, err := r.client().CreateResource(cmd.Context(), req, createIdemKey)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", res.Metadata.ID, res.Status.Phase)
			return nil
		},
	}
	create.Flags().StringVarP(&createFile, "file", "f", "", "manifest file (required)")
	create.Flags().StringVar(&createIdemKey, "idempotency-key", "", "idempotency key")
	_ = create.MarkFlagRequired("file")

	cmd.AddCommand(get, del, create)
	return cmd
}

func printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
