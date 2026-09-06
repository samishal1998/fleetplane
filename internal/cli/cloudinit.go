package cli

// fleetplane cloud-init: render a #cloud-config that turns a fresh VM into
// the Fleetplane control plane — the systemd recipe from SETUP_GUIDE §4,
// generated from a config.yaml and the secrets it references. Local only:
// no server call. The token plaintext is never embedded; only its digest
// goes into the shipped config.

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/samishal1998/fleetplane/internal/api"
	"github.com/samishal1998/fleetplane/internal/config"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
)

// distroPackages: what the installer needs beyond the base image. Verified
// in containers (2026-09): /usr/sbin/nologin exists everywhere, so the
// service user needs no per-distro shell; the Fedora/RHEL family ships
// curl-minimal, and installing `curl` CONFLICTS with it — so that family
// omits curl (curl-minimal serves the installer fine).
var distroPackages = map[string][]string{
	"ubuntu":    {"curl", "tar", "ca-certificates"},
	"debian":    {"curl", "tar", "ca-certificates"},
	"fedora":    {"tar", "ca-certificates"},
	"rhel":      {"tar", "ca-certificates"},
	"rocky":     {"tar", "ca-certificates"},
	"almalinux": {"tar", "ca-certificates"},
	"centos":    {"tar", "ca-certificates"},
	"arch":      {"curl", "tar", "ca-certificates"},
	"opensuse":  {"curl", "tar", "ca-certificates"},
}

const systemdUnit = `[Unit]
Description=Fleetplane control plane
After=network-online.target
Wants=network-online.target

[Service]
User=fleetplane
Group=fleetplane
EnvironmentFile=/etc/fleetplane/secrets.env
ExecStart=/usr/local/bin/fleetplane serve --config /etc/fleetplane/config.yaml
Restart=always
RestartSec=2
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`

type cloudInitOpts struct {
	ConfigYAML []byte
	EnvFiles   [][]byte          // KEY=VALUE lines
	Environ    map[string]string // process env fallback for secret://env refs
	Distro     string
	Version    string // release tag to pin; "" = latest
	Token      string // pre-generated plaintext; "" = generate
	TokenName  string
	Perms      []string
}

type cloudInitOut struct {
	UserData []byte
	Token    string   // plaintext — only set when generated here
	Warnings []string // non-fatal: file-scheme secrets, storage path
}

func renderCloudInit(o cloudInitOpts) (cloudInitOut, error) {
	var out cloudInitOut
	pkgs, ok := distroPackages[o.Distro]
	if !ok {
		names := make([]string, 0, len(distroPackages))
		for d := range distroPackages {
			names = append(names, d)
		}
		sort.Strings(names)
		return out, fmt.Errorf("unsupported distro %q (want one of %s)", o.Distro, strings.Join(names, ", "))
	}

	// Token: derive the record from a pre-generated plaintext, or mint one.
	set, err := api.NewPermSet(o.Perms)
	if err != nil {
		return out, err
	}
	var rec api.TokenRecord
	if o.Token != "" {
		if rec, err = api.ParseToken(o.Token, o.TokenName, set); err != nil {
			return out, err
		}
	} else {
		if out.Token, rec, err = api.GenerateToken(o.TokenName, set); err != nil {
			return out, err
		}
	}

	// Inject the token into the config (as a generic map, so unknown-but-
	// valid keys survive), then validate exactly what will ship.
	var cfg map[string]any
	if err := yaml.Unmarshal(o.ConfigYAML, &cfg); err != nil {
		return out, fmt.Errorf("config: %w", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	auth, _ := cfg["auth"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
	}
	tokens, _ := auth["tokens"].([]any)
	tokens = append(tokens, map[string]any{
		"id": rec.ID, "name": rec.Name, "sha256": fmt.Sprintf("%x", rec.SHA256), "permissions": o.Perms,
	})
	auth["tokens"] = tokens
	cfg["auth"] = auth
	shipped, err := yaml.Marshal(cfg)
	if err != nil {
		return out, err
	}
	parsed, err := config.Parse(shipped)
	if err != nil {
		return out, err
	}
	if !strings.HasPrefix(parsed.Storage.Path, "/var/lib/fleetplane/") {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"storage.path %q is outside /var/lib/fleetplane — the service runs as user fleetplane, which owns only that directory", parsed.Storage.Path))
	}

	// Every secret://env/NAME the config references must be satisfied by
	// the env files or the current environment; missing ones are one error.
	env := map[string]string{}
	for _, f := range o.EnvFiles {
		parseEnvFile(f, env)
	}
	var missing []string
	seen := map[string]bool{}
	walkStrings(cfg, func(s string) {
		if !secretref.IsRef(s) {
			return
		}
		scheme, path, err := secretref.Split(s)
		if err != nil {
			return // config.Parse does not validate refs; boot will
		}
		switch scheme {
		case "env":
			if seen[path] {
				return
			}
			seen[path] = true
			if _, ok := env[path]; ok {
				return
			}
			if v, ok := o.Environ[path]; ok {
				env[path] = v
				return
			}
			missing = append(missing, path)
		case "file":
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: files are not shipped by cloud-init; place %s on the VM yourself", s, path))
		}
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return out, fmt.Errorf("config references secret://env/{%s} but no value was given (use --env FILE or export them)", strings.Join(missing, ", "))
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	var secrets strings.Builder
	for _, k := range names {
		fmt.Fprintf(&secrets, "%s=%s\n", k, env[k])
	}

	install := "curl -fsSL https://raw.githubusercontent.com/samishal1998/fleetplane/main/install.sh | FLEETPLANE_INSTALL_DIR=/usr/local/bin"
	if o.Version != "" {
		install += " FLEETPLANE_VERSION=" + o.Version
	}
	install += " sh"

	// write_files runs before users are created, so files are written
	// root-owned with final modes and ownership is fixed in runcmd — the
	// same commands as SETUP_GUIDE §4.
	doc := map[string]any{
		"package_update": true,
		"packages":       pkgs,
		"write_files": []map[string]any{
			{"path": "/etc/fleetplane/config.yaml", "permissions": "0640", "content": string(shipped)},
			{"path": "/etc/fleetplane/secrets.env", "permissions": "0600", "content": secrets.String()},
			{"path": "/etc/systemd/system/fleetplane.service", "permissions": "0644", "content": systemdUnit},
		},
		"runcmd": []string{
			"useradd --system --home /var/lib/fleetplane --shell /usr/sbin/nologin fleetplane || true",
			"mkdir -p /var/lib/fleetplane",
			"chown fleetplane:fleetplane /var/lib/fleetplane",
			"chgrp fleetplane /etc/fleetplane/config.yaml",
			install,
			"systemctl daemon-reload",
			"systemctl enable --now fleetplane",
		},
	}
	body, err := yaml.MarshalWithOptions(doc, yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return out, err
	}
	out.UserData = append([]byte("#cloud-config\n"), body...)
	return out, nil
}

// parseEnvFile reads KEY=VALUE lines (comments, blanks and a leading
// "export " tolerated; surrounding quotes stripped).
func parseEnvFile(b []byte, into map[string]string) {
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		into[strings.TrimSpace(k)] = v
	}
}

func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, c := range t {
			walkStrings(c, fn)
		}
	case []any:
		for _, c := range t {
			walkStrings(c, fn)
		}
	}
}

