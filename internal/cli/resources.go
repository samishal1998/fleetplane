package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
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

	var createFile, createIdemKey, cName, cClass, cProvider, cKind, cServerType, cImage, cLocation, cUserDataFile string
	var cLabels []string
	create := &cobra.Command{
		Use:   "create [-f FILE] [--class C] [--image I] [--user-data-file F] ...",
		Short: "Create a resource from a manifest and/or flags (flags overlay the manifest)",
		Long: `Create one resource. A JSON manifest (-f) and flags compose: flags fill or
override the manifest. With --class, kind and provider come from the class and
the machine fields given here (--image, --server-type, --location, --user-data-file)
overlay the class spec — e.g. the class's machine from a different snapshot, or
with a specific cloud-init file as user data.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var req apiclient.CreateResourceRequest
			if createFile != "" {
				raw, err := os.ReadFile(createFile)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(raw, &req); err != nil {
					return fmt.Errorf("manifest: %w", err)
				}
			}
			machine := map[string]any{}
			if len(req.Spec.Machine) > 0 {
				if err := json.Unmarshal(req.Spec.Machine, &machine); err != nil {
					return fmt.Errorf("manifest spec.machine: %w", err)
				}
			}
			for k, v := range map[string]string{"serverType": cServerType, "image": cImage, "location": cLocation} {
				if v != "" {
					machine[k] = v
				}
			}
			if cUserDataFile != "" {
				ud, err := os.ReadFile(cUserDataFile)
				if err != nil {
					return err
				}
				machine["userData"] = string(ud)
			}
			if len(cLabels) > 0 {
				labels := map[string]string{}
				if existing, ok := machine["labels"].(map[string]any); ok {
					for k, v := range existing {
						labels[k] = fmt.Sprint(v)
					}
				}
				for _, kv := range cLabels {
					k, v, ok := strings.Cut(kv, "=")
					if !ok {
						return fmt.Errorf("--label wants key=value, got %q", kv)
					}
					labels[k] = v
				}
				machine["labels"] = labels
			}
			if len(machine) > 0 {
				req.Spec.Machine, _ = json.Marshal(machine)
			}
			for dst, v := range map[*string]string{&req.Metadata.Name: cName, &req.Spec.Class: cClass, &req.Spec.Provider: cProvider, &req.Spec.Kind: cKind} {
				if v != "" {
					*dst = v
				}
			}
			if req.Spec.Class == "" && req.Spec.Kind == "" {
				req.Spec.Kind = "compute.machine"
			}
			if createFile == "" && req.Spec.Class == "" && req.Spec.Provider == "" {
				return fmt.Errorf("need -f, --class, or --provider with --image/--server-type")
			}
			res, err := r.client().CreateResource(cmd.Context(), req, createIdemKey)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", res.Metadata.ID, res.Status.Phase)
			return nil
		},
	}
	create.Flags().StringVarP(&createFile, "file", "f", "", "JSON manifest file")
	create.Flags().StringVar(&createIdemKey, "idempotency-key", "", "idempotency key")
	create.Flags().StringVar(&cName, "name", "", "resource name")
	create.Flags().StringVar(&cClass, "class", "", "class to create from (kind/provider inherited; machine flags overlay its spec)")
	create.Flags().StringVar(&cProvider, "provider", "", "provider instance (when not using --class)")
	create.Flags().StringVar(&cKind, "kind", "", "resource kind (default compute.machine)")
	create.Flags().StringVar(&cServerType, "server-type", "", "machine serverType")
	create.Flags().StringVar(&cImage, "image", "", `machine image, e.g. "snapshot:ci-runner=v12" or "name:ubuntu-24.04"`)
	create.Flags().StringVar(&cLocation, "location", "", "machine location/zone")
	create.Flags().StringVar(&cUserDataFile, "user-data-file", "", "file whose content becomes the machine's user data (cloud-init)")
	create.Flags().StringSliceVar(&cLabels, "label", nil, "machine label key=value (repeatable)")

	drain := &cobra.Command{
		Use:   "drain ID",
		Short: "Drain a resource: no new leases; deleted once existing leases end",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().DrainResource(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: draining\n", args[0])
			return nil
		},
	}

	park := &cobra.Command{
		Use:   "park ID",
		Short: "Stop a ready machine into the near-free parked tier (docs/12)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().ParkResource(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: parking\n", args[0])
			return nil
		},
	}
	start := &cobra.Command{
		Use:   "start ID",
		Short: "Start a parked machine back into service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := r.client().StartResource(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: starting\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(get, del, create, drain, park, start)
	return cmd
}

func printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
