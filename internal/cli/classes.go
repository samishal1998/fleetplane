package cli

// `fleetplane classes` — dynamic class management (04 §2). Config-file
// classes appear here too (source=config) but are read-only via the API.

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func classesCmd(r *root) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "classes",
		Short: "List and manage resource classes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			classes, err := r.client().ListClasses(cmd.Context())
			if err != nil {
				return err
			}
			if r.output == "json" {
				return printJSON(cmd, classes)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tPROVIDER\tSOURCE\tRECLAIM\tQUEUE")
			for _, c := range classes {
				reclaim, queue := "-", "-"
				if c.Spec.Reclaim != nil && c.Spec.Reclaim.IdleAfter != "" {
					reclaim = c.Spec.Reclaim.IdleAfter
				}
				if c.Spec.Scheduling != nil && c.Spec.Scheduling.Queue != nil && c.Spec.Scheduling.Queue.MaxWait != "" {
					queue = c.Spec.Scheduling.Queue.MaxWait
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					c.Metadata.Name, c.Spec.Kind, c.Spec.Provider, c.Source, reclaim, queue)
			}
			return w.Flush()
		},
	}
	cmd.AddCommand(classGetCmd(r), classCreateCmd(r), classDeleteCmd(r))
	return cmd
}

func classGetCmd(r *root) *cobra.Command {
	return &cobra.Command{
		Use:   "get NAME",
		Short: "Show one class",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cls, err := r.client().GetClass(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, cls)
		},
	}
}

func classCreateCmd(r *root) *cobra.Command {
	var kind, provider, template, templateFile, reclaimIdle, queueMaxWait string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a class (template validated against the kind registry)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tpl := json.RawMessage(template)
			if templateFile != "" {
				b, err := os.ReadFile(templateFile)
				if err != nil {
					return err
				}
				tpl = b
			}
			if len(tpl) == 0 {
				return fmt.Errorf("a template is required: --template '<json>' or --template-file spec.json")
			}
			m := apiclient.ClassManifest{
				APIVersion: apiclient.APIVersion, Kind: "Class",
				Metadata: apiclient.Metadata{Name: args[0]},
				Spec:     apiclient.ClassSpec{Kind: kind, Provider: provider, Template: tpl},
			}
			if reclaimIdle != "" {
				m.Spec.Reclaim = &apiclient.ClassReclaim{IdleAfter: reclaimIdle}
			}
			if queueMaxWait != "" {
				m.Spec.Scheduling = &apiclient.ClassScheduling{Queue: &apiclient.ClassQueue{MaxWait: queueMaxWait}}
			}
			cls, err := r.client().CreateClass(cmd.Context(), m)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "class/%s created\n", cls.Metadata.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "compute.machine", "resource kind")
	cmd.Flags().StringVar(&provider, "provider", "", "provider instance name (required)")
	cmd.Flags().StringVar(&template, "template", "", "kind-specific spec as inline JSON")
	cmd.Flags().StringVar(&templateFile, "template-file", "", "kind-specific spec from a JSON file")
	cmd.Flags().StringVar(&reclaimIdle, "reclaim-idle-after", "", "idle reclamation policy, e.g. 5m")
	cmd.Flags().StringVar(&queueMaxWait, "queue-max-wait", "", "acquisition queue budget, e.g. 10m (docs/11)")
	_ = cmd.MarkFlagRequired("provider")
	return cmd
}

func classDeleteCmd(r *root) *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete an api-managed class (refused while a pool references it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().DeleteClass(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "class/%s deleted\n", args[0])
			return nil
		},
	}
}
