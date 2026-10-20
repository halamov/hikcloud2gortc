// HTTP-слой: /health /cameras /workers /logs /stream.ts /prime /frame
// /snapshot /preview /offline /keys — порт эндпоинтов app.py.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hikcloud2gortc/internal/cloudcam"
	"hikcloud2gortc/internal/worker"
)

type Server struct {
	cfg    Config
	client *cloudcam.Client
	snap   *cloudcam.SnapClient
	mgr    *worker.Manager
	log    *slog.Logger

	// кеш списка камер (TTL 600с, пустой — 15с с ретраями)
	camMu    sync.Mutex
	camAt    time.Time
	camTTL   time.Duration
	cameras  []cloudcam.Camera
}

// Config — параметры HTTP-сервиса (из config.Config).
type Config struct {
	Port            int
	DefaultChannel  int
	DefaultStream   string
	OfflineResponse string // "error"|"nosignal"
	TokenMaxAge     time.Duration
}

func NewServer(cfg Config, client *cloudcam.Client, snap *cloudcam.SnapClient, mgr *worker.Manager, log *slog.Logger) *Server {
	if cfg.DefaultStream == "" {
		cfg.DefaultStream = "sub"
	}
	if cfg.OfflineResponse == "" {
		cfg.OfflineResponse = "error"
	}
	return &Server{cfg: cfg, client: client, snap: snap, mgr: mgr, log: log, camTTL: 600 * time.Second}
}

// Handler — маршруты.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /cameras", s.handleCameras)
	mux.HandleFunc("GET /workers", s.handleWorkers)
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("GET /stream.ts", s.handleStreamTS)
	mux.HandleFunc("GET /prime", s.handlePrime)
	mux.HandleFunc("GET /frame", s.handleFrame)
	mux.HandleFunc("GET /snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /preview", s.handlePreview)
	mux.HandleFunc("GET /offline", s.handleOffline)
	mux.HandleFunc("POST /keys/request", s.handleKeysRequest)
	mux.HandleFunc("POST /keys/set", s.handleKeysSet)
	return mux
}

// --- общие хелперы ----------------------------------------------------------

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func httpErr(code int, format string, args ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr — HTTP-ошибка; в режиме nosignal для «офлайн»-состояний —
// обработчик сам решает (сюда только честные ошибки).
func writeErr(w http.ResponseWriter, err error) {
	if ae, ok := err.(*apiError); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ae.code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": ae.msg})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(500)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
}

// resolve — (serial, channel) из ?channel=&serial=.
func (s *Server) resolve(channel int, serial string) (string, int, error) {
	cams, err := s.cameraList(false)
	if err != nil {
		return "", 0, httpErr(502, "список камер: %s", err)
	}
	if len(cams) == 0 {
		return "", 0, httpErr(502, "нет активных камер в облаке")
	}
	if serial != "" {
		var hit []cloudcam.Camera
		for _, c := range cams {
			if strings.EqualFold(c.Serial, serial) {
				hit = append(hit, c)
			}
		}
		if len(hit) == 0 {
			return "", 0, httpErr(404, "устройство %s не в списке облака", serial)
		}
		if channel > 0 {
			var sub []cloudcam.Camera
			for _, c := range hit {
				if c.Channel == channel {
					sub = append(sub, c)
				}
			}
			if len(sub) > 0 {
				hit = sub
			}
		}
		return hit[0].Serial, hit[0].Channel, nil
	}
	ch := channel
	if ch <= 0 {
		if s.cfg.DefaultChannel > 0 {
			ch = s.cfg.DefaultChannel
		} else {
			ch = cams[0].Channel
		}
	}
	for _, c := range cams {
		if c.Channel == ch {
			return c.Serial, c.Channel, nil
		}
	}
	avail := ""
	seen := map[int]bool{}
	for _, c := range cams {
		if !seen[c.Channel] {
			seen[c.Channel] = true
			avail += strconv.Itoa(c.Channel) + " "
		}
	}
	return "", 0, httpErr(404, "канал %d не активен; доступны: %s", ch, avail)
}

// cameraList — активные VTM-каналы (с кешем, как в Python).
func (s *Server) cameraList(force bool) ([]cloudcam.Camera, error) {
	s.camMu.Lock()
	defer s.camMu.Unlock()
	if !force && len(s.cameras) > 0 && time.Since(s.camAt) < s.camTTL {
		return s.cameras, nil
	}
	if err := s.client.RefreshSession(true); err != nil {
		return nil, err
	}
	cams, err := s.client.ListCameras()
	if err != nil {
		return nil, err
	}
	if len(cams) == 0 {
		// временный сбой релея: 3 ретрая с интервалом 2с
		for range 3 {
			time.Sleep(2 * time.Second)
			if cams, err = s.client.ListCameras(); err == nil && len(cams) > 0 {
				break
			}
		}
	}
	ttl := 600 * time.Second
	if len(cams) == 0 {
		ttl = 15 * time.Second
	}
	s.cameras, s.camAt, s.camTTL = cams, time.Now(), ttl
	s.log.Info("список камер", "channels", len(cams), "ttl_s", int(ttl.Seconds()))
	return s.cameras, nil
}

// ensureWorker — воркер + ожидание первых данных (20с: облачная сессия
// + ожидание ключевого кадра может занять >8с на холодном старте).
func (s *Server) ensureWorker(ctx context.Context, serial string, channel int, mode string) (*worker.Worker, error) {
	w := s.mgr.Ensure(serial, channel, mode)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if !w.WaitReady(ctx) {
		st := w.Status()
		if st.KeyError {
			return w, httpErr(503, "media key mismatch (2FA: /keys/request)")
		}
		if st.Offline {
			return w, httpErr(502, "камера офлайн: %s", st.LastErr)
		}
		return w, httpErr(502, "stream worker не запустился")
	}
	return w, nil
}
