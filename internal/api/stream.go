// /stream.ts (TS-поток, Annex-B → remux ffmpeg) и /prime (прогрев).
// Порт _stream_ts / _prime / _stream_gen / _ts_remux из app.py.
package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"time"

	"hikcloud2gortc/internal/worker"
)

// attachRing — подписка на кольцо воркера (на ctx запроса).
// Возвращает канал новых записей и стартовый снимок.
func (s *Server) attachRing(ctx context.Context, w *worker.Worker) (<-chan []byte, [][]byte) {
	ch, _, _, snap := w.Subscribe(ctx)
	return ch, snap
}

// mergeRing — снимок + новые записи в один канал; закрывается при ctx.
func mergeRing(ctx context.Context, snap [][]byte, sub <-chan []byte) <-chan []byte {
	out := make(chan []byte, 128)
	go func() {
		defer close(out)
		for _, b := range snap {
			select {
			case out <- b:
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-sub:
				if !ok {
					return
				}
				select {
				case out <- b:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// detectAnnexBCodec — H.264 vs HEVC по первому NAL (как в Python).
func detectAnnexBCodec(head []byte) string {
	idx := -1
	for i := 0; i+4 <= len(head); i++ {
		if head[i] == 0 && head[i+1] == 0 && head[i+2] == 0 && head[i+3] == 1 {
			idx = i
			break
		}
	}
	sc := 4
	if idx == -1 {
		for i := 0; i+3 <= len(head); i++ {
			if head[i] == 0 && head[i+1] == 0 && head[i+2] == 1 {
				idx = i
				sc = 3
				break
			}
		}
	}
	if idx != -1 && idx+sc < len(head) {
		b0 := head[idx+sc]
		htype := (b0 >> 1) & 0x3F
		switch htype {
		case 19, 20, 21, 16, 17, 18, 32, 33, 34, 35, 36:
			return "hevc"
		}
		if b0&0x1F >= 1 && b0&0x1F <= 9 {
			return "h264"
		}
	}
	return "hevc" // парк NVR гоняет HEVC
}

// clientWriteTimeout — дедлайн на запись в клиент: если зритель перестал
// читать (зависший/мёртвый клиент), TCP-буфер заполнится, запись упрётся
// в дедлайн и handler завершится → воркер освобождается reaper'ом.
// 30 с — с большим запасом над GOP (~4 с) и медленным интернетом зрителя.
const clientWriteTimeout = 30 * time.Second

// remuxToTS — Annex-B → MPEG-TS через ffmpeg (pipe), стримим в клиент.
func (s *Server) remuxToTS(ctx context.Context, w http.ResponseWriter, fl http.Flusher, codec string, head []byte, rest <-chan []byte) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32768", "-analyzeduration", "100",
		"-f", codec, "-i", "pipe:0", "-c", "copy", "-f", "mpegts", "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeErr(w, httpErr(500, "ffmpeg: %v", err))
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, httpErr(500, "ffmpeg: %v", err))
		return
	}
	if err := cmd.Start(); err != nil {
		s.log.Warn("ffmpeg remux: не запустился", "err", err)
		writeErr(w, httpErr(502, "ffmpeg remux: %v", err))
		return
	}
	go func() {
		defer stdin.Close()
		if _, err := stdin.Write(head); err != nil {
			return
		}
		for b := range rest {
			if _, err := stdin.Write(b); err != nil {
				return
			}
		}
	}()
	// копия ffmpeg stdout → клиент с дедлайном записи (вместо io.Copy):
	// зависший клиент не должен держать ffmpeg и воркер вечно
	rc := http.NewResponseController(w)
	buf := make([]byte, 256*1024)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				s.log.Warn("stream.ts (remux): клиент не читает — закрываю", "err", werr)
				break
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait() // забираем процесс, иначе ffmpeg остаётся зомби
}

// handleStreamTS — GET /stream.ts?channel=&serial=&stream=
func (s *Server) handleStreamTS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	channel, cerr := qChannel(q)
	if cerr != nil {
		writeErr(w, cerr)
		return
	}
	stream := q.Get("stream")
	if stream != "sub" && stream != "main" && stream != "auto" {
		stream = s.cfg.DefaultStream
		if stream != "sub" && stream != "main" && stream != "auto" {
			stream = "sub"
		}
	}
	serial := q.Get("serial")
	sn, ch, err := s.resolve(channel, serial)
	if err != nil {
		writeErr(w, err)
		return
	}
	wk, err := s.ensureWorker(r.Context(), sn, ch, stream)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.mgr.Touch(wk)
	defer s.mgr.Untouch(wk)

	subCh, snap := s.attachRing(r.Context(), wk)
	if len(snap) == 0 {
		writeErr(w, fmt.Errorf("кольцо пусто"))
		return
	}
	merged := mergeRing(r.Context(), snap[1:], subCh)
	first := snap[0]

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	fl, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)

	if len(first) > 0 && first[0] == 0x47 {
		// воркер уже отдаёт MPEG-TS — pass-through
		s.log.Info("stream.ts: TS pass-through", "sn", wk.SN)
		for {
			select {
			case <-r.Context().Done():
				return
			case b, ok := <-merged:
				if !ok {
					return
				}
				// проверка клиента: дедлайн записи (мёртвый зритель не должен
				// держать воркер с VTM-сессией вечно)
				_ = rc.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
				if _, err := w.Write(b); err != nil {
					s.log.Warn("stream.ts: клиент не читает — закрываю", "sn", wk.SN, "err", err)
					return
				}
				if fl != nil {
					fl.Flush()
				}
			}
		}
	}
	codec := detectAnnexBCodec(first)
	s.log.Info("stream.ts: Annex-B remux", "sn", wk.SN, "codec", codec, "head", len(first))
	s.remuxToTS(r.Context(), w, fl, codec, first, merged)
}

// handlePrime — GET /prime?channel=&serial=&stream=main
func (s *Server) handlePrime(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	channel, cerr := qChannel(q)
	if cerr != nil {
		writeErr(w, cerr)
		return
	}
	stream := q.Get("stream")
	if stream != "sub" && stream != "main" {
		stream = "main"
	}
	serial := q.Get("serial")
	sn, ch, err := s.resolve(channel, serial)
	if err != nil {
		writeErr(w, err)
		return
	}
	t0 := time.Now()
	wk, err := s.ensureWorker(r.Context(), sn, ch, stream)
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = wk
	writeJSON(w, map[string]any{
		"warm":        true,
		"waited_s":    round2(time.Since(t0).Seconds()),
		"worker_mode": s.mgr.Mode(),
	})
}

// qInt — int из query-параметра (0 при мусоре).
func qInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// qChannel — channel из query: пустой → 0 (дефолтная камера),
// валидное число → оно, мусор → 400 (не молчаливый дефолт).
func qChannel(q url.Values) (int, error) {
	v := q.Get("channel")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, httpErr(400, "channel: некорректное значение %q (нужно целое ≥ 0)", v)
	}
	return n, nil
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
