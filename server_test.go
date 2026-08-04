package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newServer(t.TempDir(), "/bin/sh"))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string) (int, executeResponse) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/execute", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post /execute: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var out executeResponse
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return res.StatusCode, out
}

func TestExecute(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	t.Run("captures stdout and exit code", func(t *testing.T) {
		status, out := post(t, srv, `{"command": "echo hi; exit 3"}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if out.Stdout != "hi\n" || out.ExitCode != 3 {
			t.Fatalf("stdout = %q, exitCode = %d; want \"hi\\n\", 3", out.Stdout, out.ExitCode)
		}
	})

	t.Run("runs in the workspace", func(t *testing.T) {
		_, out := post(t, srv, `{"command": "pwd > cwd.txt && cat cwd.txt"}`)
		if strings.TrimSpace(out.Stdout) == "" {
			t.Fatal("expected the workspace path on stdout")
		}
	})

	t.Run("times out and kills the process tree", func(t *testing.T) {
		_, out := post(t, srv, `{"command": "sleep 30", "timeoutSeconds": 1}`)
		if out.ExitCode != timeoutExitCode {
			t.Fatalf("exitCode = %d, want %d", out.ExitCode, timeoutExitCode)
		}
	})

	t.Run("truncates oversized output", func(t *testing.T) {
		_, out := post(t, srv, `{"command": "yes 0123456789 | head -c 2000000"}`)
		if !out.Truncated {
			t.Fatal("truncated = false, want true")
		}
		if len(out.Stdout) != maxStream {
			t.Fatalf("stdout length = %d, want %d", len(out.Stdout), maxStream)
		}
	})

	t.Run("rejects a missing command", func(t *testing.T) {
		if status, _ := post(t, srv, `{}`); status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
	})
}

func TestExecuteIsSerialized(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Go(func() {
			statuses[i], _ = post(t, srv, `{"command": "sleep 1"}`)
		})
	}
	wg.Wait()

	var conflicts int
	for _, status := range statuses {
		if status == http.StatusConflict {
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("got %d conflicts across two concurrent execs, want exactly 1 (statuses: %v)", conflicts, statuses)
	}
}

func TestFiles(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	client := srv.Client()

	do := func(t *testing.T, method, target, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+target, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}

	t.Run("write then read", func(t *testing.T) {
		if res := do(t, http.MethodPut, "/files/nested/main.py", "print(1)"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT status = %d, want 204", res.StatusCode)
		}

		res := do(t, http.MethodGet, "/files/nested/main.py", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", res.StatusCode)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "print(1)" {
			t.Fatalf("body = %q, want %q", body, "print(1)")
		}
	})

	t.Run("read a directory as JSON", func(t *testing.T) {
		res := do(t, http.MethodGet, "/files/nested", "")
		var out struct{ Entries []entry }
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("decode listing: %v", err)
		}
		if len(out.Entries) != 1 || out.Entries[0].Name != "main.py" {
			t.Fatalf("entries = %+v, want one main.py", out.Entries)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if res := do(t, http.MethodDelete, "/files/nested/main.py", ""); res.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204", res.StatusCode)
		}
		if res := do(t, http.MethodGet, "/files/nested/main.py", ""); res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET after DELETE status = %d, want 404", res.StatusCode)
		}
	})

	t.Run("rejects writing over or deleting the workspace root", func(t *testing.T) {
		if res := do(t, http.MethodPut, "/files/", "x"); res.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT /files/ status = %d, want 400", res.StatusCode)
		}
		if res := do(t, http.MethodDelete, "/files/", ""); res.StatusCode != http.StatusBadRequest {
			t.Errorf("DELETE /files/ status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("lists the workspace root", func(t *testing.T) {
		if res := do(t, http.MethodGet, "/files/", ""); res.StatusCode != http.StatusOK {
			t.Errorf("GET /files/ status = %d, want 200", res.StatusCode)
		}
	})

	t.Run("rejects escapes", func(t *testing.T) {
		// Unencoded ".." never reaches the handler — net/http cleans the path
		// first — so these are the encoded forms the guard actually sees.
		for _, target := range []string{"/files/..%2fescaped", "/files/a%2f..%2f..%2fescaped", "/files/%2fetc%2fpasswd"} {
			if res := do(t, http.MethodGet, target, ""); res.StatusCode != http.StatusBadRequest {
				t.Errorf("GET %s status = %d, want 400", target, res.StatusCode)
			}
		}
	})
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}
