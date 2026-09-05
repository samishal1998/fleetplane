// Package docker drives compute.machine as Docker containers: a real,
// asynchronous, label-addressable provider available on any developer
// machine or CI runner, so the full kernel can be exercised end to end
// without cloud credentials (ADR-020). It speaks the Engine HTTP API over
// the socket with net/http only — no moby client dependency.
//
// Containers are cheap stand-ins for VMs: create+start = provision, stop/
// start = park/resume (ParkAware), remove = delete. Identity labels are
// applied verbatim as Docker labels and filtered server-side on Discover.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

const Driver = "docker"

func init() {
	provider.Register(Driver, func(_ context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
		return New(cfg)
	})
}

// Settings is the driver config block (03 §4). No credentials: access is
// the socket's filesystem permission (or the daemon's TLS on tcp hosts).
type Settings struct {
	Host    string   `json:"host,omitempty"`    // unix:///var/run/docker.sock (default) | tcp://host:2375
	Command []string `json:"command,omitempty"` // container entrypoint override; default keeps PID 1 alive
	Network string   `json:"network,omitempty"` // docker network to attach; default bridge
}

// defaultCommand is portable across busybox and coreutils (sleep infinity
// is GNU-only); with HostConfig.Init the bundled docker-init forwards
// SIGTERM so stop is immediate rather than the 10s timeout+SIGKILL.
var defaultCommand = []string{"sleep", "2147483647"}

type Docker struct {
	http     *http.Client
	base     string // http://docker or http://host:port
	instance string
	ownerID  string
	command  []string
	network  string
}

func New(cfg provider.InstanceConfig) (*Docker, error) {
	var s Settings
	if len(cfg.Settings) > 0 && string(cfg.Settings) != "null" {
		if err := json.Unmarshal(cfg.Settings, &s); err != nil {
			return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: cfg.Instance, Message: "docker settings: " + err.Error()}
		}
	}
	host := s.Host
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	d := &Docker{instance: cfg.Instance, ownerID: cfg.OwnerID, command: s.Command, network: s.Network}
	if len(d.command) == 0 {
		d.command = defaultCommand
	}
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		d.base = "http://docker"
		d.http = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", path)
			},
		}}
	case strings.HasPrefix(host, "tcp://"):
		d.base = "http://" + strings.TrimPrefix(host, "tcp://")
		d.http = &http.Client{}
	default:
		return nil, &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
			Provider: cfg.Instance, Message: "docker host must be unix:// or tcp://"}
	}
	return d, nil
}

// --- provider.Provider ---

func (d *Docker) Descriptor() provider.Descriptor {
	return provider.Descriptor{
		Driver: Driver, Instance: d.instance, Version: "engine-api",
		Kinds:                  []provider.ResourceKind{compute.Kind},
		SupportsLabelDiscovery: true,
	}
}

func (d *Docker) Capabilities(context.Context) ([]provider.CapabilityID, error) {
	return []provider.CapabilityID{"compute.machine.create", "compute.machine.park"}, nil
}

func (d *Docker) ResourceDriver(kind provider.ResourceKind) (provider.ResourceDriver, bool) {
	if kind != compute.Kind {
		return nil, false
	}
	return d, true
}

// Parking implements provider.ParkAware: stopped containers cost nothing
// but disk, and start is sub-second.
func (d *Docker) Parking(kind provider.ResourceKind) provider.ParkPolicy {
	if kind == compute.Kind {
		return provider.ParkPolicy{Supported: true, StartEstimate: 2 * time.Second}
	}
	return provider.ParkPolicy{}
}

func (d *Docker) Health(ctx context.Context) error {
	_, _, err := d.do(ctx, http.MethodGet, "/_ping", nil, provider.EffectNone)
	return err
}

func (d *Docker) Close() error { return nil }

// --- provider.ResourceDriver ---

func (d *Docker) Kind() provider.ResourceKind { return compute.Kind }

