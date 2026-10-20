// Worker — «вечно читающий» воркер: держит облачный поток одной камеры
// открытым постоянно и пишет расшифрованные данные в общий кольцевой лог.
// Порт _Worker из decrypt_proxy.py.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hikcloud2gortc/internal/cloudcam"
	"hikcloud2gortc/internal/decode"
)

const (
	streamingState = "streaming"
	startingState  = "starting"
	errorState     = "error"
)

// camKeysFileLoad — {"SERIAL:CH": key} | {serial: key} | [].
func loadCamKeys(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	// простой JSON: key — строка. Формат: {"key": "value"}
	// парсим стандартной json-библиотекой
	if err := jsonUnmarshalCamKeys(b, out); err != nil {
		return out
	}
	return out
}

// Worker — одна (серия, канал, режим) с постоянным облачным reader.
type Worker struct {
	SN      string
	Serial  string
	Channel int
	Mode    string
	Key     string

	client   *cloudcam.Client
	mgr      *Manager
	cache    *cloudcam.VtmCache
	cacheDir string
	pace     bool // PACE_FRAMES: сглаживать GOP-bursts (PacedWriter)

	ring *Ring

	mu      sync.Mutex
	state   string
	lastErr string
	offline bool
	keyErr  bool
	annexb  bool
	codec   string
	format  string // rtp|ps|ts|none
	restarts int
	openedAt time.Time
	startedAt time.Time

	active   int
	lastUse  time.Time

	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// OpenSession — результат одного открытого облачного потока.
type OpenSession struct {
	stream    *cloudcam.Stream
	transform func(b []byte) []byte
	primed    [][]byte
	keyerr    func() bool
	pacer     *PacedWriter // nil — если pass-through TS (без дешифрования)
}

func newWorker(m *Manager, serial string, channel int, mode string) *Worker {
	key := m.CamKey(serial, channel)
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		SN:        serial + "#channel" + strconv.Itoa(channel),
		Serial:    serial,
		Channel:   channel,
		Mode:      mode,
		Key:       key,
		client:    m.client,
		mgr:       m,
		cache:     m.cache,
		cacheDir:  m.dataDir,
		pace:      m.pace,
		ring:      NewRing(),
		state:     startingState,
		startedAt: time.Now(),
		lastUse:   time.Now(),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	// отдельный лог воркера: worker_<serial>_<ch>_<mode>.log (как в Python)
	logPath := m.dataDir + "/worker_" + serial + "_" + strconv.Itoa(channel) + "_" + mode + ".log"
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		w.log = slog.New(slog.NewTextHandler(f, nil)).With("sn", w.SN)
	} else {
		w.log = m.log
	}
	return w
}

func (w *Worker) setState(state, err string) {
	w.mu.Lock()
	w.state = state
	if err != "" {
		w.lastErr = err
	}
	w.mu.Unlock()
}

// --- открытие сессии -------------------------------------------------------

// primePackets — прочитать начало потока, определить формат (+codec при RTP).
func primePackets(stream *cloudcam.Stream, ctx context.Context) ([][]byte, bool, string) {
	var buffered [][]byte
	var isRTP *bool
	codec := ""
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case b, ok := <-stream.Packets():
			if !ok {
				break loop
			}
			if isRTP == nil {
				v := len(b) >= 1 && b[0]>>6 == 2
				isRTP = &v
			}
			buffered = append(buffered, b)
			if *isRTP {
				if len(b) >= 2 && b[1]&0x7F == decode.HevcVideoPT {
					pl, err := decode.RtpPayload(b)
					if err == nil && len(pl) >= 1 {
						if c := decode.DetectRtpCodec(pl[0]); c != "" {
							codec = c
							break loop
						}
					}
				}
			} else {
				break loop // PS/TS: codec не нужен — первый пакет
			}
		}
		if len(buffered) >= 400 {
			break
		}
	}
	rtp := isRTP != nil && *isRTP
	return buffered, rtp, codec
}

