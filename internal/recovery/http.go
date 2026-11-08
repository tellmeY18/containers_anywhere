package recovery

import (
	"encoding/json"
	"net/http"
	"os"
)

// Routes must be mounted only behind the guest agent's authenticated host
// transport. This package does not create an unauthenticated listener.
func (e *Engine) Routes(mux *http.ServeMux) {
	respond := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			status := http.StatusConflict
			if os.IsNotExist(err) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /_agent/recovery/operations", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		op, err := e.Submit(r.Context(), req)
		respond(w, op, err)
	})
	mux.HandleFunc("GET /_agent/recovery/operations", func(w http.ResponseWriter, r *http.Request) {
		ops, err := e.Operations()
		respond(w, map[string]any{"operations": ops}, err)
	})
	mux.HandleFunc("GET /_agent/recovery/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		op, err := e.Status(r.PathValue("id"))
		respond(w, op, err)
	})
	mux.HandleFunc("GET /_agent/recovery/restart-needed", func(w http.ResponseWriter, r *http.Request) {
		ops, err := e.Operations()
		pending := []Operation{}
		for _, op := range ops {
			if active(op) {
				pending = append(pending, op)
			}
		}
		respond(w, map[string]any{"restartNeeded": len(pending) > 0, "operations": pending}, err)
	})
	mux.HandleFunc("GET /_agent/recovery/points", func(w http.ResponseWriter, r *http.Request) {
		points, err := e.List()
		respond(w, map[string]any{"points": points}, err)
	})
	mux.HandleFunc("GET /_agent/recovery/points/{id}", func(w http.ResponseWriter, r *http.Request) { m, err := e.Get(r.PathValue("id")); respond(w, m, err) })
	mux.HandleFunc("DELETE /_agent/recovery/points/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := e.Delete(r.Context(), r.PathValue("id"))
		respond(w, map[string]any{"deleted": err == nil}, err)
	})
	mux.HandleFunc("GET /_agent/recovery/points/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		stream := &responseStream{ResponseWriter: w}
		if err := e.Export(r.Context(), r.PathValue("id"), stream); err != nil { // Abort, never append an error message to an apparently successful archive.
			if !stream.started {
				w.Header().Set("Content-Type", "text/plain")
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			panic(http.ErrAbortHandler)
		}
	})
	mux.HandleFunc("POST /_agent/recovery/import", func(w http.ResponseWriter, r *http.Request) {
		m, err := e.Import(r.Context(), http.MaxBytesReader(w, r.Body, MaxArchiveBytes+(64<<20)))
		respond(w, m, err)
	})
	mux.HandleFunc("POST /_agent/recovery/workload-snapshots", func(w http.ResponseWriter, r *http.Request) {
		var req RegisterRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil {
			respond(w, nil, err)
			return
		}
		m, err := e.RegisterSnapshots(r.Context(), req)
		respond(w, m, err)
	})
	mux.HandleFunc("POST /_agent/recovery/workload-restore", func(w http.ResponseWriter, r *http.Request) {
		var req PopulateRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil {
			respond(w, nil, err)
			return
		}
		p, err := e.PopulateWorkload(r.Context(), req)
		respond(w, p, err)
	})
	mux.HandleFunc("GET /_agent/recovery/workload-restore/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := e.PopulationStatus(r.PathValue("id"))
		respond(w, p, err)
	})
}

type responseStream struct {
	http.ResponseWriter
	started bool
}

func (w *responseStream) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}