func (d *Docker) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.ObservedResource, error) {
	var labels []string
	if req.Scope != provider.ScopeAll {
		labels = append(labels, provider.LabelManaged+"=true", provider.LabelOwner+"="+d.ownerID)
	}
	for k, v := range req.Selector {
		labels = append(labels, k+"="+v)
	}
	filters, _ := json.Marshal(map[string][]string{"label": labels})
	q := url.Values{"all": {"true"}, "filters": {string(filters)}}
	_, body, err := d.do(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil, provider.EffectNone)
	if err != nil {
		return nil, err
	}
	var list []struct{ Id string }
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, d.retryable(err)
	}
	// The list endpoint omits HostConfig limits and per-network addresses;
	// inspect each hit so observations are identical to Get (03 §2).
	// ponytail: one inspect per container (O(n) socket round-trips; ScopeAll
	// walks the whole host); batch via a single list-with-size call if a
	// fleet of hundreds of containers ever makes discovery slow.
	var out []provider.ObservedResource
	for _, c := range list {
		obs, err := d.Get(ctx, provider.ExternalRef{ID: c.Id})
		if provider.IsClass(err, provider.ErrNotFound) {
			continue // removed between list and inspect
		}
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, nil
}

func (d *Docker) Get(ctx context.Context, ref provider.ExternalRef) (provider.ObservedResource, error) {
	_, body, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(ref.ID)+"/json", nil, provider.EffectNone)
	if err != nil {
		return provider.ObservedResource{}, err
	}
	return d.observe(body)
}

type createParams struct {
	Name   string              `json:"name"`
	Spec   compute.MachineSpec `json:"spec"`
	CPU    int64               `json:"cpu"`
	MemMiB int64               `json:"memMiB"`
	Labels map[string]string   `json:"labels"`
}

func (d *Docker) Plan(_ context.Context, req provider.PlanRequest) (provider.Plan, error) {
	switch {
	case req.Desired != nil && req.Observed == nil:
		spec, err := compute.ParseSpec(req.Desired.Spec)
		if err != nil {
			return provider.Plan{}, d.invalid(err.Error())
		}
		cpu, mem, err := parseSize(spec.ServerType)
		if err != nil {
			return provider.Plan{}, d.invalid(err.Error())
		}
		if _, err := imageRef(spec.Image); err != nil {
			return provider.Plan{}, d.invalid(err.Error())
		}
		params, err := json.Marshal(createParams{
			Name: "fp-" + req.ResourceID, Spec: *spec, CPU: cpu, MemMiB: mem, Labels: req.Desired.Labels,
		})
		if err != nil {
			return provider.Plan{}, err
		}
		return provider.Plan{
			Actions: []provider.Action{{Kind: "create", ResourceID: req.ResourceID, Params: params}},
			Summary: []string{fmt.Sprintf("create container fp-%s (%s, %s)", req.ResourceID, spec.ServerType, spec.Image)},
		}, nil
	case req.Desired == nil && req.Observed != nil:
		return provider.Plan{
			Actions: []provider.Action{{Kind: "delete", ResourceID: req.ResourceID, Ref: &req.Observed.Ref, Destructive: true}},
			Summary: []string{"remove container " + req.Observed.Ref.ID},
		}, nil
	default:
		return provider.Plan{}, nil
	}
}

// parseSize reads "<cpu>x<memMiB>" (e.g. "2x1024"); a bare "<cpu>" means
// no memory limit.
func parseSize(serverType string) (cpu, memMiB int64, err error) {
	c, m, _ := strings.Cut(serverType, "x")
	cpu, err = strconv.ParseInt(c, 10, 64)
	if err != nil || cpu <= 0 {
		return 0, 0, fmt.Errorf(`docker serverType must be "<cpu>x<memMiB>" (e.g. "2x1024"), got %q`, serverType)
	}
	if m != "" {
		memMiB, err = strconv.ParseInt(m, 10, 64)
		if err != nil || memMiB < 6 {
			return 0, 0, fmt.Errorf(`docker serverType memory must be an integer MiB >= 6, got %q`, serverType)
		}
	}
	return cpu, memMiB, nil
}

