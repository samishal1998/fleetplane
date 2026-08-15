package gcp

// GCP labels are strictly lowercase [a-z0-9_-] (keys must start with a
// letter, 63 chars max); dots, slashes and uppercase — everything the
// fleetplane.io/* identity keys are made of — are illegal. This codec is the
// GCP twin of providers/digitalocean/tags.go: identity keys map to fixed
// fp-* short names, values are lowercased on encode, and decode restores the
// ULID-shaped identity values (ULIDs are case-insensitive Crockford base32,
// canonically uppercase), so the SDK's label contract round-trips unchanged.

import (
	"regexp"
	"strings"

	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

const maxLabelLen = 63

// shortNames maps reserved label keys to fixed GCP-legal short keys.
var shortNames = map[string]string{
	provider.LabelManaged: "fp-managed",
	provider.LabelOwner:   "fp-owner",
	provider.LabelID:      "fp-id",
	provider.LabelOp:      "fp-op",
	provider.LabelClass:   "fp-class",
	provider.LabelTest:    "fp-test",
	provider.LabelTestRun: "fp-test-run",
}

var longNames = func() map[string]string {
	m := make(map[string]string, len(shortNames))
	for long, short := range shortNames {
		m[short] = long
	}
	return m
}()

// restoreCase marks the short keys whose values are prefixed ULIDs
// ("own_01H...", "res_01H...", "op_01H...") that must regain their canonical
// uppercase payload on decode.
var restoreCase = map[string]bool{
	"fp-owner":    true,
	"fp-id":       true,
	"fp-op":       true,
	"fp-test-run": true,
}

// ulidLike matches a lowercased prefixed ULID value: "<prefix>_<payload>".
var ulidLike = regexp.MustCompile(`^[a-z]+_[0-9a-z]+$`)

// encodeKey converts one SDK label key to a GCP-legal key. Identity keys use
// the fixed short-name map; anything else is sanitized (lowercase, illegal
// runes become '-', "u-" prefixed when not starting with a letter).
func encodeKey(k string) string {
	if short, ok := shortNames[k]; ok {
		return short
	}
	return truncateLabel(sanitizeKey(k))
}

func sanitizeKey(k string) string {
	s := sanitizeRunes(k)
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "u-" + s
	}
	return s
}

// encodeValue lowercases and sanitizes one label value.
func encodeValue(v string) string {
	return truncateLabel(sanitizeRunes(v))
}

func sanitizeRunes(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func truncateLabel(s string) string {
	if len(s) > maxLabelLen {
		return s[:maxLabelLen]
	}
	return s
}

// encodeLabels converts an SDK label map to GCP-legal labels.
func encodeLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[encodeKey(k)] = encodeValue(v)
	}
	return out
}

// decodeLabels recovers the SDK label map from a GCP resource's labels.
// Identity values regain their canonical uppercase ULID payload; fp-managed,
// fp-class and fp-test pass through; unknown keys are provider-native labels
// and pass through verbatim.
func decodeLabels(raw map[string]string) map[string]string {
	labels := map[string]string{}
	for k, v := range raw {
		long, known := longNames[k]
		if !known {
			labels[k] = v
			continue
		}
		if restoreCase[k] && ulidLike.MatchString(v) {
			prefix, payload, _ := strings.Cut(v, "_")
			v = prefix + "_" + strings.ToUpper(payload)
		}
		labels[long] = v
	}
	return labels
}
