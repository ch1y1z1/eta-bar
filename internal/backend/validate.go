package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type validated struct {
	Attempt            string
	Seq                uint64
	Canonical          []byte
	Fields             map[string]any
	Timestamp, Started time.Time
}

func timestamp(v any) time.Time { t, _ := time.Parse(time.RFC3339Nano, text(v)); return t }
func terminal(v any) bool       { return oneOf(v, "succeeded failed cancelled") }

// JSON numbers are compared exactly, including counters larger than 2^53.
func exactNumber(v any) *big.Rat {
	// number rejects nonzero underflow. Handle zero here so an arbitrary
	// exponent on a zero mantissa never reaches big.Rat's exponent parser.
	if f, _ := number(v); f == 0 {
		return new(big.Rat)
	}
	r, _ := new(big.Rat).SetString(string(v.(json.Number)))
	return r
}
func compareNumbers(a, b any) int {
	return exactNumber(a).Cmp(exactNumber(b))
}
func stateTransition(a, b any) bool {
	if terminal(a) {
		return a == b
	}
	return a != "running" || b != "pending"
}
func validTransition(a, b validated) bool {
	if !a.Started.Equal(b.Started) || b.Timestamp.Before(a.Timestamp) || !stateTransition(a.Fields["state"], b.Fields["state"]) {
		return false
	}
	if p, ok := a.Fields["last_progress_at"]; ok {
		q, exists := b.Fields["last_progress_at"]
		if !exists || timestamp(q).Before(timestamp(p)) {
			return false
		}
	}
	oldTasks := a.Fields["tasks"].([]any)
	newTasks := b.Fields["tasks"].([]any)
	if len(oldTasks) != len(newTasks) {
		return false
	}
	for i, old := range oldTasks {
		x := old.(map[string]any)
		y := newTasks[i].(map[string]any)
		p := x["progress"].(map[string]any)
		q := y["progress"].(map[string]any)
		if x["id"] != y["id"] || p["unit"] != q["unit"] || compareNumbers(q["completed"], p["completed"]) < 0 || !stateTransition(x["state"], y["state"]) {
			return false
		}
		if timestamp(y["eta"].(map[string]any)["updated_at"]).Before(timestamp(x["eta"].(map[string]any)["updated_at"])) {
			return false
		}
	}
	return true
}

var invalid = errors.New("invalid snapshot")

