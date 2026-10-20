// /snapshot (облачный снапшот) и /preview (сетка снапшотов всех камер).
// Порт /snapshot + /preview из app.py (PIL → image + x/image/draw).
package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/image/draw"

	"hikcloud2gortc/internal/cloudcam"
)

// handleSnapshot — GET /snapshot?channel=
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	channel := qInt(r.URL.Query().Get("channel"))
	cams, err := s.snap.ListCameras()
	if err != nil {
		s.snapshotFail(w, r, channel, err)
		return
	}
	if len(cams) == 0 {
		s.snapshotFail(w, r, channel, httpErr(502, "no active cameras in cloud"))
		return
	}
	var cam *cloudcam.Camera
	if channel <= 0 {
		cam = &cams[0]
	} else {
		for i, c := range cams {
			if c.Channel == channel {
				cam = &cams[i]
				break
			}
		}
		if cam == nil {
			s.snapshotFail(w, r, channel, httpErr(404, "channel %d not found", channel))
			return
		}
	}
	data, err := s.snap.Capture(*cam, "")
	if err != nil {
		s.snapshotFail(w, r, channel, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(data)
}

func (s *Server) snapshotFail(w http.ResponseWriter, r *http.Request, channel int, err error) {
	code := 502
	msg := err.Error()
	if ae, ok := err.(*apiError); ok {
		code, msg = ae.code, ae.msg
	}
	if s.cfg.OfflineResponse == "nosignal" &&
		code != 400 && code != 404 && code != 405 && code != 409 && code != 422 && code != 429 {
		if s.offlineJPEGResponse(w, code, msg, "", 352, 288) {
			return
		}
	}
	writeErr(w, &apiError{code: code, msg: "snapshot failed: " + msg})
}

// pickCols — колонки под n камер (как _preview_pick_cols).
func pickCols(n int) int {
	if n <= 5 {
		return max(1, n)
	}
	maxCols := min(8, n-1)
	// разложимые: самый «квадратный» делитель с cols >= rows
	best := 0
	bestDiff, bestD := math.MaxInt, math.MaxInt
	for d := 2; d <= maxCols; d++ {
		if n%d == 0 && d >= n/d {
			diff := absd(d - n/d)
			if diff < bestDiff || (diff == bestDiff && d < bestD) {
				best, bestDiff, bestD = d, diff, d
			}
		}
	}
	if best > 0 {
		return best
	}
	// простые n: минимум «хвоста»
	best = 4
	bestPen, bestDiff := math.MaxInt, math.MaxInt
	for d := 2; d <= maxCols; d++ {
		rows := (n + d - 1) / d
		if rows < 2 {
			continue
		}
		last := n - (rows-1)*d
		pen := 0
		if last == 1 {
			pen = 1
		}
		diff := absd(d - rows)
		if pen < bestPen || (pen == bestPen && diff < bestDiff) {
			best, bestPen, bestDiff = d, pen, diff
		}
	}
	return best
}

func absd(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// cropResize — crop-fill 4:3 + resize (как PIL-тайл).
func cropResize(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	iw, ih := b.Dx(), b.Dy()
	target := float64(w) / float64(h)
	var r image.Rectangle
	if float64(iw)/float64(ih) > target {
		nw := int(float64(ih) * target)
		r = image.Rect(b.Min.X+(iw-nw)/2, b.Min.Y, b.Min.X+(iw+nw)/2, b.Max.Y)
	} else {
		nh := int(float64(iw) / target)
		r = image.Rect(b.Min.X, b.Min.Y+(ih-nh)/2, b.Max.X, b.Min.Y+(ih+nh)/2)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, r, draw.Over, nil)
	return dst
}

// tile — кадр или NO SIGNAL.
func tile(w, h int, data []byte, seed int64, bigSize, smSize float64) *image.RGBA {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8 {
		if src, _, err := image.Decode(bytes.NewReader(data)); err == nil {
			return cropResize(src, w, h)
		}
	}
	m := noiseTile(w, h, seed)
	cy := h / 2
	drawText(m, w/2, cy-22, "NO SIGNAL", true, bigSize, color.RGBA{120, 120, 128, 255})
	drawText(m, w/2, cy+14, "камера оффлайн", false, smSize, color.RGBA{90, 90, 96, 255})
	return m
}

// handlePreview — GET /preview?cell=320&workers=8&quality=88
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cell := qInt(q.Get("cell"))
	if cell < 120 || cell > 640 {
		cell = 320
	}
	workersN := qInt(q.Get("workers"))
	if workersN < 1 || workersN > 16 {
		workersN = 8
	}
	quality := qInt(q.Get("quality"))
	if quality < 40 || quality > 95 {
		quality = 88
	}

	cams, err := s.snap.ListCameras()
	if err != nil {
		writeErr(w, httpErr(502, "camera list failed: %s", err))
		return
	}
	if len(cams) == 0 {
		writeErr(w, httpErr(502, "no active cameras in cloud"))
		return
	}

	PW := cell
	PH := max(90, int(float64(PW)*3/4))
	LABEL_H := max(20, int(float64(PW)*0.09))
	HEAD_H := max(28, int(float64(PW)*0.12))

	type res struct {
		ch   int
		data []byte
		auth bool
	}
	results := map[int][]byte{}
	rejects := 0

	grabAll := func(targets []cloudcam.Camera) int {
		var rej int
		out := make([]res, len(targets))
		var wg sync.WaitGroup
		sem := make(chan struct{}, workersN)
		for i, cam := range targets {
			wg.Add(1)
			go func(i int, cam cloudcam.Camera) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				data, err := s.snap.Capture(cam, "")
				if err != nil {
					s.log.Info("preview: камера не снята", "ch", cam.Channel, "err", err)
					out[i] = res{ch: cam.Channel, auth: isAuthErr(err)}
					return
				}
				out[i] = res{ch: cam.Channel, data: data}
			}(i, cam)
		}
		wg.Wait()
		for _, rr := range out {
			if rr.data != nil {
				results[rr.ch] = rr.data
			}
			if rr.auth {
				rej++
			}
		}
		return rej
	}

	rejects = grabAll(cams)
	if rejects > 0 && rejects*2 >= len(cams) {
		s.log.Warn("preview: auth rejects — re-login, retry", "rejects", rejects)
		if err := s.snap.Login(); err != nil {
			s.log.Warn("preview: re-login failed", "err", err)
		} else {
			var missed []cloudcam.Camera
			for _, c := range cams {
				if _, ok := results[c.Channel]; !ok {
					missed = append(missed, c)
				}
			}
			_ = grabAll(missed)
		}
	}

	n := len(cams)
	cols := pickCols(n)
	rows := (n + cols - 1) / cols
	CW, CH := cols*PW, rows*PH

	canvas := image.NewRGBA(image.Rect(0, 0, CW, CH))
	for x := 0; x < CW; x++ {
		for y := 0; y < CH; y++ {
			canvas.Set(x, y, color.RGBA{8, 8, 10, 255})
		}
	}

	device := ""
	for _, c := range cams {
		if c.DeviceName != "" {
			device = c.DeviceName
			break
		}
	}
	online := 0
	for _, c := range cams {
		if _, ok := results[c.Channel]; ok {
			online++
		}
	}
	title := safeText(device + "  •  " + nowStamp() + "  •  онлайн " + itoa(online) + "/" + itoa(n))

	for i, cam := range cams {
		rr, cc := i/cols, i%cols
		x0, y0 := cc*PW, rr*PH
		t := tile(PW, PH, results[cam.Channel], int64(cam.Channel)*7919,
			math.Max(18, float64(PW)*0.095), math.Max(11, float64(PW)*0.05))
		// вставить тайл (пиксель-копия)
		for x := x0; x < x0+PW && x < CW; x++ {
			for y := y0; y < y0+PH && y < CH; y++ {
				off := 4 * ((y - y0) * PW + (x - x0))
				canvas.Set(x, y, color.RGBA{t.Pix[off], t.Pix[off+1], t.Pix[off+2], t.Pix[off+3]})
			}
		}
		// рамка
		for x := x0; x < x0+PW && x < CW; x++ {
			setPixel(canvas, x, y0, color.RGBA{60, 60, 68, 255})
			setPixel(canvas, x, y0+PH-1, color.RGBA{60, 60, 68, 255})
		}
		for y := y0; y < y0+PH && y < CH; y++ {
			setPixel(canvas, x0, y, color.RGBA{60, 60, 68, 255})
			setPixel(canvas, x0+PW-1, y, color.RGBA{60, 60, 68, 255})
		}
		// нижняя подпись с градиентом
		band := max(LABEL_H, int(float64(LABEL_H)*1.7))
		gradient(canvas, x0, y0+PH-band, PW, band, false)
		fitText(canvas, x0+6, y0+PH-LABEL_H/2,
			safeText("CH "+pad2(cam.Channel)+"  "+cam.Name), true,
			math.Max(13, float64(PW)*0.06), color.RGBA{170, 235, 180, 255}, PW-12)
	}
	// шапка с градиентом
	gradient(canvas, 0, 0, CW, HEAD_H, true)
	drawTextLeft(canvas, 10, int(float64(HEAD_H)*0.45), title, true,
		math.Max(13, float64(PW)*0.06), color.RGBA{245, 235, 150, 255}, CW-20)

	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: quality})
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(buf.Bytes())
}

func setPixel(m *image.RGBA, x, y int, c color.RGBA) {
	b := m.Bounds()
	if x < b.Min.X || x >= b.Max.X || y < b.Min.Y || y >= b.Max.Y {
		return
	}
	m.Set(x, y, c)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func isAuthErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden")
}
