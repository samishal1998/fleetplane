package digitalocean

// DigitalOcean has no key=value labels — only flat tags of letters, numbers,
// colons, dashes and underscores. This codec is the Phase-8 portability
// correction: Fleetplane's identity labels round-trip through tags of the
// form "fp-<short>:<value>", entirely inside the driver — the SDK contract
// (map[string]string labels) never changed.

import (
	"strings"

	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// shortNames maps reserved label keys to compact tag prefixes.
var shortNames = map[string]string{
	provider.LabelManaged: "managed",
	provider.LabelOwner:   "owner",
	provider.LabelID:      "id",
	provider.LabelOp:      "op",
	provider.LabelClass:   "class",
	provider.LabelTest:    "test",
	provider.LabelTestRun: "test-run",
	"fleetplane.io/pool":  "pool",
}

var longNames = func() map[string]string {
	m := map[string]string{}
	for long, short := range shortNames {
		m[short] = long
	}
	return m
}()

const tagPrefix = "fp-"

// sanitizeTag replaces characters DO tags forbid. ULID-based values
// (res_/op_/own_ + Crockford base32) pass through unchanged.
func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ':', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// encodeTag converts one label to a tag. Unknown keys are dropped (foreign
// labels have no reserved encoding; DO tags are not a general label store).
func encodeTag(k, v string) (string, bool) {
	short, ok := shortNames[k]
	if !ok {
		return "", false
	}
	return tagPrefix + short + ":" + sanitizeTag(v), true
}

// encodeTags converts a label map to a sorted-stable tag list.
func encodeTags(labels map[string]string) []string {
	var out []string
	for k, v := range labels {
		if tag, ok := encodeTag(k, v); ok {
			out = append(out, tag)
		}
	}
	return out
}

// decodeTags recovers the label map from a droplet's tags.
func decodeTags(tags []string) map[string]string {
	labels := map[string]string{}
	for _, tag := range tags {
		body, ok := strings.CutPrefix(tag, tagPrefix)
		if !ok {
			continue
		}
		short, value, ok := strings.Cut(body, ":")
		if !ok {
			continue
		}
		long, known := longNames[short]
		if !known {
			continue
		}
		labels[long] = value
	}
	return labels
}
