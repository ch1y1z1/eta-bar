package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const ReadLimit = 256 * 1024

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type HostProfile struct {
	Alias       string `json:"alias"`
	AllowedRoot string `json:"allowed_root"`
}
type Source struct {
	Mode    string `json:"mode"`
	Host    string `json:"host,omitempty"`
	LogPath string `json:"log_path"`
}
type Run struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Source   Source          `json:"source"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
}
type Config struct {
	Hosts map[string]HostProfile `json:"hosts"`
	Runs  []Run                  `json:"runs"`
}
type SafeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Collection struct {
	State         string     `json:"state"`
	ObservedAt    string     `json:"observed_at"`
	LastSuccessAt *string    `json:"last_success_at"`
	Error         *SafeError `json:"error"`
	Window        string     `json:"window"`
}
type Envelope struct {
	Data       json.RawMessage `json:"data"`
	Collection Collection      `json:"collection"`
}
type Cursor struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}
type checkpoint struct {
	Envelope Envelope          `json:"envelope"`
	Current  string            `json:"current"`
	Attempts map[string]Cursor `json:"attempts"`
}
type runState struct {
	mu    sync.RWMutex
	run   Run
	saved checkpoint
}
type Server struct {
	config  Config
	dataDir string
	token   [32]byte
	auth    bool
	runs    map[string]*runState
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open configuration")
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1024*1024+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("invalid configuration")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid configuration")
	}
	return c, nil
}

func ValidateListen(address, token string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("invalid listen address")
	}
	ip := net.ParseIP(host)
	if token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("non-loopback listen requires ETA_API_TOKEN")
	}
	return nil
}

func New(c Config, dataDir, token string) (*Server, error) {
	s := &Server{config: c, dataDir: dataDir, auth: token != "", token: sha256.Sum256([]byte(token)), runs: map[string]*runState{}}
	for _, h := range c.Hosts {
		if !aliasPattern.MatchString(h.Alias) || !strings.HasPrefix(h.AllowedRoot, "/") || filepath.Clean(h.AllowedRoot) == "/" || strings.ContainsAny(h.AllowedRoot, "\x00\r\n") {
			return nil, errors.New("invalid host profile")
		}
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, errors.New("cannot create cache directory")
	}
	for _, r := range c.Runs {
		if !idPattern.MatchString(r.ID) || r.Title == "" || s.runs[r.ID] != nil || !filepath.IsAbs(r.Source.LogPath) || strings.ContainsAny(r.Source.LogPath, "\x00\r\n") {
			return nil, errors.New("invalid run configuration")
		}
		switch r.Source.Mode {
		case "local":
		case "ssh":
			if _, ok := c.Hosts[r.Source.Host]; !ok {
				return nil, errors.New("unknown host profile")
			}
		default:
			return nil, errors.New("invalid source mode")
		}
		if len(r.Manifest) > 0 && (!json.Valid(r.Manifest) || strings.TrimSpace(string(r.Manifest))[0] != '{') {
			return nil, errors.New("invalid manifest")
		}
		st := &runState{run: r, saved: checkpoint{Attempts: map[string]Cursor{}, Envelope: Envelope{Collection: Collection{State: "unreachable", ObservedAt: now(), Window: "bounded_tail", Error: &SafeError{Code: "not_collected", Message: "Source has not been collected"}}}}}
		s.restore(st)
		s.runs[r.ID] = st
	}
	return s, nil
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Start launches one independent collector per run. Requests never trigger SSH or file I/O.
// A bounded tail is re-read every interval; history before that window is not collected.
func (s *Server) Start(ctx context.Context) {
	for _, st := range s.runs {
		go func(st *runState) {
			s.poll(ctx, st)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.poll(ctx, st)
				}
			}
		}(st)
	}
}
func (s *Server) PollOnce(ctx context.Context) {
	for _, st := range s.runs {
		s.poll(ctx, st)
	}
}
func collectionError(code, message string) *SafeError {
	return &SafeError{Code: code, Message: message}
}

func (s *Server) poll(ctx context.Context, st *runState) {
	b, err := s.readSource(ctx, st.run.Source)
	st.mu.Lock()
	defer st.mu.Unlock()
	c := &st.saved.Envelope.Collection
	previousSuccess := c.LastSuccessAt
	c.ObservedAt = now()
	if err != nil {
		c.State = "unreachable"
		c.Error = collectionError("source_unavailable", "Source is unavailable")
	} else {
		// The envelope uses ok/unreachable; not_ready is the reason code.
		c.State = "unreachable"
		c.Error = collectionError("not_ready", "No complete record in collection window")
		// ReadLimit+1 is a sentinel: discard the possibly clipped first line.
		if len(b) > ReadLimit {
			if i := bytes.IndexByte(b, '\n'); i >= 0 {
				b = b[i+1:]
			} else {
				b = nil
			}
		}
		// Only the latest complete line determines collection health. A partial
		// final line is not a record; earlier valid data remains usable.
		identities := map[string]string{}
		for len(b) > 0 {
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				break
			}
			line := b[:i]
			b = b[i+1:]
			issue := s.accept(st, line, identities)
			if issue != nil {
				c.State = "unreachable"
				c.Error = issue
			} else {
				c.State = "ok"
				c.Error = nil
			}
		}
		if c.State == "ok" {
			t := c.ObservedAt
			c.LastSuccessAt = &t
		}
	}
	if err := s.persist(st); err != nil {
		c.LastSuccessAt = previousSuccess
		c.State = "unreachable"
		c.Error = collectionError("cache_write_failed", "Checkpoint could not be saved")
		// Never log filesystem errors: their text can contain private paths.
		log.Print("ETA checkpoint persistence failed")
	}
}
func (s *Server) accept(st *runState, line []byte, identities map[string]string) *SafeError {
	v, err := validateSnapshot(line, st.run.ID)
	if err != nil {
		return collectionError("data_invalid", "Invalid complete snapshot")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(v.Canonical))
	key := fmt.Sprintf("%s/%d", v.Attempt, v.Seq)
	if prior, ok := identities[key]; ok && prior != hash {
		return collectionError("SEQ_CONFLICT", "Conflicting snapshot sequence")
	}
	identities[key] = hash
	prev, seen := st.saved.Attempts[v.Attempt]
	if seen && v.Seq == prev.Seq && hash != prev.Hash {
		return collectionError("SEQ_CONFLICT", "Conflicting snapshot sequence")
	}
	// Retired attempts and older sequences are harmless tail replays. They
	// count as valid collection but must never replace the current snapshot.
	if seen && (v.Attempt != st.saved.Current || v.Seq <= prev.Seq) {
		return nil
	}
	if !seen && len(st.saved.Attempts) >= 128 {
		return collectionError("attempt_limit", "Attempt tracking limit reached")
	}
	if len(st.saved.Envelope.Data) > 0 {
		current, err := validateSnapshot(st.saved.Envelope.Data, st.run.ID)
		if err != nil {
			return collectionError("data_invalid", "Invalid cached snapshot")
		}
		if v.Attempt == current.Attempt {
			if !validTransition(current, v) {
				return collectionError("data_invalid", "Snapshot lifecycle regressed")
			}
		} else if v.Started.Before(current.Started) || v.Timestamp.Before(current.Timestamp) {
			return collectionError("data_invalid", "Attempt chronology regressed")
		}
	}
	st.saved.Current = v.Attempt
	st.saved.Attempts[v.Attempt] = Cursor{Seq: v.Seq, Hash: hash}
	st.saved.Envelope.Data = append(json.RawMessage(nil), line...)
	return nil
}
func (s *Server) persist(st *runState) error {
	b, err := json.Marshal(st.saved)
	if err != nil {
		return errors.New("checkpoint encoding failed")
	}
	f, err := os.CreateTemp(s.dataDir, ".checkpoint-*")
	if err != nil {
		return errors.New("checkpoint creation failed")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return errors.New("checkpoint write failed")
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return errors.New("checkpoint sync failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("checkpoint close failed")
	}
	if err = os.Rename(name, filepath.Join(s.dataDir, st.run.ID+".json")); err != nil {
		return errors.New("checkpoint replacement failed")
	}
	return nil
}
func (s *Server) restore(st *runState) {
	f, err := os.Open(filepath.Join(s.dataDir, st.run.ID+".json"))
	if err != nil {
		return
	}
	defer f.Close()
	var p checkpoint
	d := json.NewDecoder(io.LimitReader(f, 2*1024*1024))
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || len(p.Attempts) > 128 || p.Attempts == nil {
		return
	}
	for attempt, cursor := range p.Attempts {
		if !idPattern.MatchString(attempt) || cursor.Seq == 0 || cursor.Seq > 9007199254740991 || len(cursor.Hash) != 64 {
			return
		}
		for _, digit := range cursor.Hash {
			if !strings.ContainsRune("0123456789abcdef", digit) {
				return
			}
		}
	}
	if p.Envelope.Collection.LastSuccessAt != nil && !utc(*p.Envelope.Collection.LastSuccessAt) {
		return
	}
	if len(p.Envelope.Data) == 0 || string(p.Envelope.Data) == "null" {
		if p.Current != "" || len(p.Attempts) != 0 || p.Envelope.Collection.LastSuccessAt != nil {
			return
		}
	} else {
		v, err := validateSnapshot(p.Envelope.Data, st.run.ID)
		if err != nil || v.Attempt != p.Current {
			return
		}
		cur, ok := p.Attempts[v.Attempt]
		if !ok || cur.Seq != v.Seq || cur.Hash != fmt.Sprintf("%x", sha256.Sum256(v.Canonical)) {
			return
		}
	}
	p.Envelope.Collection.State = "unreachable"
	p.Envelope.Collection.ObservedAt = now()
	p.Envelope.Collection.Window = "bounded_tail"
	p.Envelope.Collection.Error = &SafeError{Code: "not_collected", Message: "Source has not been collected"}
	st.saved = p
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		writeError(w, 405, "method_not_allowed")
		return
	}
	if r.URL.Path == "/healthz" {
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	if s.auth {
		supplied := r.Header.Get("Authorization")
		digest := sha256.Sum256([]byte(strings.TrimPrefix(supplied, "Bearer ")))
		if !strings.HasPrefix(supplied, "Bearer ") || subtle.ConstantTimeCompare(digest[:], s.token[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, 401, "unauthorized")
			return
		}
	}
	if r.URL.Path == "/v1/runs" {
		runs := make([]map[string]string, 0, len(s.config.Runs))
		for _, r := range s.config.Runs {
			runs = append(runs, map[string]string{"id": r.ID, "title": r.Title})
		}
		writeJSON(w, map[string]any{"runs": runs})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "runs" {
		writeError(w, 404, "not_found")
		return
	}
	st := s.runs[parts[2]]
	if st == nil {
		writeError(w, 404, "not_found")
		return
	}
	switch parts[3] {
	case "snapshot":
		st.mu.RLock()
		defer st.mu.RUnlock()
		writeJSON(w, st.saved.Envelope)
	case "manifest":
		if len(st.run.Manifest) > 0 {
			writeJSON(w, st.run.Manifest)
		} else {
			writeJSON(w, map[string]any{"schema_version": "1.0", "run_id": st.run.ID, "title": st.run.Title, "execution": "sequential", "tasks": []any{}})
		}
	default:
		writeError(w, 404, "not_found")
	}
}
func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": SafeError{Code: code, Message: http.StatusText(status)}})
}