// imageRef accepts "name:<ref>" (the cross-provider image form) or a bare
// Docker reference.
func imageRef(image string) (string, error) {
	ref := strings.TrimPrefix(image, "name:")
	if ref == "" || strings.HasPrefix(ref, "snapshot:") || strings.HasPrefix(ref, "id:") {
		return "", fmt.Errorf(`docker image must be "name:<repo:tag>" or a bare reference, got %q`, image)
	}
	return ref, nil
}

type opData struct {
	V    int    `json:"v"`
	Kind string `json:"kind"` // create | delete | stop | start
}

func (d *Docker) Apply(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	switch action.Kind {
	case "create":
		return d.applyCreate(ctx, action)
	case "delete":
		return d.applyDelete(ctx, action)
	case "stop", "start":
		return d.applyPower(ctx, action)
	default:
		return provider.OperationRef{}, d.invalid("unknown action kind " + action.Kind)
	}
}

func (d *Docker) opRef(action provider.Action, ref *provider.ExternalRef) provider.OperationRef {
	data, _ := json.Marshal(opData{V: 1, Kind: action.Kind})
	return provider.OperationRef{ActionID: action.ActionID, Ref: ref, Data: data}
}

func (d *Docker) applyCreate(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	var p createParams
	if err := json.Unmarshal(action.Params, &p); err != nil {
		return provider.OperationRef{}, d.invalid("malformed create params: " + err.Error())
	}
	// Op-label dedup (invariant 7): a crash between create and start leaves
	// a `created` container that ObserveOperation adopts and starts.
	if existing, err := d.Discover(ctx, provider.DiscoverRequest{
		Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelOp: action.ActionID},
	}); err == nil && len(existing) > 0 {
		return d.opRef(action, &existing[0].Ref), nil
	}
	image, err := imageRef(p.Spec.Image)
	if err != nil {
		return provider.OperationRef{}, d.invalid(err.Error())
	}
	labels := map[string]string{}
	for k, v := range p.Spec.Labels {
		labels[k] = v
	}
	for k, v := range p.Labels {
		labels[k] = v
	}
	init := true
	hostCfg := map[string]any{"Init": &init, "NanoCpus": p.CPU * 1e9, "Memory": p.MemMiB << 20}
	if d.network != "" {
		hostCfg["NetworkMode"] = d.network
	}
	body := map[string]any{"Image": image, "Cmd": d.command, "Labels": labels, "HostConfig": hostCfg}
	if p.Spec.UserData != "" {
		body["Env"] = []string{"FLEETPLANE_USER_DATA=" + p.Spec.UserData}
	}
	path := "/containers/create?name=" + url.QueryEscape(p.Name)
	code, resp, err := d.do(ctx, http.MethodPost, path, body, provider.EffectMaybe)
	if provider.IsClass(err, provider.ErrNotFound) {
		// Missing image: pull once, then retry the create.
		if perr := d.pull(ctx, image); perr != nil {
			return provider.OperationRef{}, perr
		}
		code, resp, err = d.do(ctx, http.MethodPost, path, body, provider.EffectMaybe)
	}
	if err != nil {
		return provider.OperationRef{}, err
	}
	_ = code
	var created struct{ Id string }
	if err := json.Unmarshal(resp, &created); err != nil || created.Id == "" {
		return provider.OperationRef{}, d.retryable(fmt.Errorf("create response: %v", err))
	}
	ref := &provider.ExternalRef{ID: created.Id}
	// Start failures are not fatal here: ObserveOperation re-issues start
	// on `created` containers, so a transient error just delays success.
	_, _, _ = d.do(ctx, http.MethodPost, "/containers/"+created.Id+"/start", nil, provider.EffectMaybe)
	return d.opRef(action, ref), nil
}

