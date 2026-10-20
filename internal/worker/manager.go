// Manager — реестр воркеров (ключ serial#ch#mode), репейр idle-воркеров,
// рестарт и полная остановка. Порт WorkerManager.
package worker

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hikcloud2gortc/internal/cloudcam"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Manager — держит воркеры.
type Manager struct {
	mu       sync.Mutex
	workers  map[string]*Worker
	client   *cloudcam.Client
	cache    *cloudcam.VtmCache
	dataDir  string
	mode     string // always|idle
	idleSec  int
	pace     bool // PACE_FRAMES
	log      *slog.Logger
	camKeys  map[string]string
	stopAll  bool
}

func NewManager(client *cloudcam.Client, cache *cloudcam.VtmCache, dataDir, mode string, idleSec int, pace bool, log *slog.Logger) *Manager {
	m := &Manager{
		workers: map[string]*Worker{},
		client:  client,
		cache:   cache,
		dataDir: dataDir,
		mode:    mode,
		idleSec: idleSec,
		pace:    pace,
		log:     log,
		camKeys: loadCamKeys(dataDir + "/cam_keys.json"),
	}
	go m.reaper()
	return m
}

// CamKey — ключ камеры из cam_keys.json ("SERIAL:CH", затем "SERIAL"), fallback — ноль.
func (m *Manager) CamKey(serial string, channel int) string {
	if m == nil {
		return strings.Repeat("0", 32)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.camKeys[serial+":CH"+itoa(channel)]; ok {
		return k
	}
	if k, ok := m.camKeys[serial]; ok {
		return k
	}
	return strings.Repeat("0", 32)
}

func itoa(n int) string { return strconv.Itoa(n) }

func keySN(serial string, channel int, mode string) string {
	return serial + "#channel" + strconv.Itoa(channel) + "#" + mode
}

// Ensure — живого воркера нет? Создаёт и стартует.
func (m *Manager) Ensure(serial string, channel int, mode string) *Worker {
	k := keySN(serial, channel, mode)
	m.mu.Lock()
	if w, ok := m.workers[k]; ok {
		w.mu.Lock()
		w.lastUse = time.Now()
		w.mu.Unlock()
		m.mu.Unlock()
		return w
	}
	m.mu.Unlock()
	w := newWorker(m, serial, channel, mode)
	m.mu.Lock()
	if old, ok := m.workers[k]; ok { // гонка двух Ensure
		m.mu.Unlock()
		return old
	}
	m.workers[k] = w
	m.mu.Unlock()
	go w.run()
	m.log.Info("воркер создан", "sn", w.SN, "mode", mode)
	return w
}

// Touch — отметка активности (уменьшает idle).
func (m *Manager) Touch(w *Worker) {
	w.mu.Lock()
	w.active++
	w.lastUse = time.Now()
	w.mu.Unlock()
}

// Untouch — снятие отметки активности.
func (m *Manager) Untouch(w *Worker) {
	w.mu.Lock()
	if w.active > 0 {
		w.active--
	}
	w.lastUse = time.Now()
	w.mu.Unlock()
}

// Reaper — каждые 15с: останавливает idle-воркеры (idle-режим, не offline).
func (m *Manager) reaper() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		if m.stopAll {
			m.mu.Unlock()
			return
		}
		for k, w := range m.workers {
			w.mu.Lock()
			active := w.active
			offline := w.offline
			idle := time.Since(w.lastUse)
			w.mu.Unlock()
			if m.mode == "idle" && active == 0 && !offline && idle > time.Duration(m.idleSec)*time.Second {
				m.log.Info("воркер остановлен (idle)", "sn", w.SN, "idle_s", int(idle.Seconds()))
				w.Stop()
				delete(m.workers, k)
			}
		}
		m.mu.Unlock()
	}
}

// Restart — перезапуск воркера (смена режима или снос кеша).
func (m *Manager) Restart(serial string, channel int) int {
	m.mu.Lock()
	n := 0
	for k, w := range m.workers {
		if w.Serial == serial && w.Channel == channel {
			m.log.Info("рестарт воркера", "sn", w.SN)
			w.Stop()
			delete(m.workers, k)
			n++
		}
	}
	m.mu.Unlock()
	return n
}

// List — статусы всех воркеров.
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, w.Status())
	}
	return out
}

// Get — по SN.
func (m *Manager) Get(sn string) *Worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[sn]
}

// remove — снять воркера из реестра (после panic/ошибки).
func (m *Manager) remove(w *Worker) {
	m.mu.Lock()
	if m.workers[w.SN] == w {
		delete(m.workers, w.SN)
	}
	m.mu.Unlock()
}

// StopAll — остановка всех воркеров (при выходе из HTTP).
func (m *Manager) StopAll() {
	m.mu.Lock()
	m.stopAll = true
	workers := make([]*Worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.workers = map[string]*Worker{}
	m.mu.Unlock()
	for _, w := range workers {
		w.Stop()
	}
}

// Mode — режим воркеров (always|idle).
func (m *Manager) Mode() string { return m.mode }

// SaveCamKey — запись ключа в cam_keys.json (атомарно) + в кеш.
func (m *Manager) SaveCamKey(serial string, channel int, key string) error {
	path := m.dataDir + "/cam_keys.json"
	kk := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &kk)
	}
	kk[serial+":"+itoa(channel)] = key
	out, err := json.MarshalIndent(kk, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	m.mu.Lock()
	m.camKeys = kk
	m.mu.Unlock()
	return nil
}

// DataDir — каталог данных (для кешей декодера).
func (m *Manager) DataDir() string { return m.dataDir }
