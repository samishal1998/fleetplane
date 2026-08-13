package api

// Handlers for acquisitions, pools, operations, events and providers.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/samimishal/fleetplane/internal/app"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/apiclient"
)

// --- acquisitions (04 §4) ---

func (s *Server) createAcquisition(w http.ResponseWriter, r *http.Request) {
	reqID := w.Header().Get("X-Request-Id")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "request body too large or unreadable", false)
		return
	}
	var req apiclient.AcquireRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "malformed request: "+err.Error(), false)
		return
	}
	var ttl time.Duration
	if req.Lease != nil && req.Lease.TTL != "" {
		ttl, err = time.ParseDuration(req.Lease.TTL)
		if err != nil {
			writeError(w, reqID, http.StatusBadRequest, "invalid", "lease.ttl: "+err.Error(), false)
			return
		}
	}
	// The body's idempotencyKey (04 §4) and the header must agree.
	idemKey := r.Header.Get("Idempotency-Key")
	if req.IdempotencyKey != "" {
		if idemKey != "" && idemKey != req.IdempotencyKey {
			writeError(w, reqID, http.StatusBadRequest, "invalid", "Idempotency-Key header and body idempotencyKey disagree", false)
			return
		}
		idemKey = req.IdempotencyKey
	}

	actor := principalOf(r).Name
	sum := sha256.Sum256(body)
	out, err := s.app.Acquire(r.Context(), app.AcquireCmd{
		Kind: req.Kind, Class: req.Class,
		Constraints: req.Constraints, Exclusive: req.Exclusive,
		Quantity: req.Quantity, TTL: ttl,
		Actor: actor, IdemKey: idemKey,
		IdemScope:   "POST /v1/acquisitions|" + actor,
		RequestHash: hex.EncodeToString(sum[:]),
		BuildResponse: func(a *storage.Acquisition) (int, json.RawMessage) {
			b, _ := json.Marshal(toAcqEnvelope(a))
			return http.StatusCreated, b
		},
	})
	if err != nil {
		s.writeAppError(w, reqID, err)
		return
	}
	writeOutcome(w, out)
}

func (s *Server) getAcquisition(w http.ResponseWriter, r *http.Request) {
	acq, err := s.app.GetAcquisition(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, toAcqEnvelope(acq))
}

func (s *Server) releaseAcquisition(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Release(r.Context(), r.PathValue("id"), principalOf(r).Name); err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id"), "state": "released"})
}

func toAcqEnvelope(a *storage.Acquisition) apiclient.Acquisition {
	env := apiclient.Acquisition{
		APIVersion: apiclient.APIVersion, Kind: "Acquisition",
		ID: string(a.ID), State: string(a.State), Class: a.Class,
		ResourceKind: a.Kind, Actor: a.Actor,
		CreatedAt: time.UnixMilli(a.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(a.UpdatedAt).UTC(),
	}
	if a.LeaseID != nil {
		env.LeaseID = string(*a.LeaseID)
	}
	if a.PendingResourceID != nil {
		env.ResourceID = string(*a.PendingResourceID)
	}
	return env
}

// --- pools (04 §5) ---

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	s.upsertPool(w, r, "")
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) {
	s.upsertPool(w, r, r.PathValue("id"))
}

func (s *Server) upsertPool(w http.ResponseWriter, r *http.Request, id string) {
	reqID := w.Header().Get("X-Request-Id")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "request body too large or unreadable", false)
		return
	}
	var req apiclient.PoolManifest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "malformed pool: "+err.Error(), false)
		return
	}
	pool, err := s.app.UpsertPool(r.Context(), app.UpsertPoolCmd{
		ID: id, Name: req.Metadata.Name, Spec: req.Spec, Actor: principalOf(r).Name,
	})
	if err != nil {
		s.writeAppError(w, reqID, err)
		return
	}
	writeJSON(w, http.StatusOK, toPoolEnvelope(pool))
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request) {
	pool, err := s.app.GetPool(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, toPoolEnvelope(pool))
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.app.ListPools(r.Context())
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	out := apiclient.PoolList{APIVersion: apiclient.APIVersion, Kind: "PoolList", Items: []apiclient.Pool{}}
	for _, p := range pools {
		out.Items = append(out.Items, toPoolEnvelope(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) reconcilePool(w http.ResponseWriter, r *http.Request) {
	if err := s.app.ReconcilePool(r.Context(), r.PathValue("id")); err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": r.PathValue("id"), "status": "reconciling"})
}

func toPoolEnvelope(p *storage.Pool) apiclient.Pool {
	return apiclient.Pool{
		APIVersion: apiclient.APIVersion, Kind: "Pool",
		Metadata: apiclient.Metadata{
			ID: string(p.ID), Name: p.Name,
			Generation: p.Generation, ObservedGeneration: p.ObservedGeneration,
			CreatedAt: time.UnixMilli(p.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(p.UpdatedAt).UTC(),
		},
		Spec:   p.Spec,
		Paused: p.Paused,
	}
}

// --- operations / events / providers ---

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.app.GetOperation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, toOpEnvelope(op))
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := s.app.ListOperations(r.Context())
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	out := []apiclient.Operation{}
	for _, op := range ops {
		out = append(out, toOpEnvelope(op))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiVersion": apiclient.APIVersion, "kind": "OperationList", "items": out})
}

func toOpEnvelope(op *storage.Operation) apiclient.Operation {
	env := apiclient.Operation{
		ID: string(op.ID), Kind: string(op.Kind), State: string(op.State),
		Provider: string(op.Provider), Attempt: op.Attempt,
		CreatedAt: time.UnixMilli(op.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(op.UpdatedAt).UTC(),
	}
	if op.ResourceID != nil {
		env.ResourceID = string(*op.ResourceID)
	}
	if op.ErrorClass != nil {
		env.ErrorClass = *op.ErrorClass
	}
	return env
}

// resolveOperation thaws an uncertain operation (plan R23; ADR-API-001).
func (s *Server) resolveOperation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"` // retry-verification | mark-failed
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, w.Header().Get("X-Request-Id"), http.StatusBadRequest, "invalid", "body: "+err.Error(), false)
		return
	}
	if err := s.app.ResolveOperation(r.Context(), r.PathValue("id"), req.Action, principalOf(r).Name); err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": r.PathValue("id"), "action": req.Action})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	f := storage.EventFilter{After: storage.EventID(r.URL.Query().Get("after"))}
	if since := r.URL.Query().Get("since"); since != "" {
		d, err := time.ParseDuration(since)
		if err != nil {
			writeError(w, w.Header().Get("X-Request-Id"), http.StatusBadRequest, "invalid", "since: "+err.Error(), false)
			return
		}
		f.SinceTS = time.Now().Add(-d).UnixMilli()
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	events, err := s.app.ListEvents(r.Context(), f, limit)
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	out := []apiclient.Event{}
	for _, ev := range events {
		e := apiclient.Event{
			ID: string(ev.ID), TS: time.UnixMilli(ev.TS).UTC(), Type: ev.Type,
			Actor: ev.Actor, Outcome: ev.Outcome, Provider: string(ev.Provider),
		}
		if ev.ResourceID != nil {
			e.ResourceID = string(*ev.ResourceID)
		}
		if ev.OperationID != nil {
			e.OperationID = string(*ev.OperationID)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiVersion": apiclient.APIVersion, "kind": "EventList", "items": out})
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	out := []app.ProviderHealth{}
	if s.health != nil {
		out = s.health.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiVersion": apiclient.APIVersion, "kind": "ProviderList", "items": out})
}
