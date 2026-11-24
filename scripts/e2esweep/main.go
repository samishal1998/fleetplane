// Command e2esweep deletes leftover E2E servers (ADR-015). It refuses to
// touch ANYTHING that does not carry fleetplane.io/test=1 — conservative
// destruction applies to test tooling too.
//
// Hetzner runs when HETZNER_TOKEN is set; GCP when GCP_E2E_PROJECT and
// GCP_E2E_CREDENTIALS_FILE are set; both when both are. E2E_SWEEP_ALL=1
// drops the age backstop for every cloud, never the label rule.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	gce "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// gcpTestKey is provider.LabelTest ("fleetplane.io/test") as the GCP driver
// encodes it (providers/gcp/labels.go shortNames; the value "1" encodes to
// itself). GCP label keys cannot hold "." or "/". sweep_test.go pins this
// constant against the driver's real encoding.
const gcpTestKey = "fp-test"

func main() {
	token := os.Getenv("HETZNER_TOKEN")
	project, keyFile := os.Getenv("GCP_E2E_PROJECT"), os.Getenv("GCP_E2E_CREDENTIALS_FILE")
	useGCP := project != "" && keyFile != ""
	if token == "" && !useGCP {
		fmt.Println("e2esweep: HETZNER_TOKEN and GCP_E2E_PROJECT/GCP_E2E_CREDENTIALS_FILE unset; nothing to do")
		return
	}
	maxAge := 2 * time.Hour
	if os.Getenv("E2E_SWEEP_ALL") == "1" {
		maxAge = 0
	}
	ctx := context.Background()

	ok := true
	if token != "" {
		ok = sweepHetzner(ctx, token, maxAge) && ok
		ok = sweepHetznerSnapshots(ctx, token, maxAge) && ok
	}
	if useGCP {
		// The library reads the key file; this command never does.
		svc, err := gce.NewService(ctx,
			option.WithAuthCredentialsFile(option.ServiceAccount, keyFile),
			option.WithUserAgent("fleetplane-e2esweep"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2esweep: gcp:", err)
			ok = false
		} else {
			ok = sweepGCP(ctx, svc, project, maxAge) && ok
			ok = sweepGCPImages(ctx, svc, project, maxAge) && ok
		}
	}
	if !ok {
		os.Exit(1)
	}
}

func sweepHetzner(ctx context.Context, token string, maxAge time.Duration) bool {
	client := hcloud.NewClient(hcloud.WithToken(token))

	servers, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: "fleetplane.io/test=1", PerPage: 50},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2esweep:", err)
		return false
	}
	swept, failed := 0, 0
	for _, srv := range servers {
		if srv.Labels["fleetplane.io/test"] != "1" {
			continue // double-check the label even after server-side filtering
		}
		if time.Since(srv.Created) < maxAge {
			fmt.Printf("e2esweep: keeping %s (%d): younger than %s\n", srv.Name, srv.ID, maxAge)
			continue
		}
		if _, _, err := client.Server.DeleteWithResult(ctx, srv); err != nil {
			fmt.Fprintf(os.Stderr, "e2esweep: delete %d: %v\n", srv.ID, err)
			failed++
			continue
		}
		fmt.Printf("e2esweep: deleted %s (%d)\n", srv.Name, srv.ID)
		swept++
	}
	fmt.Printf("e2esweep: %d deleted, %d failed, %d total test servers\n", swept, failed, len(servers))
	return failed == 0
}

