package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCollectionValidityAndSuccessClock(t *testing.T) {
	s, _, path, _ := setup(t, "")
	poll := func(body string) Envelope {
		t.Helper()
		put(t, path, body)
		s.PollOnce(context.Background())
		return s.runs["demo"].saved.Envelope
	}
	first := poll(sample(1, "a") + "\n")
	if first.Collection.LastSuccessAt == nil {
		t.Fatal("missing successful observation")
	}
	original := string(first.Data)
	// A sentinel makes clock changes observable without sleeping.
	old := "2025-01-01T00:00:00Z"
	s.runs["demo"].saved.Envelope.Collection.LastSuccessAt = &old
	for _, tc := range []struct{ body, state, code string }{
		{"{broken}\n", "unreachable", "data_invalid"},
		{sample(2, "a"), "unreachable", "not_ready"},
		{"", "unreachable", "not_ready"},
		{sample(1, "a") + "\n{broken}\n", "unreachable", "data_invalid"},
		{"\n", "unreachable", "data_invalid"},
	} {
		e := poll(tc.body)
		if e.Collection.State != tc.state || e.Collection.Error == nil || e.Collection.Error.Code != tc.code || e.Collection.LastSuccessAt == nil || *e.Collection.LastSuccessAt != old || string(e.Data) != original {
			t.Fatalf("body %q: %+v", tc.body, e)
		}
	}
	for _, body := range []string{sample(1, "a") + "\n", sample(1, "a") + "\n" + sample(2, "a")[:40], "{bad}\n" + sample(1, "a") + "\n"} {
		e := poll(body)
		if e.Collection.State != "ok" || e.Collection.Error != nil || *e.Collection.LastSuccessAt == old || string(e.Data) != original {
			t.Fatalf("valid replay changed data or failed: %+v", e)
		}
	}
}

func TestSequenceConflictAndAttemptReplay(t *testing.T) {
	s, c, path, cache := setup(t, "")
	poll := func(body string) Envelope {
		t.Helper()
		put(t, path, body)
		s.PollOnce(context.Background())
		return s.runs["demo"].saved.Envelope
	}
	poll(sample(2, "a") + "\n")
	conflicting := strings.Replace(sample(2, "a"), `"completed":2`, `"completed":3`, 1)
	e := poll(conflicting + "\n")
	if e.Collection.Error == nil || e.Collection.Error.Code != "SEQ_CONFLICT" || string(e.Data) != sample(2, "a") {
		t.Fatalf("conflict not detected: %+v", e)
	}
	// Hash identity is independent of object key order and whitespace.
	var value any
	if err := json.Unmarshal([]byte(sample(2, "a")), &value); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(value)
	e = poll(string(canonical) + "\n")
	if e.Collection.State != "ok" {
		t.Fatal("canonical replay was treated as conflict")
	}
	e = poll(sample(1, "b") + "\n" + sample(1, "a") + "\n" + sample(2, "a") + "\n" + sample(99, "a") + "\n")
	if e.Collection.State != "ok" || !strings.Contains(string(e.Data), `"attempt_id":"b"`) {
		t.Fatalf("retired tail replay: %+v", e)
	}
	restored, err := New(c, cache, "")
	if err != nil {
		t.Fatal(err)
	}
	s = restored
	e = poll(sample(1, "a") + "\n" + sample(99, "a") + "\n")
	if e.Collection.State != "ok" || !strings.Contains(string(e.Data), `"attempt_id":"b"`) {
		t.Fatal("retired attempt returned after restart")
	}
	// Within-window identities also detect conflicts below the stored high-water mark.
	e = poll(sample(1, "a") + "\n" + strings.Replace(sample(1, "a"), `"completed":2`, `"completed":3`, 1) + "\n")
	if e.Collection.Error == nil || e.Collection.Error.Code != "SEQ_CONFLICT" {
		t.Fatal("window conflict was lost")
	}
}