func (d *Docker) pull(ctx context.Context, image string) error {
	ref := image
	if !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		ref += ":latest"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/images/create?fromImage="+url.QueryEscape(ref), nil)
	if err != nil {
		return err
	}
	res, err := d.http.Do(req)
	if err != nil {
		return d.retryable(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(res.Body)
		return d.mapStatus(res.StatusCode, string(msg), provider.EffectNone)
	}
	// Progress is a JSON-lines stream; a failed pull reports {"error":...}.
	dec := json.NewDecoder(res.Body)
	for {
		var line struct{ Error string }
		if err := dec.Decode(&line); err == io.EOF {
			return nil
		} else if err != nil {
			return d.retryable(err)
		}
		if line.Error != "" {
			return &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone,
				Provider: d.instance, Message: "image pull " + ref + ": " + line.Error}
		}
	}
}

func (d *Docker) applyDelete(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	if action.Ref == nil {
		return provider.OperationRef{}, d.invalid("delete requires a ref")
	}
	_, _, err := d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(action.Ref.ID)+"?force=true&v=true", nil, provider.EffectMaybe)
	if err != nil && !provider.IsClass(err, provider.ErrNotFound) { // delete-of-deleted is success (FI-7)
		return provider.OperationRef{}, err
	}
	return d.opRef(action, action.Ref), nil
}

// applyPower issues stop/start. Docker answers 304 when already in the
// target state — exactly the idempotency docs/12 §7 requires.
func (d *Docker) applyPower(ctx context.Context, action provider.Action) (provider.OperationRef, error) {
	if action.Ref == nil {
		return provider.OperationRef{}, d.invalid(action.Kind + " requires a ref")
	}
	path := "/containers/" + url.PathEscape(action.Ref.ID) + "/" + action.Kind
	if action.Kind == "stop" {
		path += "?t=5"
	}
	if _, _, err := d.do(ctx, http.MethodPost, path, nil, provider.EffectMaybe); err != nil {
		return provider.OperationRef{}, err
	}
	return d.opRef(action, action.Ref), nil
}

func (d *Docker) ObserveOperation(ctx context.Context, op provider.OperationRef) (provider.OperationStatus, error) {
	if op.Ref == nil {
		return provider.OperationStatus{State: provider.OpUnknown}, nil
	}
	var data opData
	if len(op.Data) > 0 {
		_ = json.Unmarshal(op.Data, &data)
	}
	obs, err := d.Get(ctx, *op.Ref)
	if err != nil {
		return provider.OperationStatus{}, err // not_found: the engine interprets by op kind
	}
	running := provider.OperationStatus{State: provider.OpRunning, Ref: op.Ref, RetryAfter: time.Second}
	switch data.Kind {
	case "delete":
		return running, nil
	case "stop":
		if obs.Phase == provider.PhaseStopped {
			return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
		}
		return running, nil
	default: // create | start: success only on observed running
		switch obs.ProviderState {
		case "running":
			return provider.OperationStatus{State: provider.OpSucceeded, Ref: op.Ref, Resource: &obs}, nil
		case "created":
			// Crash between create and start, or start not yet issued.
			_, _, _ = d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(op.Ref.ID)+"/start", nil, provider.EffectMaybe)
			return running, nil
		case "exited", "dead":
			if data.Kind == "start" {
				return running, nil // start is in flight or failed transiently; the engine's attempt cap bounds this
			}
			var st struct {
				State struct{ ExitCode int }
			}
			_ = json.Unmarshal(obs.Extensions, &st)
			return provider.OperationStatus{State: provider.OpFailed, Ref: op.Ref, Failure: &provider.Error{
				Class: provider.ErrTerminal, SideEffect: provider.EffectMaybe, Provider: d.instance,
				Message: fmt.Sprintf("container exited (code %d) before becoming ready", st.State.ExitCode),
			}}, nil
		default:
			return running, nil
		}
	}
}