func object(v any, required, optional string) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, k := range strings.Fields(required) {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	for _, k := range strings.Fields(optional) {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return nil, false
		}
	}
	return m, true
}
func text(v any) string { s, _ := v.(string); return s }
func oneOf(v any, values string) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, token := range strings.Fields(values) {
		if s == token {
			return true
		}
	}
	return false
}
func utc(v any) bool {
	s, ok := v.(string)
	if !ok || !strings.HasSuffix(s, "Z") {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}
func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f == 0 {
		mantissa := strings.FieldsFunc(string(n), func(r rune) bool { return r == 'e' || r == 'E' })[0]
		if strings.ContainsAny(mantissa, "123456789") {
			return 0, false
		}
	}
	return f, true
}
func scalars(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return false
	}
	for _, v := range m {
		switch v.(type) {
		case nil, string, bool:
		case json.Number:
			if _, ok := number(v); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func validError(v any) bool {
	if v == nil {
		return true
	}
	m, ok := object(v, "code message", "")
	return ok && text(m["code"]) != "" && text(m["message"]) != ""
}
func validateSnapshot(b []byte, runID string) (validated, error) {
	var out validated
	if len(b) > ReadLimit || !utf8.Valid(b) {
		return out, invalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF {
		return out, invalid
	}
	m, ok := object(v, "schema_version run_id attempt_id seq timestamp started_at state execution tasks metrics error", "last_progress_at")
	if !ok {
		return out, invalid
	}
	if m["schema_version"] != "1.0" || !idPattern.MatchString(text(m["run_id"])) || (runID != "" && m["run_id"] != runID) || !idPattern.MatchString(text(m["attempt_id"])) || !utc(m["timestamp"]) || !utc(m["started_at"]) || !oneOf(m["state"], "pending running succeeded failed cancelled") || m["execution"] != "sequential" || !scalars(m["metrics"]) || !validError(m["error"]) {
		return out, invalid
	}
	if m["state"] == "failed" && m["error"] == nil {
		return out, invalid
	}
	started, stamp := timestamp(m["started_at"]), timestamp(m["timestamp"])
	if started.After(stamp) {
		return out, invalid
	}
	if p, ok := m["last_progress_at"]; ok && (!utc(p) || timestamp(p).Before(started) || timestamp(p).After(stamp)) {
		return out, invalid
	}
	seq, ok := m["seq"].(json.Number)
	if !ok {
		return out, invalid
	}
	n, err := strconv.ParseUint(string(seq), 10, 64)
	if err != nil || n == 0 || n > 9007199254740991 {
		return out, invalid
	}
	tasks, ok := m["tasks"].([]any)
	if !ok || len(tasks) == 0 || len(tasks) > 200 {
		return out, invalid
	}
	ids := map[string]bool{}
	running, succeeded, failed, cancelled := 0, 0, 0, 0
	prefix := true
	for _, t := range tasks {
		task, ok := object(t, "id title state progress eta dimensions metrics", "")
		if !ok {
			return out, invalid
		}
		id := text(task["id"])
		if !idPattern.MatchString(id) || ids[id] || text(task["title"]) == "" || !oneOf(task["state"], "pending running succeeded failed cancelled") || !scalars(task["dimensions"]) || !scalars(task["metrics"]) {
			return out, invalid
		}
		ids[id] = true
		switch task["state"] {
		case "succeeded":
			if !prefix {
				return out, invalid
			}
			succeeded++
		case "running":
			if !prefix {
				return out, invalid
			}
			running++
			prefix = false
		case "failed":
			if !prefix {
				return out, invalid
			}
			failed++
			prefix = false
		case "cancelled":
			cancelled++
			prefix = false
		default:
			prefix = false
		}
		p, ok := object(task["progress"], "completed total unit", "")
		if !ok {
			return out, invalid
		}
		completed, ok := number(p["completed"])
		if !ok || completed < 0 || text(p["unit"]) == "" || utf8.RuneCountInString(text(p["unit"])) > 64 {
			return out, invalid
		}
		if p["total"] != nil {
			total, ok := number(p["total"])
			if !ok || total <= 0 || compareNumbers(p["completed"], p["total"]) > 0 {
				return out, invalid
			}
		}
		eta, ok := object(task["eta"], "remaining_seconds method updated_at", "")
		if !ok || !utc(eta["updated_at"]) || !oneOf(eta["method"], "reported rate historical manual unknown") {
			return out, invalid
		}
		if timestamp(eta["updated_at"]).Before(started) || timestamp(eta["updated_at"]).After(stamp) {
			return out, invalid
		}
		if task["state"] == "pending" && completed != 0 {
			return out, invalid
		}
		if eta["remaining_seconds"] == nil {
			if eta["method"] != "unknown" {
				return out, invalid
			}
		} else {
			n, ok := number(eta["remaining_seconds"])
			if !ok || n < 0 || eta["method"] == "unknown" {
				return out, invalid
			}
		}
	}
	if running > 1 || (m["state"] == "succeeded" && succeeded != len(tasks)) || (m["state"] == "pending" && (running+succeeded+failed+cancelled != 0)) || (failed > 0 && m["state"] != "failed") || (cancelled > 0 && !terminal(m["state"])) || (terminal(m["state"]) && running > 0) {
		return out, invalid
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return out, invalid
	}
	return validated{Attempt: text(m["attempt_id"]), Seq: n, Canonical: canonical, Fields: m, Started: started, Timestamp: stamp}, nil
}
