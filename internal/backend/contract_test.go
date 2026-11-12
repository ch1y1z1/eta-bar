package backend

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnumTokensAreExact(t *testing.T) {
	for _, values := range []string{"pending running succeeded failed cancelled", "reported rate historical manual unknown"} {
		tokens := strings.Fields(values)
		for _, token := range tokens {
			if !oneOf(token, values) {
				t.Fatalf("rejected token %q", token)
			}
		}
		for _, value := range []any{strings.Join(tokens[:2], " "), " " + tokens[0], tokens[0] + " ", "", nil, 1} {
			if oneOf(value, values) {
				t.Fatalf("accepted non-token %#v", value)
			}
		}
	}
	good := sample(1, "a")
	for _, bad := range []string{
		strings.Replace(good, `"state":"running"`, `"state":"pending running"`, 1),
		strings.ReplaceAll(good, `"state":"running"`, `"state":"running succeeded"`),
		strings.Replace(good, `"remaining_seconds":null,"method":"unknown"`, `"remaining_seconds":1,"method":"reported rate"`, 1),
	} {
		if _, err := validateSnapshot([]byte(bad), "demo"); err == nil {
			t.Fatal("accepted concatenated enum in snapshot")
		}
	}
}

func TestSnapshotContractLimits(t *testing.T) {
	for _, tc := range []struct {
		seq   string
		valid bool
	}{
		{"1", true}, {"9007199254740991", true}, {"9007199254740992", false}, {"18446744073709551615", false}, {"0", false},
	} {
		b := strings.Replace(sample(1, "a"), `"seq":1`, `"seq":`+tc.seq, 1)
		if _, err := validateSnapshot([]byte(b), "demo"); (err == nil) != tc.valid {
			t.Fatalf("seq %s: %v", tc.seq, err)
		}
	}
	for _, count := range []int{0, 1, 200, 201} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(sample(1, "a")), &m); err != nil {
			t.Fatal(err)
		}
		var tasks []json.RawMessage
		if err := json.Unmarshal(m["tasks"], &tasks); err != nil {
			t.Fatal(err)
		}
		many := make([]json.RawMessage, count)
		for i := range many {
			many[i] = json.RawMessage(strings.Replace(string(tasks[0]), `"id":"task"`, fmt.Sprintf(`"id":"task%d"`, i), 1))
			many[i] = json.RawMessage(strings.Replace(string(many[i]), `"state":"running"`, `"state":"pending"`, 1))
			many[i] = json.RawMessage(strings.Replace(string(many[i]), `"completed":2`, `"completed":0`, 1))
		}
		m["tasks"], _ = json.Marshal(many)
		m["state"] = json.RawMessage(`"pending"`)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateSnapshot(b, "demo"); (err == nil) != (count >= 1 && count <= 200) {
			t.Fatalf("tasks %d: %v", count, err)
		}
	}
}

func TestEmptyRuns(t *testing.T) {
	s, err := New(Config{Runs: []Run{}}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/healthz", "/v1/runs"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if path == "/v1/runs" && strings.TrimSpace(rec.Body.String()) != `{"runs":[]}` {
			t.Fatalf("empty list: %s", rec.Body.String())
		}
	}
}
