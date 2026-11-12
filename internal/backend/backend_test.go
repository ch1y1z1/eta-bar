package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sample(seq int, attempt string) string {
	return fmt.Sprintf(`{"schema_version":"1.0","run_id":"demo","attempt_id":%q,"seq":%d,"timestamp":"2026-01-01T00:00:00Z","started_at":"2026-01-01T00:00:00Z","state":"running","execution":"sequential","tasks":[{"id":"task","title":"Task","state":"running","progress":{"completed":2,"total":null,"unit":"steps"},"eta":{"remaining_seconds":null,"method":"unknown","updated_at":"2026-01-01T00:00:00Z"},"dimensions":{},"metrics":{}}],"metrics":{},"error":null}`, attempt, seq)
}
func setup(t *testing.T, token string) (*Server, Config, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	cache := filepath.Join(dir, "cache")
	c := Config{Runs: []Run{{ID: "demo", Title: "Demo", Source: Source{Mode: "local", LogPath: path}}}}
	s, err := New(c, cache, token)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, path, cache
}
func put(t *testing.T, path, b string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(b), 0600); err != nil {
		t.Fatal(err)
	}
}
func snapshot(t *testing.T, client *http.Client, url, token string) Envelope {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/v1/runs/demo/snapshot", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var e Envelope
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}
func seqOf(t *testing.T, e Envelope) uint64 {
	t.Helper()
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return 0
	}
	var v struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(e.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v.Seq
}
func TestHTTPCollectionAndRestart(t *testing.T) {
	s, c, path, cache := setup(t, "test-token")
	ts := httptest.NewServer(s)
	defer ts.Close()
	put(t, path, sample(1, "a")+"\n"+sample(2, "a")[:40])
	s.PollOnce(context.Background())
	e := snapshot(t, ts.Client(), ts.URL, "test-token")
	if seqOf(t, e) != 1 || e.Collection.State != "ok" || e.Collection.LastSuccessAt == nil {
		t.Fatalf("bad first snapshot: %+v", e)
	}
	put(t, path, sample(1, "a")+"\n{bad json}\n"+sample(2, "a")+"\n")
	s.PollOnce(context.Background())
	e = snapshot(t, ts.Client(), ts.URL, "test-token")
	if seqOf(t, e) != 2 {
		t.Fatal("complete appended line not collected")
	}
	original := string(e.Data)
	put(t, path, strings.Replace(sample(2, "a"), `"completed":2`, `"completed":3`, 1)+"\n"+sample(1, "a")+"\n")
	s.PollOnce(context.Background())
	if got := snapshot(t, ts.Client(), ts.URL, "test-token"); string(got.Data) != original {
		t.Fatal("conflict or rollback replaced snapshot")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.PollOnce(context.Background())
	e = snapshot(t, ts.Client(), ts.URL, "test-token")
	if seqOf(t, e) != 2 || e.Collection.State != "unreachable" || e.Collection.Error.Code != "source_unavailable" || e.Collection.LastSuccessAt == nil {
		t.Fatalf("cache lost on outage: %+v", e)
	}
	restored, err := New(c, cache, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	rt := httptest.NewServer(restored)
	defer rt.Close()
	e = snapshot(t, rt.Client(), rt.URL, "test-token")
	if seqOf(t, e) != 2 || e.Collection.State != "unreachable" {
		t.Fatal("checkpoint restore failed")
	}
	put(t, path, sample(1, "b")+"\n"+sample(99, "a")+"\n")
	restored.PollOnce(context.Background())
	e = snapshot(t, rt.Client(), rt.URL, "test-token")
	if seqOf(t, e) != 1 || !strings.Contains(string(e.Data), `"attempt_id":"b"`) {
		t.Fatal("retired attempt replaced new attempt")
	}
}
func TestAuthAndReadOnlyRoutes(t *testing.T) {
	s, _, _, _ := setup(t, "secret")
	ts := httptest.NewServer(s)
	defer ts.Close()
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", "/healthz", "", 200}, {"GET", "/v1/runs", "", 401}, {"GET", "/v1/runs", "wrong", 401}, {"GET", "/v1/runs", "secret", 200}, {"GET", "/v1/runs/demo/manifest", "secret", 200}, {"GET", "/v1/runs/missing/snapshot", "secret", 404}, {"GET", "/v1/runs/demo/history", "secret", 404}, {"POST", "/v1/runs/demo/snapshot", "secret", 405},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || !json.Valid(body) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, body)
		}
	}
}
func TestValidation(t *testing.T) {
	good := sample(1, "a")
	if _, err := validateSnapshot([]byte(good), "demo"); err != nil {
		t.Fatal("unknown total should be valid")
	}
	for _, bad := range []string{
		strings.Replace(good, `"seq":1`, `"seq":0`, 1),
		strings.Replace(good, `"seq":1`, `"seq":1.5`, 1),
		strings.Replace(good, `"run_id":"demo"`, `"run_id":"other"`, 1),
		strings.Replace(good, `"completed":2`, `"completed":1e999`, 1),
		strings.Replace(good, `"completed":2`, `"completed":-1`, 1),
		strings.Replace(good, `"total":null`, `"total":1`, 1),
		strings.Replace(good, `"method":"unknown"`, `"method":"rate"`, 1),
		strings.Replace(good, `"execution":"sequential"`, `"execution":"parallel"`, 1),
		strings.Replace(good, `"dimensions":{}`, `"dimensions":{"x":[]}`, 1),
		strings.Replace(good, `"state":"running"`, `"state":"failed"`, 1),
	} {
		if _, err := validateSnapshot([]byte(bad), "demo"); err == nil {
			t.Fatalf("accepted invalid snapshot: %s", bad)
		}
	}
}
func TestBoundedTailAndPartialLine(t *testing.T) {
	s, _, path, _ := setup(t, "")
	put(t, path, strings.Repeat("x", 2*ReadLimit)+"\n"+sample(1, "a")+"\n"+sample(2, "a"))
	s.PollOnce(context.Background())
	st := s.runs["demo"]
	if seqOf(t, st.saved.Envelope) != 1 {
		t.Fatal("failed to find latest complete bounded record")
	}
	put(t, path, strings.Repeat("x", ReadLimit+20)+"\n")
	s.PollOnce(context.Background())
	if seqOf(t, st.saved.Envelope) != 1 {
		t.Fatal("oversized line replaced snapshot")
	}
	put(t, path, sample(3, "a")+"\n")
	s.PollOnce(context.Background())
	if seqOf(t, st.saved.Envelope) != 3 {
		t.Fatal("rotation/truncation recovery failed")
	}
}
func TestListenAndHostValidation(t *testing.T) {
	for _, tc := range []struct {
		addr, token string
		valid       bool
	}{{"127.0.0.1:8080", "", true}, {"[::1]:8080", "", true}, {"0.0.0.0:8080", "", false}, {":8080", "", false}, {"0.0.0.0:8080", "token", true}} {
		if (ValidateListen(tc.addr, tc.token) == nil) != tc.valid {
			t.Fatalf("listen validation: %s", tc.addr)
		}
	}
	for _, alias := range []string{"-oProxyCommand=evil", "host\nother", "user@host", "host path"} {
		if _, err := New(Config{Hosts: map[string]HostProfile{"x": {Alias: alias, AllowedRoot: "/logs"}}}, t.TempDir(), ""); err == nil {
			t.Fatalf("accepted alias %q", alias)
		}
	}
}
func TestIncompleteAndConflictCollectionEnvelope(t *testing.T) {
	s, _, path, _ := setup(t, "")
	put(t, path, sample(1, "a")[:40])
	s.PollOnce(context.Background())
	e := s.runs["demo"].saved.Envelope
	if e.Collection.State != "unreachable" || e.Collection.Error == nil || e.Collection.Error.Code != "not_ready" || e.Collection.LastSuccessAt != nil {
		t.Fatal("partial-only source must return the supported unreachable state")
	}
	put(t, path, sample(1, "a")+"\n")
	s.PollOnce(context.Background())
	before := s.runs["demo"].saved.Envelope
	if before.Collection.State != "ok" || before.Collection.LastSuccessAt == nil {
		t.Fatal("valid record was not collected")
	}
	put(t, path, strings.Replace(sample(1, "a"), `"completed":2`, `"completed":3`, 1)+"\n")
	s.PollOnce(context.Background())
	after := s.runs["demo"].saved.Envelope
	if after.Collection.State != "unreachable" || after.Collection.Error == nil || after.Collection.Error.Code != "SEQ_CONFLICT" || string(after.Data) != string(before.Data) || *after.Collection.LastSuccessAt != *before.Collection.LastSuccessAt {
		t.Fatal("sequence conflict must retain cached data and last success")
	}
}

func TestShellQuoting(t *testing.T) {
	for _, value := range []string{"/logs/a'b.jsonl", "/logs/a; echo unsafe", "/logs/$(echo unsafe)", "/logs/with space", ""} {
		out, err := exec.Command("sh", "-c", "printf '%s' "+ShellQuote(value)).Output()
		if err != nil || string(out) != value {
			t.Fatalf("quote %q: %q %v", value, out, err)
		}
	}
	script := sshScript("/logs/a'b.jsonl", "/logs")
	if !strings.Contains(script, `case "$p" in "$root"/*)`) || !strings.Contains(script, "tail -c 262145") {
		t.Fatal("missing confinement or limit")
	}
}