// sweepGCP deletes test-labeled instances in every zone of the project
// (aggregated list: a crashed test may have created one anywhere). Disks
// need no pass of their own: the driver creates only a boot disk with
// autoDelete=true, and labels the instance, not the disk — so a label-ruled
// disk sweep could never match anything.
func sweepGCP(ctx context.Context, svc *gce.Service, project string, maxAge time.Duration) bool {
	var instances []*gce.Instance
	err := svc.Instances.AggregatedList(project).
		Filter(fmt.Sprintf("labels.%s = \"1\"", gcpTestKey)).
		Pages(ctx, func(page *gce.InstanceAggregatedList) error {
			for _, scoped := range page.Items {
				instances = append(instances, scoped.Instances...)
			}
			return nil
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2esweep: gcp:", err)
		return false
	}
	swept, failed := 0, 0
	for _, inst := range instances {
		if inst == nil || inst.Labels[gcpTestKey] != "1" {
			continue // double-check the label even after server-side filtering
		}
		created, err := time.Parse(time.RFC3339, inst.CreationTimestamp)
		if err != nil {
			created = time.Now() // unknown age counts as young: conservative
		}
		if time.Since(created) < maxAge {
			fmt.Printf("e2esweep: gcp: keeping %s: younger than %s\n", inst.Name, maxAge)
			continue
		}
		zone := path.Base(inst.Zone)
		if _, err := svc.Instances.Delete(project, zone, inst.Name).Context(ctx).Do(); err != nil {
			var gerr *googleapi.Error
			if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
				fmt.Printf("e2esweep: gcp: %s/%s already gone\n", zone, inst.Name)
				continue
			}
			fmt.Fprintf(os.Stderr, "e2esweep: gcp: delete %s/%s: %v\n", zone, inst.Name, err)
			failed++
			continue
		}
		fmt.Printf("e2esweep: gcp: deleted %s/%s\n", zone, inst.Name)
		swept++
	}
	fmt.Printf("e2esweep: gcp: %d deleted, %d failed, %d total test instances\n", swept, failed, len(instances))
	return failed == 0
}

// sweepHetznerSnapshots deletes test-labeled snapshots (the custom-image
// E2E builds one from a running server). Same label + age rules.
func sweepHetznerSnapshots(ctx context.Context, token string, maxAge time.Duration) bool {
	client := hcloud.NewClient(hcloud.WithToken(token))
	images, err := client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: "fleetplane.io/test=1", PerPage: 50},
		Type:     []hcloud.ImageType{hcloud.ImageTypeSnapshot},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2esweep: snapshots:", err)
		return false
	}
	swept, failed := 0, 0
	for _, img := range images {
		if img.Labels["fleetplane.io/test"] != "1" || img.Type != hcloud.ImageTypeSnapshot {
			continue
		}
		if time.Since(img.Created) < maxAge {
			fmt.Printf("e2esweep: keeping snapshot %d: younger than %s\n", img.ID, maxAge)
			continue
		}
		if _, err := client.Image.Delete(ctx, img); err != nil {
			fmt.Fprintf(os.Stderr, "e2esweep: delete snapshot %d: %v\n", img.ID, err)
			failed++
			continue
		}
		fmt.Printf("e2esweep: deleted snapshot %s (%d)\n", img.Description, img.ID)
		swept++
	}
	fmt.Printf("e2esweep: %d deleted, %d failed, %d total test snapshots\n", swept, failed, len(images))
	return failed == 0
}

// sweepGCPImages deletes test-labeled custom images (the "snapshot:"
// image-selector E2E creates one). Same label + age rules.
func sweepGCPImages(ctx context.Context, svc *gce.Service, project string, maxAge time.Duration) bool {
	var images []*gce.Image
	err := svc.Images.List(project).Filter(fmt.Sprintf("labels.%s = \"1\"", gcpTestKey)).
		Pages(ctx, func(page *gce.ImageList) error {
			images = append(images, page.Items...)
			return nil
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2esweep: gcp images:", err)
		return false
	}
	swept, failed := 0, 0
	for _, img := range images {
		if img == nil || img.Labels[gcpTestKey] != "1" {
			continue
		}
		created, err := time.Parse(time.RFC3339, img.CreationTimestamp)
		if err != nil {
			created = time.Now()
		}
		if time.Since(created) < maxAge {
			fmt.Printf("e2esweep: gcp: keeping image %s: younger than %s\n", img.Name, maxAge)
			continue
		}
		if _, err := svc.Images.Delete(project, img.Name).Context(ctx).Do(); err != nil {
			var gerr *googleapi.Error
			if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
				continue
			}
			fmt.Fprintf(os.Stderr, "e2esweep: gcp: delete image %s: %v\n", img.Name, err)
			failed++
			continue
		}
		fmt.Printf("e2esweep: gcp: deleted image %s\n", img.Name)
		swept++
	}
	fmt.Printf("e2esweep: gcp: %d deleted, %d failed, %d total test images\n", swept, failed, len(images))
	return failed == 0
}
