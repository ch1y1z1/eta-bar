package backend

import (
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

const pathCacheLimit = 64

type pathEntry struct {
	state *runState
	used  uint64
}

func safeAbsolutePath(path string) bool {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
func underRoot(path, root string) bool {
	root = filepath.Clean(root)
	return strings.HasPrefix(path, root+"/")
}

// Only profile names cross the API boundary; the caller cannot choose SSH
// destinations, credentials, commands, or local permission roots.
func (s *Server) pathSource(host, path string) (Source, bool) {
	if !safeAbsolutePath(path) {
		return Source{}, false
	}
	path = filepath.Clean(path)
	source := Source{Mode: "ssh", Host: host, LogPath: path}
	if host == "local" {
		source.Mode = "local"
		for _, root := range s.config.LocalAllowedRoots {
			if underRoot(path, root) {
				source.localRoot = filepath.Clean(root)
				return source, true
			}
		}
		return Source{}, false
	}
	h, ok := s.config.Hosts[host]
	return source, ok && underRoot(path, h.AllowedRoot)
}

func parsePathQuery(r *http.Request) ([2]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 2 || len(q["host"]) != 1 || len(q["path"]) != 1 || q.Get("host") == "" || q.Get("path") == "" {
		return [2]string{}, errors.New("invalid source")
	}
	return [2]string{q.Get("host"), q.Get("path")}, nil
}

func (s *Server) pathSnapshot(w http.ResponseWriter, r *http.Request) {
	q, err := parsePathQuery(r)
	if err != nil {
		writeError(w, 400, "invalid_source")
		return
	}
	source, ok := s.pathSource(q[0], q[1])
	if !ok {
		writeError(w, 400, "invalid_source")
		return
	}
	// Serialize on-demand reads so neither in-flight work nor retained lifecycle
	// state can grow with request concurrency. No collectors or disk files are added.
	s.pathsMu.Lock()
	defer s.pathsMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	if s.paths == nil {
		s.paths = make(map[Source]*pathEntry)
	}
	entry := s.paths[source]
	if entry == nil {
		if len(s.paths) >= pathCacheLimit {
			var oldest Source
			used := ^uint64(0)
			for key, e := range s.paths {
				if e.used < used {
					oldest, used = key, e.used
				}
			}
			delete(s.paths, oldest)
		}
		entry = &pathEntry{state: &runState{run: Run{Source: source}, onDemand: true,
			saved: checkpoint{Attempts: map[string]Cursor{}, Envelope: Envelope{Collection: Collection{Window: "bounded_tail"}}}}}
		s.paths[source] = entry
	}
	s.pathClock++
	entry.used = s.pathClock
	s.poll(r.Context(), entry.state)
	writeJSON(w, entry.state.saved.Envelope)
}
