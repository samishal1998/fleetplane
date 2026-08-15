package cli

// `fleetplane apply -f` — the doc-10 IR-first declarative layer: versioned
// YAML/JSON manifests compiled onto the SAME imperative API (04 §8: the
// declarative path must never become a second orchestration engine).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func applyCmd(r *root) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "apply -f FILE",
		Short: "Apply declarative manifests (multi-doc YAML: Pool, Resource)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var in io.Reader
			if file == "-" {
				in = cmd.InOrStdin()
			} else {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				in = f
			}
			docs, err := decodeManifests(in)
			if err != nil {
				return err
			}
			if len(docs) == 0 {
				return fmt.Errorf("no manifests found in %s", file)
			}
			for i, doc := range docs {
				if err := applyOne(cmd, r, doc); err != nil {
					return fmt.Errorf("document %d: %w", i+1, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "manifest file, or - for stdin (required)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

type manifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Raw        json.RawMessage `json:"-"`
}

// decodeManifests reads --- separated YAML (or JSON) into JSON documents.
func decodeManifests(in io.Reader) ([]manifest, error) {
	raw, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	var out []manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("manifest parse: %w", err)
		}
		if len(v) == 0 {
			continue
		}
		j, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var m manifest
		if err := json.Unmarshal(j, &m); err != nil {
			return nil, err
		}
		m.Raw = j
		out = append(out, m)
	}
	return out, nil
}

func applyOne(cmd *cobra.Command, r *root, m manifest) error {
	if m.APIVersion != "" && m.APIVersion != apiclient.APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %s)", m.APIVersion, apiclient.APIVersion)
	}
	switch m.Kind {
	case "Class":
		var cm apiclient.ClassManifest
		if err := json.Unmarshal(m.Raw, &cm); err != nil {
			return err
		}
		// Class applies are declarative upserts keyed by name.
		cls, err := r.client().UpsertClass(cmd.Context(), cm.Metadata.Name, cm)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "class/%s: applied\n", cls.Metadata.Name)
		return nil
	case "Pool":
		var pm apiclient.PoolManifest
		if err := json.Unmarshal(m.Raw, &pm); err != nil {
			return err
		}
		pool, err := r.client().ApplyPool(cmd.Context(), pm)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "pool/%s (%s): applied\n", pool.Metadata.Name, pool.Metadata.ID)
		return nil
	case "Resource":
		var req apiclient.CreateResourceRequest
		if err := json.Unmarshal(m.Raw, &req); err != nil {
			return err
		}
		// Resource applies are create-only and keyed by name for
		// idempotent re-application of the same file.
		idemKey := "apply:" + req.Metadata.Name
		res, err := r.client().CreateResource(cmd.Context(), req, idemKey)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "resource/%s (%s): %s\n", res.Metadata.Name, res.Metadata.ID, res.Status.Phase)
		return nil
	default:
		return fmt.Errorf("unsupported manifest kind %q (want Class, Pool or Resource)", m.Kind)
	}
}