// --- mapping ---

type inspect struct {
	Id     string
	Name   string
	Config struct{ Labels map[string]string }
	State  struct {
		Status   string
		ExitCode int
	}
	HostConfig struct {
		NanoCpus int64
		Memory   int64
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

func (d *Docker) observe(raw []byte) (provider.ObservedResource, error) {
	var c inspect
	if err := json.Unmarshal(raw, &c); err != nil {
		return provider.ObservedResource{}, d.retryable(err)
	}
	var ph provider.ObservedPhase
	switch c.State.Status {
	case "created", "restarting":
		ph = provider.PhasePending
	case "running":
		ph = provider.PhaseRunning
	case "exited", "paused":
		ph = provider.PhaseStopped
	case "removing":
		ph = provider.PhaseDeleting
	default:
		ph = provider.PhaseUnknown
	}
	var addrs []provider.Address
	for _, n := range c.NetworkSettings.Networks {
		if n.IPAddress != "" {
			addrs = append(addrs, provider.Address{Network: "private", Addr: n.IPAddress})
		}
	}
	labels := c.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return provider.ObservedResource{
		Ref:           provider.ExternalRef{ID: c.Id},
		Kind:          compute.Kind,
		FleetplaneID:  labels[provider.LabelID],
		CreateOpID:    labels[provider.LabelOp],
		Owned:         labels[provider.LabelManaged] == "true" && labels[provider.LabelOwner] == d.ownerID,
		Phase:         ph,
		ProviderState: c.State.Status,
		Capacity: provider.Capacity{
			compute.DimCPU:       c.HostConfig.NanoCpus / 1e9,
			compute.DimMemoryMiB: c.HostConfig.Memory >> 20,
		},
		Addresses:  addrs,
		Labels:     labels,
		Extensions: json.RawMessage(raw), // full inspect object (invariant 6)
		ObservedAt: time.Now(),
	}, nil
}

// --- transport ---

// do performs one Engine API call. 304 (already in target state) is success
// with an empty body; error statuses map to typed provider errors.
func (d *Docker) do(ctx context.Context, method, path string, body any, effectIfSent provider.SideEffect) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.http.Do(req)
	if err != nil {
		if method == http.MethodGet {
			effectIfSent = provider.EffectNone
		}
		return 0, nil, &provider.Error{Class: provider.ErrRetryable, SideEffect: effectIfSent,
			Provider: d.instance, Message: err.Error(), Cause: err}
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, nil, d.retryable(err)
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotModified {
		var msg struct{ Message string }
		_ = json.Unmarshal(out, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(out))
		}
		return res.StatusCode, nil, d.mapStatus(res.StatusCode, msg.Message, effectIfSent)
	}
	return res.StatusCode, out, nil
}

func (d *Docker) mapStatus(code int, msg string, effectIfSent provider.SideEffect) error {
	e := &provider.Error{Provider: d.instance, Message: msg, Code: strconv.Itoa(code)}
	switch code {
	case http.StatusNotFound:
		e.Class, e.SideEffect = provider.ErrNotFound, provider.EffectNone
	case http.StatusConflict:
		e.Class, e.SideEffect = provider.ErrConflict, effectIfSent
	case http.StatusBadRequest, http.StatusForbidden:
		e.Class, e.SideEffect = provider.ErrInvalid, provider.EffectNone
	default:
		e.Class, e.SideEffect = provider.ErrRetryable, effectIfSent
	}
	return e
}

func (d *Docker) invalid(msg string) error {
	return &provider.Error{Class: provider.ErrInvalid, SideEffect: provider.EffectNone, Provider: d.instance, Message: msg}
}

func (d *Docker) retryable(err error) error {
	return &provider.Error{Class: provider.ErrRetryable, SideEffect: provider.EffectNone, Provider: d.instance, Message: err.Error(), Cause: err}
}