func TestHigherSequenceCannotRegress(t *testing.T) {
	for _, tc := range []struct{ name, old, next string }{
		{"completed", sample(1, "a"), strings.Replace(sample(2, "a"), `"completed":2`, `"completed":1`, 1)},
		{"timestamp", strings.Replace(sample(1, "a"), `"timestamp":"2026-01-01T00:00:00Z"`, `"timestamp":"2026-01-01T00:01:00Z"`, 1), sample(2, "a")},
		{"terminal", strings.ReplaceAll(sample(1, "a"), `"state":"running"`, `"state":"succeeded"`), sample(2, "a")},
		{"started_at", sample(1, "a"), strings.Replace(sample(2, "a"), `"started_at":"2026-01-01T00:00:00Z"`, `"started_at":"2025-12-31T00:00:00Z"`, 1)},
		{"task_identity", sample(1, "a"), strings.Replace(sample(2, "a"), `"id":"task"`, `"id":"other"`, 1)},
		{"large_counter", strings.Replace(sample(1, "a"), `"completed":2`, `"completed":9007199254740993`, 1), strings.Replace(sample(2, "a"), `"completed":2`, `"completed":9007199254740992`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, path, _ := setup(t, "")
			put(t, path, tc.old+"\n")
			s.PollOnce(context.Background())
			if s.runs["demo"].saved.Envelope.Collection.State != "ok" {
				t.Fatal("invalid fixture")
			}
			put(t, path, tc.next+"\n")
			s.PollOnce(context.Background())
			e := s.runs["demo"].saved.Envelope
			if string(e.Data) != tc.old || e.Collection.Error == nil || e.Collection.Error.Code != "data_invalid" {
				t.Fatalf("regression accepted: %+v", e)
			}
		})
	}
}

func TestProtocolCrossFieldRules(t *testing.T) {
	good := sample(1, "a")
	for _, bad := range []string{
		strings.Replace(good, `"started_at":"2026-01-01T00:00:00Z"`, `"started_at":"2026-01-02T00:00:00Z"`, 1),
		strings.Replace(good, `"updated_at":"2026-01-01T00:00:00Z"`, `"updated_at":"2026-01-02T00:00:00Z"`, 1),
		strings.Replace(good, `"metrics":{},"error":null`, `"last_progress_at":"2026-01-02T00:00:00Z","metrics":{},"error":null`, 1),
		strings.Replace(good, `"state":"running"`, `"state":"succeeded"`, 1),
		strings.Replace(good, `"state":"running"`, `"state":"pending"`, 1),
		strings.Replace(good, `"state":"running","progress"`, `"state":"failed","progress"`, 1),
		strings.Replace(good, `"total":null`, `"total":1.99999999999999999`, 1),
		strings.Replace(good, `"completed":2`, `"completed":-1e-999999999`, 1),
		strings.Replace(good, `"completed":2`, `"completed":1e-999999999`, 1),
	} {
		if _, err := validateSnapshot([]byte(bad), "demo"); err == nil {
			t.Fatalf("accepted invalid cross-field data: %s", bad)
		}
	}
}

func TestCheckpointFailureIsVisibleAndSafe(t *testing.T) {
	for _, mode := range []string{"create", "rename"} {
		t.Run(mode, func(t *testing.T) {
			s, _, path, cache := setup(t, "")
			put(t, path, sample(1, "a")+"\n")
			s.PollOnce(context.Background())
			old := *s.runs["demo"].saved.Envelope.Collection.LastSuccessAt
			if mode == "create" {
				s.dataDir = filepath.Join(cache, "private-missing-directory")
			} else {
				target := filepath.Join(cache, "demo.json")
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			put(t, path, sample(2, "a")+"\n")
			s.PollOnce(context.Background())
			e := s.runs["demo"].saved.Envelope
			if e.Collection.State != "unreachable" || e.Collection.Error == nil || e.Collection.Error.Code != "cache_write_failed" || *e.Collection.LastSuccessAt != old {
				t.Fatalf("persistence failure hidden: %+v", e)
			}
			if output.Len() == 0 || strings.Contains(output.String(), cache) || strings.Contains(e.Collection.Error.Message, cache) {
				t.Fatal("unsafe or missing persistence diagnostic")
			}
			entries, err := os.ReadDir(cache)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".checkpoint-") {
					t.Fatal("temporary checkpoint leaked")
				}
			}
		})
	}
}

func TestConcurrentSnapshotReads(t *testing.T) {
	s, _, path, _ := setup(t, "")
	put(t, path, sample(1, "a")+"\n")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r := httptest.NewRecorder()
				s.ServeHTTP(r, httptest.NewRequest("GET", "/v1/runs/demo/snapshot", nil))
				if r.Code != 200 {
					t.Error("snapshot failed")
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		s.PollOnce(context.Background())
	}
	wg.Wait()
}
