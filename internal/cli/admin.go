package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func adminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Server-host administration (ops listener)"}

	var to, opsAddr string
	backup := &cobra.Command{
		Use:   "backup --to PATH",
		Short: "Hot backup: VACUUM INTO via the ops listener (safe under WAL)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, _ := json.Marshal(map[string]string{"to": to})
			client := &http.Client{Timeout: 10 * time.Minute} // VACUUM scales with DB size
			resp, err := client.Post(opsAddr+"/admin/backup", "application/json", bytes.NewReader(body))
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				var eb struct {
					Error struct{ Message string } `json:"error"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&eb)
				return fmt.Errorf("backup failed (%d): %s", resp.StatusCode, eb.Error.Message)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backup written to %s\n", to)
			return nil
		},
	}
	defaultOps := os.Getenv("FLEETPLANE_OPS_ADDR")
	if defaultOps == "" {
		defaultOps = "http://127.0.0.1:9090"
	}
	backup.Flags().StringVar(&to, "to", "", "destination path ON THE SERVER HOST (required)")
	backup.Flags().StringVar(&opsAddr, "ops-addr", defaultOps, "ops listener address (env FLEETPLANE_OPS_ADDR)")
	_ = backup.MarkFlagRequired("to")

	cmd.AddCommand(backup)
	return cmd
}
