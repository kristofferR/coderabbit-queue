package serve

import (
	"net/http"
	"strconv"
	"strings"
)

// LogTail is a bounded suffix of a fix session log.
type LogTail struct {
	Text      string `json:"text"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Host      string `json:"host,omitempty"`
}

func (s *Server) handleAutofixLog(w http.ResponseWriter, r *http.Request) {
	if !s.allowDashboardRead(w, r) {
		return
	}
	if s.opts.TailLog == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this dashboard cannot read autofix logs"})
		return
	}
	pr, err := strconv.Atoi(r.PathValue("pr"))
	if err != nil || pr <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a positive pull request number is required"})
		return
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	s.mu.RLock()
	st := s.lastState
	s.mu.RUnlock()
	var path, host string
	for key, dispatch := range st.Dispatches {
		gotRepo, gotPR := splitKey(key)
		if strings.EqualFold(gotRepo, repo) && gotPR == pr && dispatch.Live(s.opts.Now().UTC()) {
			path, host = dispatch.Log, hostOf(dispatch.Host)
			break
		}
	}
	if path == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live autofix log for this pull request"})
		return
	}
	tail, err := s.opts.TailLog(r.Context(), repo, path, 128<<10)
	if err != nil {
		message := err.Error()
		if host != "" && !strings.EqualFold(host, s.opts.Host) {
			message = "log is on autofix host " + host + ", not dashboard host " + s.opts.Host + ": " + message
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": message})
		return
	}
	tail.Host = host
	writeJSON(w, http.StatusOK, tail)
}
