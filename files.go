package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// errBadPath covers every way a request can name something outside the
// workspace. All of them are 400, never 403: the caller learns nothing about
// what is out there.
var errBadPath = errors.New("path must be relative to the workspace and must not escape it")

type entry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"isDir"`
}

// resolve maps a request path onto an absolute path inside the workspace.
func (s *server) resolve(rel string) (string, error) {
	if strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return "", errBadPath
	}
	if rel == "" {
		return s.workspace, nil // GET /files/ lists the workspace root
	}

	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errBadPath
	}

	return filepath.Join(s.workspace, filepath.FromSlash(clean)), nil
}

func (s *server) readFile(w http.ResponseWriter, r *http.Request) {
	full, ok := s.resolved(w, r)
	if !ok {
		return
	}

	info, err := os.Stat(full)
	if err != nil {
		writeFSError(w, err)
		return
	}

	if info.IsDir() {
		s.list(w, full)
		return
	}

	file, err := os.Open(full)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *server) list(w http.ResponseWriter, full string) {
	items, err := os.ReadDir(full)
	if err != nil {
		writeFSError(w, err)
		return
	}

	entries := make([]entry, 0, len(items))
	for _, item := range items {
		e := entry{Name: item.Name(), IsDir: item.IsDir()}
		if info, err := item.Info(); err == nil {
			e.Size = info.Size()
		}
		entries = append(entries, e)
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	full, ok := s.resolved(w, r)
	if !ok {
		return
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		writeFSError(w, err)
		return
	}

	file, err := os.Create(full)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer func() { _ = file.Close() }()

	if _, err := io.Copy(file, r.Body); err != nil {
		writeFSError(w, err)
		return
	}
	if err := file.Close(); err != nil {
		writeFSError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	full, ok := s.resolved(w, r)
	if !ok {
		return
	}

	if full == s.workspace {
		writeError(w, http.StatusBadRequest, "the workspace root cannot be deleted")
		return
	}

	if _, err := os.Lstat(full); err != nil {
		writeFSError(w, err)
		return
	}
	if err := os.RemoveAll(full); err != nil {
		writeFSError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) resolved(w http.ResponseWriter, r *http.Request) (string, bool) {
	full, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return full, true
}

func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		writeError(w, http.StatusBadRequest, "path is not a writable file")
	case errors.Is(err, os.ErrPermission):
		writeError(w, http.StatusForbidden, "permission denied")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
