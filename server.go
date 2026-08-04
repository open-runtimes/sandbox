package main

import (
	"encoding/json"
	"net/http"
)

type server struct {
	workspace string
	shell     string
	// seat serializes /execute: one sandbox is one seat, and concurrent execs
	// would race on a shared filesystem. A busy sandbox answers 409.
	seat chan struct{}
	mux  *http.ServeMux
}

func newServer(workspace, shell string) *server {
	s := &server{
		workspace: workspace,
		shell:     shell,
		seat:      make(chan struct{}, 1),
		mux:       http.NewServeMux(),
	}

	s.mux.HandleFunc("POST /execute", s.execute)
	s.mux.HandleFunc("GET /files/{path...}", s.readFile)
	s.mux.HandleFunc("PUT /files/{path...}", s.writeFile)
	s.mux.HandleFunc("DELETE /files/{path...}", s.deleteFile)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
