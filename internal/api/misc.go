// /health /cameras /workers /logs /offline /keys — сервисные эндпоинты.
package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"hikcloud2gortc/internal/cloudcam"
)

// handleHealth — GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	session := "not logged in yet"
	if s.client.SessionID() != "" {
		session = "live"
	}
	ch := s.cfg.DefaultChannel
	writeJSON(w, map[string]any{
		"status":         "ok",
		"engine":         "hikcloud2gortc (Go, VTM + AES-ECB, RTP/PS/TS)",
		"session":        session,
		"cameras":        "/cameras",
		"stream":         "/stream.ts?channel=" + itoa(ch) + "&stream=" + s.cfg.DefaultStream,
		"stream_fullhd":  "/stream.ts?channel=" + itoa(ch) + "&stream=main",
		"snapshot":       "/snapshot?channel=" + itoa(ch),
		"frame_fullhd":   "/frame?channel=" + itoa(ch) + "&stream=main",
		"prime":          "/prime?channel=" + itoa(ch) + "&stream=main",
		"worker_mode":    s.mgr.Mode(),
		"workers_active": len(s.mgr.List()),
	})
}

// handleCameras — GET /cameras?refresh=0
func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	refresh := qInt(q.Get("refresh"))
	cams, err := s.cameraList(refresh != 0)
	if err != nil {
		var mfa *cloudcam.MfaRequired
		if errors.As(err, &mfa) {
			writeErr(w, httpErr(503, "2FA required (see /keys/request)"))
			return
		}
		writeErr(w, httpErr(502, "camera list failed: %s", err))
		return
	}
	out := make([]map[string]any, 0, len(cams))
	for _, c := range cams {
		out = append(out, map[string]any{
			"serial":  c.Serial,
			"channel": c.Channel,
			"name":    c.Name,
		})
	}
	writeJSON(w, out)
}

// handleWorkers — GET /workers
func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"mode": s.mgr.Mode(), "workers": s.mgr.List()})
}

// handleLogs — GET /logs?worker=&tail=100
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	workerName := q.Get("worker")
	tail := qInt(q.Get("tail"))
	if tail < 1 {
		tail = 100
	}
	if tail > 2000 {
		tail = 2000
	}
	logDir := s.logDir()
	var name string
	if workerName != "" {
		if !strings.HasPrefix(workerName, "worker_") || strings.Contains(workerName, "/") || strings.Contains(workerName, "..") {
			writeErr(w, httpErr(400, "worker must be worker_<serial>_<ch>_<mode>"))
			return
		}
		name = workerName + ".log"
	} else {
		name = "app.log"
	}
	path := filepath.Join(logDir, name)
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, httpErr(404, "log file %s not found", name))
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	size := fi.Size()
	start := size - 512*1024
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err.Error() != "unexpected EOF" {
		writeErr(w, httpErr(500, "read log failed: %s", err))
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(buf), "\r", ""), "\n")
	if tail > len(lines) {
		tail = len(lines)
	}
	writeJSON(w, map[string]any{
		"file":  name,
		"size":  size,
		"lines": lines[len(lines)-tail:],
	})
}

// handleOffline — GET /offline
func (s *Server) handleOffline(w http.ResponseWriter, r *http.Request) {
	var offline []map[string]any
	for _, st := range s.mgr.List() {
		if st.Offline || st.State == "error" {
			offline = append(offline, map[string]any{
				"sn":         st.SN,
				"state":      st.State,
				"offline":    st.Offline,
				"key_error":  st.KeyError,
				"last_error": st.LastErr,
			})
		}
	}
	if offline == nil {
		offline = []map[string]any{}
	}
	writeJSON(w, map[string]any{"ok": true, "offline": offline})
}

// handleKeysRequest — POST /keys/request?channel=&serial=
func (s *Server) handleKeysRequest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serial := q.Get("serial")
	channel := qInt(q.Get("channel"))
	if serial == "" {
		writeErr(w, httpErr(400, "serial required"))
		return
	}
	if err := s.client.RefreshSession(true); err != nil {
		writeErr(w, httpErr(502, "cloud session: %s", err))
		return
	}
	key, err := s.client.GetCamKey(serial, channel, "")
	if err != nil {
		var mfa *cloudcam.MfaRequired
		if errors.As(err, &mfa) {
			if err := s.client.SendMfaCode(); err != nil {
				writeErr(w, httpErr(502, "mfa send failed: %s", err))
				return
			}
			writeJSON(w, map[string]any{
				"ok":  true,
				"mfa": "code sent to account email; POST /keys/set?serial=&channel=&code=",
			})
			return
		}
		writeErr(w, httpErr(502, "key request failed: %s", err))
		return
	}
	if err := s.mgr.SaveCamKey(serial, channel, key); err != nil {
		writeErr(w, httpErr(500, "save key failed: %s", err))
		return
	}
	s.mgr.Restart(serial, channel)
	writeJSON(w, map[string]any{"ok": true, "stored": true})
}

// handleKeysSet — POST /keys/set?channel=&serial=&code=
func (s *Server) handleKeysSet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serial := q.Get("serial")
	channel := qInt(q.Get("channel"))
	code := q.Get("code")
	if serial == "" {
		writeErr(w, httpErr(400, "serial required"))
		return
	}
	if err := s.client.RefreshSession(true); err != nil {
		writeErr(w, httpErr(502, "cloud session: %s", err))
		return
	}
	key, err := s.client.GetCamKey(serial, channel, code)
	if err != nil {
		var mfa *cloudcam.MfaRequired
		if errors.As(err, &mfa) {
			writeErr(w, httpErr(400, "2FA code missing/invalid"))
			return
		}
		writeErr(w, httpErr(502, "key set failed: %s", err))
		return
	}
	if err := s.mgr.SaveCamKey(serial, channel, key); err != nil {
		writeErr(w, httpErr(500, "save key failed: %s", err))
		return
	}
	s.mgr.Restart(serial, channel)
	writeJSON(w, map[string]any{"ok": true, "stored": true})
}

// logDir — каталог логов (== dataDir, как LOG_DIR в Python).
func (s *Server) logDir() string { return s.mgr.DataDir() }
