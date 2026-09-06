package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

const sampleConfig = `server:
  addr: ":8080"
storage:
  path: /var/lib/fleetplane/fleetplane.db
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
      location: fsn1
  gcp-eu:
    driver: gcp
    settings:
      project: p
      zone: europe-west1-b
      credentialsJson: secret://file/etc/fleetplane/gcp.json
classes:
  ci:
    kind: compute.machine
    provider: hetzner-main
    spec: { serverType: cpx31, image: "snapshot:ci=1" }
`

func TestCloudInitRender(t *testing.T) {
	out, err := renderCloudInit(cloudInitOpts{
		ConfigYAML: []byte(sampleConfig),
		EnvFiles:   [][]byte{[]byte("# secrets\nexport HETZNER_TOKEN=\"abc123\"\n")},
		Distro:     "almalinux",
		Version:    "v0.7.0",
		Perms:      []string{"admin"},
		TokenName:  "ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	ud := string(out.UserData)
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("missing #cloud-config header")
	}
	if out.Token == "" || strings.Contains(ud, out.Token) {
		t.Fatal("token must be generated and must never appear in user-data")
	}
	secret := strings.TrimPrefix(out.Token[strings.Index(out.Token, ".")+1:], "")
	if strings.Contains(ud, secret) {
		t.Fatal("token secret leaked into user-data")
	}
	for _, want := range []string{"sha256: ", "name: ops", "HETZNER_TOKEN=abc123", "FLEETPLANE_VERSION=v0.7.0",
		"/etc/systemd/system/fleetplane.service", "systemctl enable --now fleetplane", "useradd --system"} {
		if !strings.Contains(ud, want) {
			t.Fatalf("user-data missing %q:\n%s", want, ud)
		}
	}
	// Fedora family: curl-minimal conflicts with curl, so curl is not listed.
	var doc struct {
		Packages []string `yaml:"packages"`
	}
	if err := yaml.Unmarshal(out.UserData, &doc); err != nil {
		t.Fatalf("user-data is not valid YAML: %v", err)
	}
	for _, p := range doc.Packages {
		if p == "curl" {
			t.Fatal("almalinux package list must not include curl")
		}
	}
	// secret://file refs are warned about, not shipped.
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "gcp.json") {
		t.Fatalf("want one file-secret warning, got %v", out.Warnings)
	}

	// The shipped config re-parses with the token installed.
	var cc struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	_ = yaml.Unmarshal(out.UserData, &cc)
	if len(cc.WriteFiles) != 3 || !strings.Contains(cc.WriteFiles[0].Content, "sha256:") {
		t.Fatalf("write_files = %+v", cc.WriteFiles)
	}

	// One runnable check against the real validator when it is installed.
	if ci, err := exec.LookPath("cloud-init"); err == nil {
		f := filepath.Join(t.TempDir(), "user-data.yaml")
		if err := os.WriteFile(f, out.UserData, 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := exec.Command(ci, "schema", "--config-file", f).CombinedOutput()
		if err != nil || !bytes.Contains(res, []byte("Valid")) {
			t.Fatalf("cloud-init schema: %v\n%s", err, res)
		}
	}
}

func TestCloudInitPreGeneratedTokenAndMissingSecrets(t *testing.T) {
	_, err := renderCloudInit(cloudInitOpts{ConfigYAML: []byte(sampleConfig), Distro: "ubuntu", Perms: []string{"admin"}})
	if err == nil || !strings.Contains(err.Error(), "HETZNER_TOKEN") {
		t.Fatalf("missing secret must be an error naming it, got %v", err)
	}
	out, err := renderCloudInit(cloudInitOpts{
		ConfigYAML: []byte(sampleConfig), Environ: map[string]string{"HETZNER_TOKEN": "fromenv"},
		Distro: "ubuntu", Perms: []string{"resource.read"}, TokenName: "ro",
		Token: "flp_0123abcd.c2VjcmV0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ud := string(out.UserData)
	if out.Token != "" || strings.Contains(ud, "c2VjcmV0") || !strings.Contains(ud, "id: 0123abcd") || !strings.Contains(ud, "HETZNER_TOKEN=fromenv") {
		t.Fatalf("pre-generated token handling wrong:\n%s", ud)
	}
	if _, err := renderCloudInit(cloudInitOpts{ConfigYAML: []byte(sampleConfig), Distro: "alpine", Perms: []string{"admin"}}); err == nil {
		t.Fatal("alpine (no systemd) must be rejected")
	}
}