// openSessionOnce — одна попытка (auto-проба или зафиксированный режим).
// Возвращает сессию либо nil (пробный режим исчерпан / ошибка).
func (w *Worker) openSessionOnce() *OpenSession {
	type attempt struct {
		st   string
		tmo  time.Duration
		probe bool
	}
	// sub-режим: stream_type=2. main: 1. auto: сначала sub-проба, потом main.
	var attempts []attempt
	switch w.Mode {
	case "main":
		attempts = []attempt{{"1", 20 * time.Second, false}}
	case "sub":
		attempts = []attempt{{"2", 20 * time.Second, false}}
	default: // auto
		attempts = []attempt{
			{"2", 6 * time.Second, true},
			{"1", 20 * time.Second, false},
		}
	}
	w.mu.Lock()
	w.restarts++
	w.mu.Unlock()
	for _, at := range attempts {
		st, _ := strconv.Atoi(at.st)
		stream, err := w.client.Open(w.ctx, cloudcam.OpenParams{
			Serial:     w.Serial,
			Channel:    w.Channel,
			StreamType: st,
		}, w.cache)
		if err != nil {
			w.classifyOpenErr(err)
			w.setState(errorState, err.Error())
			if !at.probe {
				return nil
			}
			continue // проба не удалась — следующий режим
		}
		// приминг: формат (+codec)
		pctx, pcancel := context.WithTimeout(context.Background(), at.tmo)
		primed, isRTP, codec := primePackets(stream, pctx)
		pcancel()
		if len(primed) == 0 {
			stream.Close()
			err := errors.New("пробное окно не дало пакетов")
			w.classifyOpenErr(err)
			w.setState(errorState, err.Error())
			if at.probe {
				continue
			}
			return nil
		}
		// формат + трансформ
		format := decode.DetectTransport(primed[0])
		if isRTP {
			format = "rtp"
		}
		var dec *decode.NalBase
		tr := func(b []byte) []byte { return b }
		switch {
		case isRTP:
			if codec == "hevc" {
				d := decode.NewHevcRtp(w.Key, w.cacheDir, w.SN)
				dec = d.NalBase
				tr = d.Feed
			} else {
				d := decode.NewH264Rtp(w.Key, w.cacheDir, w.SN)
				dec = d.NalBase
				tr = d.Feed
			}
		case format == "ps":
			d := decode.NewPs(w.Key, w.cacheDir, w.SN)
			dec = d.NalBase
			tr = d.Feed
		default: // ts / unknown — pass-through
			format = "ts"
		}
		annexb := dec != nil
		w.mu.Lock()
		w.format = format
		w.annexb = annexb
		w.codec = codec
		w.openedAt = time.Now()
		w.lastErr = ""
		w.offline = false
		w.keyErr = false
		w.mu.Unlock()
		w.ring.Begin(annexb, codec)
		w.setState(streamingState, "")
		// Pacing — только для Annex-B (дешифрованный поток): PacedWriter
		// разбивает его на кадры и выдаёт в кольцо с измеренной скоростью.
		// Pass-through TS через пейсер не гоняем (NAL-границ в TS нет —
		// буферизация до 2 МБ накрутила бы задержку).
		var pacer *PacedWriter
		if annexb {
			pacer = NewPacedWriter(w.ring, codec, w.pace).Start()
		}
		return &OpenSession{
			stream:    stream,
			transform: tr,
			primed:    primed,
			keyerr:    func() bool { return dec != nil && dec.KeyError },
			pacer:     pacer,
		}
	}
	return nil
}

// classifyOpenErr — offline/маршрут (не чистить кеш) vs прочее (чистить).
func (w *Worker) classifyOpenErr(err error) {
	s := strings.ToLower(err.Error())
	offline := strings.Contains(s, "offline") || strings.Contains(s, "unreachable") ||
		strings.Contains(s, "dial") || strings.Contains(s, "no route")
	if strings.Contains(s, "could not find vtm resource") {
		// маршрут устарел — инвалидируем кеш
		if w.cache != nil {
			w.cache.Invalidate()
		}
		offline = false
	}
	w.mu.Lock()
	w.offline = offline
	w.mu.Unlock()
}

// --- главный цикл ----------------------------------------------------------

