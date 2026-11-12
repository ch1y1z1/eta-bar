package backend

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func pathRequest(s *Server, host, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/snapshot?"+url.Values{"host": {host}, "path": {path}}.Encode(), nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func pathEnvelope(t *testing.T, s *Server, host, path string) Envelope {
	t.Helper()
	w := pathRequest(s, host, path, "test-token")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var e Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestPathSecurity(t *testing.T) {
	root := t.TempDir()
	s, err := New(Config{LocalAllowedRoots: []string{root}, Hosts: map[string]HostProfile{"cluster": {Alias: "trusted-alias", AllowedRoot: "/experiments"}}}, t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, path, token string
		status            int
	}{
		{"local", root + "/monitor.jsonl", "", 401},
		{"local", root + "/monitor.jsonl", "wrong", 401},
		{"local", root + "/../monitor.jsonl", "test-token", 400},
		{"local", root + "-other/monitor.jsonl", "test-token", 400},
		{"local", "relative/monitor.jsonl", "test-token", 400},
		{"local", root + "/bad\nfile", "test-token", 400},
		{"attacker.example", "/experiments/monitor.jsonl", "test-token", 400},
		{"-oProxyCommand=bad", "/experiments/monitor.jsonl", "test-token", 400},
		{"cluster", "/experiments/../secret", "test-token", 400},
		{"cluster", "/experiments-other/monitor.jsonl", "test-token", 400},
	} {
		w := pathRequest(s, tc.host, tc.path, tc.token)
		if w.Code != tc.status || strings.Contains(w.Body.String(), root) {
			t.Fatalf("unsafe result: %d %s", w.Code, w.Body.String())
		}
	}
	if source, ok := s.pathSource("cluster", "/experiments/run/monitor.jsonl"); !ok || source.Host != "cluster" || source.Mode != "ssh" {
		t.Fatal("predefined SSH profile rejected")
	}
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	put(t, outside, sample(1, "a")+"\n")
	link := filepath.Join(root, "monitor.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	e := pathEnvelope(t, s, "local", link)
	if seqOf(t, e) != 0 || e.Collection.Error.Code != "source_unavailable" {
		t.Fatal("symlink escape read")
	}
	noRoots, _ := New(Config{}, t.TempDir(), "test-token")
	if w := pathRequest(noRoots, "local", outside, "test-token"); w.Code != 400 {
		t.Fatal("local filesystem enabled by default")
	}
	noAuth, _ := New(Config{LocalAllowedRoots: []string{root}}, t.TempDir(), "")
	if w := pathRequest(noAuth, "local", link, ""); w.Code != 401 {
		t.Fatal("path endpoint requires token even on loopback")
	}
	for _, raw := range []string{"host=local", "host=local&host=cluster&path=/x", "host=local&path=/x&command=cat", "host=local&path=%zz"} {
		r := httptest.NewRequest("GET", "/v1/snapshot?"+raw, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("invalid query accepted")
		}
	}
	for _, root := range []string{"/", "relative", "/logs/../"} {
		if _, err := New(Config{LocalAllowedRoots: []string{root}}, t.TempDir(), "test-token"); err == nil {
			t.Fatal("unsafe root accepted")
		}
	}
}
func TestPathSSHConfinementAndHostIsolation(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	// Run the actual confined remote script locally, without contacting a host.
	fake := filepath.Join(bin, "ssh")
	put(t, fake, "#!/bin/sh\nfor arg do\n case \"$arg\" in unavailable-alias) exit 1;; esac\n last=$arg\ndone\nexec sh -c \"$last\"\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s, err := New(Config{LocalAllowedRoots: []string{root}, Hosts: map[string]HostProfile{
		"cluster": {Alias: "trusted-alias", AllowedRoot: root},
		"offline": {Alias: "unavailable-alias", AllowedRoot: root},
	}}, t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a'$(false); monitor.jsonl")
	put(t, path, sample(1, "a")+"\n")
	if e := pathEnvelope(t, s, "cluster", path); seqOf(t, e) != 1 {
		t.Fatal("SSH literal path failed")
	}
	if e := pathEnvelope(t, s, "offline", path); seqOf(t, e) != 0 || e.Collection.Error.Code != "source_unavailable" {
		t.Fatal("failed host inherited other host's cache")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	put(t, outside, sample(2, "a")+"\n")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if e := pathEnvelope(t, s, "cluster", path); seqOf(t, e) != 1 || e.Collection.Error.Code != "source_unavailable" {
		t.Fatal("SSH symlink escape or cache loss")
	}
}

func TestPathLifecycleAndIsolation(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	s, err := New(Config{LocalAllowedRoots: []string{root}}, cache, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(root, "a.jsonl"), filepath.Join(root, "b.jsonl")
	put(t, a, "{bad}\n"+sample(1, "a")+"\n"+sample(2, "a")[:40])
	e := pathEnvelope(t, s, "local", a)
	if seqOf(t, e) != 1 || e.Collection.State != "ok" || !strings.Contains(string(e.Data), `"run_id":"demo"`) {
		t.Fatal("run discovery or partial tail failed")
	}
	for _, next := range []string{
		strings.Replace(sample(2, "a"), `"completed":2`, `"completed":1`, 1),
		strings.Replace(sample(1, "a"), `"completed":2`, `"completed":3`, 1),
		strings.Replace(sample(2, "a"), `"run_id":"demo"`, `"run_id":"other"`, 1),
	} {
		put(t, a, next+"\n")
		e = pathEnvelope(t, s, "local", a)
		if seqOf(t, e) != 1 || e.Collection.State != "unreachable" {
			t.Fatal("lifecycle or run identity regressed")
		}
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	e = pathEnvelope(t, s, "local", a)
	if seqOf(t, e) != 1 || e.Collection.Error.Code != "source_unavailable" || e.Collection.LastSuccessAt == nil {
		t.Fatal("outage lost cache")
	}
	if e = pathEnvelope(t, s, "local", b); seqOf(t, e) != 0 {
		t.Fatal("failed new path inherited prior data")
	}
	put(t, b, strings.Replace(sample(1, "a"), `"run_id":"demo"`, `"run_id":"other"`, 1)+"\n")
	if e = pathEnvelope(t, s, "local", b); !strings.Contains(string(e.Data), `"run_id":"other"`) {
		t.Fatal("path-only switch did not discover new run")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); pathRequest(s, "local", b, "test-token") }()
	}
	wg.Wait()
	for i := 0; i < pathCacheLimit+2; i++ {
		pathEnvelope(t, s, "local", filepath.Join(root, fmt.Sprintf("missing-%d.jsonl", i)))
	}
	if len(s.paths) != pathCacheLimit {
		t.Fatal("cache is not bounded")
	}
	if e = pathEnvelope(t, s, "local", a); seqOf(t, e) != 0 {
		t.Fatal("LRU eviction did not clear oldest source")
	}
	files, err := os.ReadDir(cache)
	if err != nil || len(files) != 0 {
		t.Fatal("on-demand reads created disk checkpoints")
	}
}
