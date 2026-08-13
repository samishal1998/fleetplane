package api

import (
	"context"
	"net/http"
	"strings"
)

// RouteDef is one row of the single source of truth (ADR-005): it drives
// mux registration, per-route permissions, and the OpenAPI-diff test.
type RouteDef struct {
	Method   string
	Pattern  string // ServeMux pattern
	Verb     string // colon verb ("drain", "reconcile"); "" for plain routes
	SpecPath string // OpenAPI path (e.g. "/v1/resources/{id}:drain")
	Perm     Permission
	Mutating bool
	handler  http.HandlerFunc
}

// Routes returns the route table (exported for the OpenAPI contract test).
func (s *Server) Routes() []RouteDef {
	return []RouteDef{
		{Method: "POST", Pattern: "/v1/acquisitions", SpecPath: "/v1/acquisitions", Perm: PermResourceAcquire, Mutating: true, handler: s.createAcquisition},
		{Method: "GET", Pattern: "/v1/acquisitions/{id}", SpecPath: "/v1/acquisitions/{id}", Perm: PermResourceRead, handler: s.getAcquisition},
		{Method: "DELETE", Pattern: "/v1/acquisitions/{id}", SpecPath: "/v1/acquisitions/{id}", Perm: PermResourceAcquire, Mutating: true, handler: s.releaseAcquisition},

		{Method: "POST", Pattern: "/v1/resources", SpecPath: "/v1/resources", Perm: PermResourceCreate, Mutating: true, handler: s.createResource},
		{Method: "GET", Pattern: "/v1/resources", SpecPath: "/v1/resources", Perm: PermResourceRead, handler: s.listResources},
		{Method: "GET", Pattern: "/v1/resources/{id}", SpecPath: "/v1/resources/{id}", Perm: PermResourceRead, handler: s.getResource},
		{Method: "DELETE", Pattern: "/v1/resources/{id}", SpecPath: "/v1/resources/{id}", Perm: PermResourceDelete, Mutating: true, handler: s.deleteResource},
		{Method: "POST", Pattern: "/v1/resources/{idverb}", Verb: "drain", SpecPath: "/v1/resources/{id}:drain", Perm: PermResourceDelete, Mutating: true, handler: s.drainResource},

		{Method: "POST", Pattern: "/v1/pools", SpecPath: "/v1/pools", Perm: PermPoolWrite, Mutating: true, handler: s.createPool},
		{Method: "GET", Pattern: "/v1/pools", SpecPath: "/v1/pools", Perm: PermPoolRead, handler: s.listPools},
		{Method: "GET", Pattern: "/v1/pools/{id}", SpecPath: "/v1/pools/{id}", Perm: PermPoolRead, handler: s.getPool},
		{Method: "PUT", Pattern: "/v1/pools/{id}", SpecPath: "/v1/pools/{id}", Perm: PermPoolWrite, Mutating: true, handler: s.updatePool},
		{Method: "POST", Pattern: "/v1/pools/{idverb}", Verb: "reconcile", SpecPath: "/v1/pools/{id}:reconcile", Perm: PermPoolWrite, Mutating: true, handler: s.reconcilePool},

		{Method: "GET", Pattern: "/v1/operations", SpecPath: "/v1/operations", Perm: PermOperationRead, handler: s.listOperations},
		{Method: "GET", Pattern: "/v1/operations/{id}", SpecPath: "/v1/operations/{id}", Perm: PermOperationRead, handler: s.getOperation},
		{Method: "GET", Pattern: "/v1/events", SpecPath: "/v1/events", Perm: PermOperationRead, handler: s.listEvents},
		{Method: "GET", Pattern: "/v1/providers", SpecPath: "/v1/providers", Perm: PermProviderRead, handler: s.listProviders},
	}
}

// Handler builds the mux from the route table with the middleware chain:
// recover → request-id → authenticate → authorize → handler. (The mutation
// gate wraps outside, in boot.)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	byPattern := map[string][]RouteDef{}
	for _, rt := range s.Routes() {
		key := rt.Method + " " + rt.Pattern
		byPattern[key] = append(byPattern[key], rt)
	}
	for key, defs := range byPattern {
		mux.HandleFunc(key, s.dispatch(defs))
	}
	return mux
}

// dispatch resolves colon verbs (ADR-005): ServeMux wildcards span whole
// segments, so "res_x:drain" matches {idverb} and is split at the LAST
// colon (IDs never contain one).
func (s *Server) dispatch(defs []RouteDef) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var def *RouteDef
		if defs[0].Verb == "" {
			def = &defs[0]
		} else {
			idverb := r.PathValue("idverb")
			id, verb, ok := splitVerb(idverb)
			if ok {
				for i := range defs {
					if defs[i].Verb == verb {
						def = &defs[i]
						r.SetPathValue("id", id)
						break
					}
				}
			}
			if def == nil {
				s.withMiddleware(RouteDef{Perm: PermResourceRead}, func(w http.ResponseWriter, r *http.Request) {
					writeError(w, w.Header().Get("X-Request-Id"), http.StatusNotFound, "not_found", "unknown action", false)
				})(w, r)
				return
			}
		}
		s.withMiddleware(*def, def.handler)(w, r)
	}
}

func splitVerb(idverb string) (id, verb string, ok bool) {
	i := strings.LastIndexByte(idverb, ':')
	if i <= 0 || i == len(idverb)-1 {
		return "", "", false
	}
	return idverb[:i], idverb[i+1:], true
}

func (s *Server) withMiddleware(def RouteDef, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = "req_" + randHex(8)
		}
		w.Header().Set("X-Request-Id", reqID)
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic in handler", "panic", p, "request_id", reqID,
					"route", def.Method+" "+def.SpecPath)
				writeError(w, reqID, http.StatusInternalServerError, "internal", "internal error", false)
			}
		}()

		if s.auth.Enabled() {
			principal, err := s.auth.Authenticate(r.Header.Get("Authorization"))
			if err != nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, reqID, http.StatusUnauthorized, "unauthenticated", err.Error(), false)
				return
			}
			if !principal.Perms.Allows(def.Perm) {
				writeError(w, reqID, http.StatusForbidden, "permission_denied",
					"token "+principal.Name+" lacks "+string(def.Perm), false)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal))
		}
		h(w, r)
	}
}
