// Package api serves the Fleetplane HTTP API (04 §3 + ADR-API-001). A
// single RouteDef table (routes.go) drives the mux, per-route permissions
// and the OpenAPI contract test. Handlers call only app.Service.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/provision"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

type Server struct {
	app    *app.Service
	auth   *TokenAuthenticator
	health HealthSource
	log    *slog.Logger
}

// HealthSource surfaces provider health (07 §8: NEVER via readiness).
type HealthSource interface {
	Snapshot() []app.ProviderHealth
}

func New(a *app.Service, auth *TokenAuthenticator, health HealthSource, log *slog.Logger) *Server {
	return &Server{app: a, auth: auth, health: health, log: log}
}

// --- resource handlers ---

func (s *Server) createResource(w http.ResponseWriter, r *http.Request) {
	reqID := w.Header().Get("X-Request-Id")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "request body too large or unreadable", false)
		return
	}
	var req apiclient.CreateResourceRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "malformed request: "+err.Error(), false)
		return
	}

	actor := principalOf(r).Name
	sum := sha256.Sum256(body)
	out, err := s.app.CreateResource(r.Context(), app.CreateResourceCmd{
		Kind:     req.Spec.Kind,
		Provider: req.Spec.Provider,
		Name:     req.Metadata.Name,
		Class:    req.Spec.Class,
		Spec:     req.Spec.Machine,
		Labels:   req.Metadata.Labels,

		Actor:       actor,
		IdemKey:     r.Header.Get("Idempotency-Key"),
		IdemScope:   "POST /v1/resources|" + actor,
		RequestHash: hex.EncodeToString(sum[:]),

		BuildResponse: func(res *storage.Resource) (int, json.RawMessage) {
			b, _ := json.Marshal(toEnvelope(res))
			return http.StatusCreated, b
		},
	})
	if err != nil {
		s.writeAppError(w, reqID, err)
		return
	}
	writeOutcome(w, out)
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request) {
	res, err := s.app.GetResource(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, toEnvelope(res))
}

func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := storage.ResourceFilter{
		Kind:  q.Get("kind"),
		Class: q.Get("class"),
	}
	if p := q.Get("provider"); p != "" {
		f.Provider = storage.ProviderInstance(p)
	}
	if p := q.Get("pool"); p != "" {
		pid := storage.PoolID(p)
		f.PoolID = &pid
	}
	for _, p := range q["phase"] {
		ph := phase.Phase(p)
		if !phase.Valid(ph) {
			writeError(w, w.Header().Get("X-Request-Id"), http.StatusBadRequest, "invalid",
				"unknown phase "+p, false)
			return
		}
		f.Phases = append(f.Phases, ph)
	}
	items, err := s.app.ListResources(r.Context(), f)
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	list := apiclient.ResourceList{APIVersion: apiclient.APIVersion, Kind: "ResourceList", Items: []apiclient.Resource{}}
	for _, res := range items {
		list.Items = append(list.Items, toEnvelope(res))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) deleteResource(w http.ResponseWriter, r *http.Request) {
	reqID := w.Header().Get("X-Request-Id")
	actor := principalOf(r).Name
	id := r.PathValue("id")
	sum := sha256.Sum256([]byte("DELETE /v1/resources/" + id))
	dryRun := r.URL.Query().Get("dryRun") == "true"
	out, err := s.app.DeleteResource(r.Context(), app.DeleteResourceCmd{
		ID: id, Actor: actor, DryRun: dryRun,
		IdemKey: r.Header.Get("Idempotency-Key"), IdemScope: "DELETE /v1/resources|" + actor,
		RequestHash: hex.EncodeToString(sum[:]),
		BuildResponse: func(res *storage.Resource) (int, json.RawMessage) {
			if dryRun {
				b, _ := json.Marshal(map[string]any{"id": string(res.ID), "wouldDelete": true, "dryRun": true})
				return http.StatusOK, b
			}
			b, _ := json.Marshal(map[string]string{"id": string(res.ID), "status": "deleting"})
			return http.StatusAccepted, b
		},
	})
	if err != nil {
		s.writeAppError(w, reqID, err)
		return
	}
	writeOutcome(w, out)
}

func (s *Server) drainResource(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DrainResource(r.Context(), r.PathValue("id"), principalOf(r).Name); err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": r.PathValue("id"), "status": "draining"})
}

func (s *Server) parkResource(w http.ResponseWriter, r *http.Request) {
	s.parkStart(w, r, s.app.ParkResource, "parking")
}

func (s *Server) startResource(w http.ResponseWriter, r *http.Request) {
	s.parkStart(w, r, s.app.StartResource, "starting")
}

func (s *Server) undrainResource(w http.ResponseWriter, r *http.Request) {
	s.syncVerb(w, r, s.app.UndrainResource, "ready")
}

