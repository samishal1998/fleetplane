package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// The OpenAPI spec and the route table must agree in BOTH directions
// (ADR-011): every registered route is documented, every documented path is
// served.
func TestOpenAPI_RouteTableDiff(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("spec invalid: %v", err)
	}

	spec := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			spec[method+" "+path] = true
		}
	}

	srv := &Server{}
	table := map[string]bool{}
	for _, rt := range srv.Routes() {
		table[rt.Method+" "+rt.SpecPath] = true
	}

	for key := range table {
		if !spec[key] {
			t.Errorf("route %q served but missing from api/openapi.yaml", key)
		}
	}
	for key := range spec {
		if !table[key] {
			t.Errorf("path %q documented but not served", key)
		}
	}
}

// Every route carries a permission and colon verbs parse.
func TestRouteTableInvariants(t *testing.T) {
	srv := &Server{}
	for _, rt := range srv.Routes() {
		if rt.Perm == "" {
			t.Errorf("route %s %s has no permission (07 §3)", rt.Method, rt.SpecPath)
		}
		if strings.Contains(rt.SpecPath, ":") && rt.Verb == "" {
			t.Errorf("route %s has a colon path but no verb", rt.SpecPath)
		}
		if rt.Method != "GET" && !rt.Mutating && rt.Method != "HEAD" {
			t.Errorf("non-GET route %s %s not marked mutating", rt.Method, rt.SpecPath)
		}
	}
	if id, verb, ok := splitVerb("res_abc:drain"); !ok || id != "res_abc" || verb != "drain" {
		t.Fatalf("splitVerb broken: %q %q %v", id, verb, ok)
	}
	if _, _, ok := splitVerb("res_abc"); ok {
		t.Fatal("splitVerb accepted a verbless segment")
	}
}

func TestTokenRoundTripAndRedaction(t *testing.T) {
	perms, err := NewPermSet([]string{"resource.read", "resource.acquire"})
	if err != nil {
		t.Fatal(err)
	}
	plaintext, rec, err := GenerateToken("ci", perms)
	if err != nil {
		t.Fatal(err)
	}
	auth := NewTokenAuthenticator([]TokenRecord{rec})

	p, err := auth.Authenticate("Bearer " + plaintext)
	if err != nil {
		t.Fatalf("generated token failed to authenticate: %v", err)
	}
	if !p.Perms.Allows(PermResourceRead) || p.Perms.Allows(PermResourceDelete) {
		t.Fatal("permission set wrong")
	}

	for _, bad := range []string{
		"", "Bearer ", "Bearer flp_", "Bearer nope", "Bearer " + plaintext + "x",
		"Bearer flp_deadbeef.wrongsecret",
	} {
		if _, err := auth.Authenticate(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}

	if _, err := NewPermSet([]string{"root"}); err == nil {
		t.Fatal("unknown permission accepted")
	}
}

// Every query parameter a handler reads must be declared in the spec. The
// route diff above cannot see parameters, and an undocumented one is invisible
// to the generated API reference. Coarse by design — a name declared on any
// operation satisfies it — so it catches the forgotten param, not a misplaced one.
func TestOpenAPI_QueryParamsDeclared(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	add := func(ps openapi3.Parameters) {
		for _, p := range ps {
			if p.Value != nil && p.Value.In == openapi3.ParameterInQuery {
				declared[p.Value.Name] = true
			}
		}
	}
	for _, item := range doc.Paths.Map() {
		add(item.Parameters)
		for _, op := range item.Operations() {
			add(op.Parameters)
		}
	}

	read := regexp.MustCompile(`(?:Query\(\)|\bq)(?:\.Get\(|\[)"([A-Za-z]+)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range read.FindAllStringSubmatch(string(src), -1) {
			if !declared[m[1]] {
				t.Errorf("%s reads query param %q but api/openapi.yaml declares it nowhere", f, m[1])
			}
		}
	}
}
