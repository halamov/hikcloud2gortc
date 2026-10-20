// /frame — JPEG с live-потока: первый INTRA-кадр, декодированный ffmpeg.
// Порт /frame из app.py.
package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"hikcloud2gortc/internal/worker"
)

// handleFrame — GET /frame?channel=&serial=&stream=&max_wait=60
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	channel := qInt(q.Get("channel"))
	stream := q.Get("stream")
	if stream != "sub" && stream != "main" {
		stream = "main"
	}
	maxWait := qInt(q.Get("max_wait"))
	if maxWait <= 0 {
		maxWait = 60
	}
	serial := q.Get("serial")
	sn, ch, err := s.resolve(channel, serial)
	if err != nil {
		writeErr(w, err)
		return
	}
	wk, err := s.ensureWorker(r.Context(), sn, ch, stream)
	if err != nil {
		s.offlineFail(w, r, err, stream, ch)
		return
	}
	s.mgr.Touch(wk)
	defer s.mgr.Untouch(wk)

	var jpeg []byte
	var fail error
	for attempt := 1; attempt <= 2; attempt++ {
		jpeg, fail = s.captureFrame(r.Context(), wk, maxWait)
		if len(jpeg) > 0 {
			break
		}
		s.log.Warn("frame: попытка без кадра", "sn", wk.SN, "attempt", attempt, "err", fail)
		if attempt == 1 {
			time.Sleep(3 * time.Second) // облако может восстановиться
		}
	}
	if len(jpeg) == 0 {
		if fail == nil {
			fail = fmt.Errorf("no keyframe (I-frame) in stream window")
		}
		s.offlineFail(w, r, fail, stream, ch)
		return
	}
	s.log.Info("frame", "sn", wk.SN, "stream", stream, "bytes", len(jpeg))
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(jpeg)
}

// offlineFail — OFFLINE_RESPONSE: nosignal → 200 + NO SIGNAL jpeg, иначе 5xx.
func (s *Server) offlineFail(w http.ResponseWriter, r *http.Request, err error, stream string, ch int) {
	code := 502
	msg := err.Error()
	if ae, ok := err.(*apiError); ok {
		code, msg = ae.code, ae.msg
	}
	if s.cfg.OfflineResponse == "nosignal" {
		W, H := 1920, 1080
		if stream == "sub" {
			W, H = 640, 360
		}
		if s.offlineJPEGResponse(w, code, msg, fmt.Sprintf("CH %02d", ch), W, H) {
			return
		}
	}
	writeErr(w, fmt.Errorf("code %d: %s", code, msg))
}

// captureFrame — снять первый I-кадр из кольца воркера (ffmpeg-декодер).
func (s *Server) captureFrame(ctx context.Context, wk *worker.Worker, maxWait int) ([]byte, error) {
	subCh, snap := s.attachRing(ctx, wk)
	if len(snap) == 0 {
		return nil, fmt.Errorf("кольцо пусто")
	}
	first := snap[0]
	format := "mpegts"
	if len(first) > 0 && first[0] != 0x47 {
		format = detectAnnexBCodec(first)
	}
	tmp, err := os.CreateTemp("", "frame_*.jpg")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	os.Remove(tmp.Name()) // ffmpeg создаст заново

	ctx2, cancel := context.WithTimeout(ctx, time.Duration(maxWait)*time.Second)
	defer cancel()

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32768", "-analyzeduration", "100",
		"-f", format, "-i", "pipe:0",
		"-vf", "select='eq(pict_type,I)'",
		"-frames:v", "1", "-q:v", "2", "-f", "image2", tmp.Name())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	feed := mergeRing(ctx2, snap, subCh)
	go func() {
		defer stdin.Close()
		for {
			select {
			case <-waitCh:
				return
			case b, ok := <-feed:
				if !ok {
					return
				}
				if _, err := stdin.Write(b); err != nil {
					return
				}
			}
		}
	}()

	select {
	case <-waitCh: // ffmpeg нашёл I-кадр и вышел
	case <-ctx2.Done():
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
	}

	out, err := os.ReadFile(tmp.Name())
	os.Remove(tmp.Name())
	if err != nil || len(out) == 0 {
		return nil, fmt.Errorf("no keyframe (I-frame) in stream window")
	}
	return out, nil
}