func (s *Server) protectResource(w http.ResponseWriter, r *http.Request) {
	s.syncVerb(w, r, s.setProtected(true), "protected")
}

func (s *Server) unprotectResource(w http.ResponseWriter, r *http.Request) {
	s.syncVerb(w, r, s.setProtected(false), "unprotected")
}

func (s *Server) setProtected(on bool) func(context.Context, string, string) error {
	return func(ctx context.Context, id, actor string) error {
		return s.app.SetResourceProtected(ctx, id, on, actor)
	}
}

// syncVerb serves the verbs that finish inside the request. Unlike the
// journalled ones there is nothing left in flight when the call returns, so
// the status is 200 rather than 202. ErrAlreadyThere is 200 too: asking for
// a state the object is already in is success, not a conflict.
func (s *Server) syncVerb(w http.ResponseWriter, r *http.Request, act func(context.Context, string, string) error, status string) {
	id := r.PathValue("id")
	if err := act(r.Context(), id, principalOf(r).Name); err != nil && !errors.Is(err, app.ErrAlreadyThere) {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": status})
}

// parkStart implements the idempotent status matrix (docs/12): 202 on a
// fresh journal, 200 when already in/entering the requested state (a
// retried request whose response was lost must not see a conflict), 409
// otherwise.
func (s *Server) parkStart(w http.ResponseWriter, r *http.Request, act func(context.Context, string, string) error, status string) {
	id := r.PathValue("id")
	err := act(r.Context(), id, principalOf(r).Name)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": status})
	case errors.Is(err, app.ErrAlreadyThere):
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": status})
	case errors.Is(err, provision.ErrParkUnsupported):
		writeError(w, w.Header().Get("X-Request-Id"), http.StatusConflict, "park_unsupported", err.Error(), false)
	default:
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
	}
}

// --- serialization ---

func toEnvelope(r *storage.Resource) apiclient.Resource {
	env := apiclient.Resource{
		APIVersion: apiclient.APIVersion,
		Kind:       "Resource",
		Metadata: apiclient.Metadata{
			ID: string(r.ID), Name: r.Name, Labels: r.Labels,
			Ownership: string(r.Ownership), Protected: r.DeleteProtected,
			Generation: r.Generation, ObservedGeneration: r.ObservedGeneration,
			CreatedAt: time.UnixMilli(r.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
		},
		Spec: apiclient.ResourceSpec{
			Kind: r.Kind, Provider: string(r.Provider), Class: r.Class, Machine: r.Spec,
		},
		Status: apiclient.ResourceStatus{
			Phase:       string(r.Phase),
			ExternalRef: r.ExternalRef,
			Capacity:    r.Capacity,
			Extensions:  r.Extension, // provider-native, untouched (invariant 6)
		},
	}
	if r.ExternalID != nil {
		env.Status.ExternalID = *r.ExternalID
	}
	if r.ParkedAt != nil {
		t := time.UnixMilli(*r.ParkedAt).UTC()
		env.Status.ParkedAt = &t
	}
	if r.DeletedAt != nil {
		t := time.UnixMilli(*r.DeletedAt).UTC()
		env.Metadata.DeletedAt = &t
	}
	return env
}

func (s *Server) writeAppError(w http.ResponseWriter, reqID string, err error) {
	var ve *app.ValidationError
	var ife *app.InFlightError
	switch {
	case errors.As(err, &ve):
		writeError(w, reqID, http.StatusBadRequest, "invalid", ve.Msg, false)
	case errors.As(err, &ife):
		details, _ := json.Marshal(map[string]string{"operationId": string(ife.OperationID)})
		writeErrorDetails(w, reqID, http.StatusConflict, "idempotency_in_flight",
			"the original request for this idempotency key is still running; poll the operation", false, details)
	case errors.Is(err, app.ErrIdemMismatch):
		writeError(w, reqID, http.StatusConflict, "idempotency_mismatch", err.Error(), false)
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, reqID, http.StatusNotFound, "not_found", err.Error(), false)
	case errors.Is(err, storage.ErrConflict):
		writeError(w, reqID, http.StatusConflict, "conflict", err.Error(), false)
	default:
		s.log.Error("request failed", "error", err, "request_id", reqID)
		writeError(w, reqID, http.StatusInternalServerError, "internal", "internal error", true)
	}
}

func writeOutcome(w http.ResponseWriter, out app.Outcome) {
	if out.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body) // stored bytes verbatim (invariant 2)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, reqID string, code int, errCode, msg string, retryable bool) {
	writeErrorDetails(w, reqID, code, errCode, msg, retryable, nil)
}

func writeErrorDetails(w http.ResponseWriter, reqID string, code int, errCode, msg string, retryable bool, details json.RawMessage) {
	writeJSON(w, code, apiclient.ErrorBody{Error: apiclient.ErrorDetail{
		Code: errCode, Message: msg, RequestID: reqID, Retryable: retryable, Details: details,
	}})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
