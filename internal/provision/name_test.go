package provision

import (
	"regexp"
	"testing"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/storage"
)

// Both clouds' name rules: GCE's regex is the stricter superset of
// RFC 1123 labels (Hetzner), so passing it passes both.
var gceName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

func TestDefaultNameIsAValidCloudName(t *testing.T) {
	id := storage.ResourceID(ids.New(ids.Resource))
	if got := DefaultName(id); !gceName.MatchString(got) {
		t.Fatalf("DefaultName(%s) = %q: not a valid Hetzner/GCE name", id, got)
	}
}
