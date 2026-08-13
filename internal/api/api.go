// Package api serves the Fleetplane HTTP API (04 §3). Handlers call only
// app.Service; auth/authz middleware and the full route table land at I9.
package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/samimishal/fleetplane/internal/app"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/apiclient"
)

type Server struct {
	app *app.Service
	log *slog.Logger
}

func New(a *app.Service, log *slog.Logger) *Server { return &Server{app: a, log: log} }

// Handler returns the API routes, ready to mount on the main listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/resources", s.withRecovery(s.createResource))
	mux.HandleFunc("GET /v1/resources", s.withRecovery(s.listResources))
	mux.HandleFunc("GET /v1/resources/{id}", s.withRecovery(s.getResource))
	mux.HandleFunc("DELETE /v1/resources/{id}", s.withRecovery(s.deleteResource))
	return mux
}

func (s *Server) withRecovery(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = "req_" + randHex(8)
		}
		w.Header().Set("X-Request-Id", reqID)
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic in handler", "panic", fmt.Sprint(p), "request_id", reqID)
				writeError(w, reqID, http.StatusInternalServerError, "internal", "internal error", false)
			}
		}()
		h(w, r)
	}
}

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

	actor := actorOf(r)
	idemKey := r.Header.Get("Idempotency-Key")
	sum := sha256.Sum256(body)

	out, err := s.app.CreateResource(r.Context(), app.CreateResourceCmd{
		Kind:     req.Spec.Kind,
		Provider: req.Spec.Provider,
		Name:     req.Metadata.Name,
		Spec:     req.Spec.Machine,
		Labels:   req.Metadata.Labels,

		Actor:       actor,
		IdemKey:     idemKey,
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
	f := storage.ResourceFilter{
		Kind:  r.URL.Query().Get("kind"),
		Class: r.URL.Query().Get("class"),
	}
	if p := r.URL.Query().Get("provider"); p != "" {
		f.Provider = storage.ProviderInstance(p)
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
	actor := actorOf(r)
	idemKey := r.Header.Get("Idempotency-Key")
	id := r.PathValue("id")
	sum := sha256.Sum256([]byte("DELETE /v1/resources/" + id))

	out, err := s.app.DeleteResource(r.Context(), app.DeleteResourceCmd{
		ID: id, Actor: actor,
		IdemKey: idemKey, IdemScope: "DELETE /v1/resources|" + actor,
		RequestHash: hex.EncodeToString(sum[:]),
		BuildResponse: func(res *storage.Resource) (int, json.RawMessage) {
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

func actorOf(*http.Request) string { return "-" } // token auth lands at I9

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