func cloudInitCmd(version string) *cobra.Command {
	var cfgPath, outPath, distro, pin, token, tokenName string
	var envFiles, perms []string
	cmd := &cobra.Command{
		Use:   "cloud-init --config config.yaml [--env secrets.env] [--distro ubuntu] [--out user-data.yaml]",
		Short: "Render a cloud-init user-data file that bootstraps a Fleetplane control VM",
		Long: `Render a #cloud-config that installs the pinned release, writes the config
and provider secrets, and starts Fleetplane under systemd — the SETUP_GUIDE §4
recipe, generated. Every secret://env/NAME the config references must be
supplied via --env files or the current environment (missing names are listed).

An API token is added to the shipped config: pass a pre-generated one with
--token (only its digest ships), or omit it and one is generated and printed
to stderr exactly once.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgBytes, err := os.ReadFile(cfgPath)
			if err != nil {
				return err
			}
			o := cloudInitOpts{ConfigYAML: cfgBytes, Environ: map[string]string{}, Distro: distro,
				Version: pin, Token: token, TokenName: tokenName, Perms: perms}
			for _, f := range envFiles {
				b, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				o.EnvFiles = append(o.EnvFiles, b)
			}
			for _, kv := range os.Environ() {
				if k, v, ok := strings.Cut(kv, "="); ok {
					o.Environ[k] = v
				}
			}
			out, err := renderCloudInit(o)
			if err != nil {
				return err
			}
			for _, w := range out.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			if out.Token != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "token (%s, shown once): %s\n", tokenName, out.Token)
			}
			if outPath == "" {
				_, err = cmd.OutOrStdout().Write(out.UserData)
				return err
			}
			return os.WriteFile(outPath, out.UserData, 0o600)
		},
	}
	defaultPin := ""
	if strings.HasPrefix(version, "v") {
		defaultPin = version
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "config.yaml to ship (required)")
	cmd.Flags().StringSliceVar(&envFiles, "env", nil, "KEY=VALUE file(s) supplying secret://env references (repeatable)")
	cmd.Flags().StringVar(&distro, "distro", "ubuntu", "target distro: ubuntu, debian, fedora, rhel, rocky, almalinux, centos, arch, opensuse")
	cmd.Flags().StringVar(&pin, "version", defaultPin, "release tag to install (default: this CLI's version; empty = latest)")
	cmd.Flags().StringVar(&token, "token", "", "pre-generated API token (flp_…); omit to generate one")
	cmd.Flags().StringVar(&tokenName, "token-name", "admin", "token name (shown in audit events)")
	cmd.Flags().StringSliceVar(&perms, "perm", []string{"admin"}, "token permissions")
	cmd.Flags().StringVar(&outPath, "out", "", "write user-data here (default stdout)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
