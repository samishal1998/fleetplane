package api

// Class handlers (dynamic classes): config-sourced classes are read-only
// through this API; api-sourced ones are fully managed here.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func (s *Server) createClass(w http.ResponseWriter, r *http.Request) {
	s.upsertClass(w, r, "", true)
}

func (s *Server) updateClass(w http.ResponseWriter, r *http.Request) {
	s.upsertClass(w, r, r.PathValue("name"), false)
}

func (s *Server) upsertClass(w http.ResponseWriter, r *http.Request, name string, mustCreate bool) {
	reqID := w.Header().Get("X-Request-Id")
	var req apiclient.ClassManifest
	dec := json.NewDecoder(bytes.NewReader(readBody(w, r)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "malformed class: "+err.Error(), false)
		return
	}
	if name == "" {
		name = req.Metadata.Name
	} else if req.Metadata.Name != "" && req.Metadata.Name != name {
		writeError(w, reqID, http.StatusBadRequest, "invalid", "metadata.name disagrees with the URL", false)
		return
	}
	var reclaim, reclaimDel, maxWait time.Duration
	var park string
	var err error
	if r := req.Spec.Reclaim; r != nil {
		if r.IdleAfter != "" {
			if reclaim, err = time.ParseDuration(r.IdleAfter); err != nil {
				writeError(w, reqID, http.StatusBadRequest, "invalid", "reclaim.idleAfter: "+err.Error(), false)
				return
			}
		}
		if r.DeleteAfter != "" {
			if reclaimDel, err = time.ParseDuration(r.DeleteAfter); err != nil {
				writeError(w, reqID, http.StatusBadRequest, "invalid", "reclaim.deleteAfter: "+err.Error(), false)
				return
			}
		}
		park = r.Park
	}
	if req.Spec.Scheduling != nil && req.Spec.Scheduling.Queue != nil && req.Spec.Scheduling.Queue.MaxWait != "" {
		if maxWait, err = time.ParseDuration(req.Spec.Scheduling.Queue.MaxWait); err != nil {
			writeError(w, reqID, http.StatusBadRequest, "invalid", "scheduling.queue.maxWait: "+err.Error(), false)
			return
		}
	}
	rec, err := s.app.UpsertClass(r.Context(), app.UpsertClassCmd{
		Name: name, Kind: req.Spec.Kind, Provider: req.Spec.Provider,
		Template: req.Spec.Template, ReclaimIdle: reclaim, ReclaimPark: park, ReclaimDel: reclaimDel, QueueMaxWait: maxWait,
		Actor: principalOf(r).Name, MustCreate: mustCreate,
	})
	switch {
	case errors.Is(err, app.ErrClassExists):
		writeError(w, reqID, http.StatusConflict, "conflict", err.Error(), false)
	case errors.Is(err, app.ErrClassConfigOwned):
		writeError(w, reqID, http.StatusConflict, "config_owned", err.Error(), false)
	case err != nil:
		s.writeAppError(w, reqID, err)
	default:
		status := http.StatusOK
		if mustCreate {
			status = http.StatusCreated
		}
		writeJSON(w, status, toClassEnvelope(rec))
	}
}

func (s *Server) getClass(w http.ResponseWriter, r *http.Request) {
	rec, err := s.app.GetClass(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	writeJSON(w, http.StatusOK, toClassEnvelope(rec))
}

func (s *Server) listClasses(w http.ResponseWriter, r *http.Request) {
	recs, err := s.app.ListClasses(r.Context())
	if err != nil {
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
		return
	}
	out := apiclient.ClassList{APIVersion: apiclient.APIVersion, Kind: "ClassList", Items: []apiclient.Class{}}
	for _, rec := range recs {
		out.Items = append(out.Items, toClassEnvelope(rec))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteClass(w http.ResponseWriter, r *http.Request) {
	err := s.app.DeleteClass(r.Context(), r.PathValue("name"), principalOf(r).Name)
	switch {
	case errors.Is(err, app.ErrClassConfigOwned):
		writeError(w, w.Header().Get("X-Request-Id"), http.StatusConflict, "config_owned", err.Error(), false)
	case err != nil:
		s.writeAppError(w, w.Header().Get("X-Request-Id"), err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"name": r.PathValue("name"), "status": "deleted"})
	}
}

func toClassEnvelope(rec *storage.ClassRecord) apiclient.Class {
	env := apiclient.Class{
		APIVersion: apiclient.APIVersion, Kind: "Class",
		Metadata: apiclient.Metadata{
			Name:      rec.Name,
			CreatedAt: time.UnixMilli(rec.CreatedAt).UTC(),
			UpdatedAt: time.UnixMilli(rec.UpdatedAt).UTC(),
		},
		Spec: apiclient.ClassSpec{
			Kind: rec.Kind, Provider: string(rec.Provider), Template: rec.Spec,
		},
		Source: rec.Source,
	}
	if rec.ReclaimIdleAfterMs != nil || rec.ReclaimPark != "" || rec.ReclaimDeleteAfterMs != nil {
		cr := &apiclient.ClassReclaim{Park: rec.ReclaimPark}
		if rec.ReclaimIdleAfterMs != nil {
			cr.IdleAfter = (time.Duration(*rec.ReclaimIdleAfterMs) * time.Millisecond).String()
		}
		if rec.ReclaimDeleteAfterMs != nil {
			cr.DeleteAfter = (time.Duration(*rec.ReclaimDeleteAfterMs) * time.Millisecond).String()
		}
		env.Spec.Reclaim = cr
	}
	if rec.QueueMaxWaitMs != nil {
		env.Spec.Scheduling = &apiclient.ClassScheduling{Queue: &apiclient.ClassQueue{
			MaxWait: (time.Duration(*rec.QueueMaxWaitMs) * time.Millisecond).String()}}
	}
	return env
}

// readBody drains the (size-capped) request body; decode errors surface in
// the caller's json decoding.
func readBody(w http.ResponseWriter, r *http.Request) []byte {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20))
	return buf.Bytes()
}