func (w *Worker) run() {
	defer close(w.done)
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("panic в воркере — снимаю из реестра", "err", r)
			if w.mgr != nil {
				w.mgr.remove(w)
			}
		}
	}()
	backoff := 2.0
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		// idle-стоп обрабатывает менеджер (cancel)
		sess := w.openSessionOnce()
		if sess == nil {
			w.log.Warn("сессия не открыта", "err", w.Status().LastErr)
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(time.Duration(backoff * float64(time.Second))):
			}
			backoff = math.Min(backoff*2, 30.0)
			continue
		}
		backoff = 2.0
		w.log.Info("сессия открыта", "format", w.format, "codec", w.codec, "mode", w.Mode)
		// прайм-пакеты — тоже часть потока
		for _, b := range sess.primed {
			if data := sess.transform(b); len(data) > 0 {
				if sess.pacer != nil {
					sess.pacer.Feed(data)
				} else {
					w.ring.Append(data)
				}
			}
		}
		// Помпа: пока поток не оборвался. break внутри select вырывает только
		// select, поэтому выход из цикла — через метку pump.
	pump:
		for {
			select {
			case <-w.ctx.Done():
				if sess.pacer != nil {
					sess.pacer.Close(2 * time.Second)
				}
				sess.stream.Close()
				return
			case b, ok := <-sess.stream.Packets():
				if !ok {
					break pump
				}
				if sess.keyerr() {
					w.mu.Lock()
					w.keyErr = true
					w.mu.Unlock()
					break pump
				}
				if data := sess.transform(b); len(data) > 0 {
					if sess.pacer != nil {
						sess.pacer.Feed(data)
					} else {
						w.ring.Append(data)
					}
				}
			}
		}
		// сессия оборвалась: дослать хвост очереди пейсера в кольцо
		if sess.pacer != nil {
			sess.pacer.Close(5 * time.Second)
		}
		sess.stream.Close()
		w.log.Info("сессия закрыта, рестарт", "backoff_s", math.Min(backoff, 5.0))
		// сессия оборвалась — быстрый рестарт
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(time.Duration(math.Min(backoff, 5.0) * float64(time.Second))):
		}
	}
}

// Subscribe — подписка на кольцо (делегирует ring).
func (w *Worker) Subscribe(ctx context.Context) (<-chan []byte, int, int, [][]byte) {
	return w.ring.Subscribe(ctx)
}

// WaitReady — ждать первые данные (делегирует ring).
func (w *Worker) WaitReady(ctx context.Context) bool {
	return w.ring.WaitReady(ctx)
}

// Codec — кодировщик после автоопределения.
func (w *Worker) Codec() string { return w.ring.Codec() }

// IsAnnexB — формат в кольце (annexb vs rtp).
func (w *Worker) IsAnnexB() bool { return w.ring.IsAnnexB() }

// LastData — время последних данных.
func (w *Worker) LastData() time.Time { return w.ring.LastData() }

// Stop — остановка воркера.
func (w *Worker) Stop() {
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
	}
}

// --- статус ----------------------------------------------------------------

type Status struct {
	SN         string `json:"sn"`
	Serial     string `json:"serial"`
	Channel    int    `json:"channel"`
	Mode       string `json:"mode"`
	State      string `json:"state"`
	Codec      string `json:"codec"`
	Format     string `json:"format"`
	AnnexB     bool   `json:"annexb"`
	Active     int    `json:"active"`
	IdleSec    int    `json:"idle_s"`
	UptimeSec  int    `json:"uptime"`
	Restarts   int    `json:"restarts"`
	Seq        int    `json:"seq"`
	Offline    bool   `json:"offline"`
	KeyError   bool   `json:"key_error"`
	LastErr    string `json:"last_error"`
}

func (w *Worker) Status() Status {
	seq, _, _, codec, _ := w.ring.Stats()
	w.mu.Lock()
	defer w.mu.Unlock()
	idle := 0
	if !w.lastUse.IsZero() {
		idle = int(time.Since(w.lastUse).Seconds())
	}
	return Status{
		SN:        w.SN,
		Serial:    w.Serial,
		Channel:   w.Channel,
		Mode:      w.Mode,
		State:     w.state,
		Codec:     codec,
		Format:    w.format,
		AnnexB:    w.annexb,
		Active:    w.active,
		IdleSec:   idle,
		UptimeSec: int(time.Since(w.startedAt).Seconds()),
		Restarts:  w.restarts,
		Seq:       seq,
		Offline:   w.offline,
		KeyError:  w.keyErr,
		LastErr:   w.lastErr,
	}
}

// CamKeyLookup — загрузка cam_keys.json.
func jsonUnmarshalCamKeys(b []byte, out map[string]string) error {
	// json-импорт здесь, чтобы не дублировать
	var raw map[string]any
	if err := jsonUnmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		}
	}
	return nil
}
