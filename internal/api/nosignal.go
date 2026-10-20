// «NO SIGNAL»-JPEG и графические хелперы (шум, градиенты, текст DejaVu).
// Порт _nosignal_jpeg / _preview_bottom_label / _preview_top_header из app.py.
package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"hikcloud2gortc/fonts"
)

var (
	fFace *opentype.Font
	fBold *opentype.Font
)

func init() {
	f, err := opentype.Parse(fonts.DejaVuSans)
	if err != nil {
		panic("DejaVuSans: " + err.Error())
	}
	fFace = f
	b, err := opentype.Parse(fonts.DejaVuSansBold)
	if err != nil {
		panic("DejaVuSansBold: " + err.Error())
	}
	fBold = b
}

func itoa(n int) string { return strconv.Itoa(n) }

func newFace(bold bool, size float64) (font.Face, error) {
	f := fFace
	if bold {
		f = fBold
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
}

func toRGBA(c color.Color) color.RGBA {
	if r, ok := c.(color.RGBA); ok {
		return r
	}
	r, g, b, a := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// midBaseline — базовая линия для вертикального центрирования (PIL anchor
// "m" — середина между ascender и descender): baseline = cy + (asc-desc)/2.
func midBaseline(cy int, face font.Face) int64 {
	mt := face.Metrics()
	return int64(cy)*64 + int64((mt.Ascent-mt.Descent)/2)
}

// putText — цветной антиалиас-текст напрямую на холст (каноничный
// x/image/font паттерн: Drawer{Dst: холст, Src: uniform(цвет)}).
func putText(m *image.RGBA, dotX, dotY int64, text string, bold bool, size float64, c color.Color) {
	face, err := newFace(bold, size)
	if err != nil {
		return
	}
	defer face.Close()
	d := font.Drawer{
		Dst:  m,
		Src:  image.NewUniform(toRGBA(c)),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.Int26_6(dotX), Y: fixed.Int26_6(dotY)},
	}
	d.DrawString(text)
}

// drawText — текст с центром в (cx, cy) (аналог PIL anchor="mm").
func drawText(m *image.RGBA, cx, cy int, text string, bold bool, size float64, c color.Color) {
	if text == "" {
		return
	}
	face, err := newFace(bold, size)
	if err != nil {
		return
	}
	defer face.Close()
	// ширина по Drawer-измерению через временную маску
	mask := image.NewAlpha(m.Bounds())
	d := font.Drawer{Dst: mask, Face: face}
	wid := int64(d.MeasureString(text))
	dotX := int64(cx)*64 - wid/2
	dotY := midBaseline(cy, face)
	putText(m, dotX, dotY, text, bold, size, c)
}

// drawTextLeft — текст слева (anchor="lm"): x, cy.
func drawTextLeft(m *image.RGBA, x, cy int, text string, bold bool, size float64, c color.Color, maxW int) string {
	face, err := newFace(bold, size)
	if err != nil {
		return ""
	}
	defer face.Close()
	mask := image.NewAlpha(m.Bounds())
	d := font.Drawer{Dst: mask, Face: face}
	limit := int64(maxW) * 64
	for text != "" && int64(d.MeasureString(text)) > limit {
		text = trimRune(text)
	}
	if text == "" {
		return ""
	}
	dotY := midBaseline(cy, face)
	putText(m, int64(x)*64, dotY, text, bold, size, c)
	return text
}

// fit — укоротить до maxW (для _preview_fit).
func fitText(m *image.RGBA, x, cy int, text string, bold bool, size float64, c color.Color, maxW int) {
	face, err := newFace(bold, size)
	if err != nil {
		return
	}
	defer face.Close()
	mask := image.NewAlpha(m.Bounds())
	d := font.Drawer{Dst: mask, Face: face}
	limit := int64(maxW) * 64
	if int64(d.MeasureString(text)) <= limit {
		putText(m, int64(x)*64, midBaseline(cy, face), text, bold, size, c)
		return
	}
	t := text
	for len(t) > 1 && int64(d.MeasureString(t+"…")) > limit {
		t = trimRune(t)
	}
	putText(m, int64(x)*64, midBaseline(cy, face), t+"…", bold, size, c)
}

// trimRune — убрать последний rune строки.
func trimRune(s string) string {
	runes := []rune(s)
	if len(runes) <= 1 {
		return ""
	}
	return string(runes[:len(runes)-1])
}

// safeText — CJK в облачных причинах → '?' (DejaVu без CJK — не «квадраты»-tofu).
// Кириллица/латиница — как есть (DejaVu поддерживает).
func safeText(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); {
		r, size := decodeRune(b[i:])
		if isCJK(r) {
			for j := 0; j < size; j++ {
				b[i+j] = '?'
			}
			i += size
		} else {
			i++
		}
	}
	return string(b)
}

func decodeRune(b []byte) (rune, int) {
	if len(b) == 0 {
		return '?', 1
	}
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c >= 0xC2 && c < 0xE0:
		if len(b) < 2 {
			return '?', 1
		}
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case c >= 0xE0 && c < 0xF0:
		if len(b) < 3 {
			return '?', 1
		}
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	default:
		if len(b) < 4 {
			return '?', 1
		}
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	}
}

func isCJK(r rune) bool {
	return (r >= 0x2E80 && r <= 0x9FFF) || (r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x3000 && r <= 0x30FF)
}

// noiseTile — тёмная подложка с шумом и линиями «помех».
func noiseTile(w, h int, seed int64) *image.RGBA {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x9E3779B9))
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			m.Set(x, y, color.RGBA{22, 22, 26, 255})
		}
	}
	for i := 0; i < w*h/300; i++ {
		x := rng.IntN(w)
		y := rng.IntN(h)
		g := byte(28 + rng.IntN(32))
		m.Set(x, y, color.RGBA{g, g, g, 255})
	}
	for i := 0; i < 5; i++ {
		y := rng.IntN(h)
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{35, 35, 40, 255})
			if y+1 < h {
				m.Set(x, y+1, color.RGBA{35, 35, 40, 255})
			}
		}
	}
	return m
}

// noSignalJPEG — плитка «NO SIGNAL» + код/причина.
func noSignalJPEG(width, height, code int, reason string) []byte {
	if reason == "" {
		reason = "camera offline"
	}
	if len(reason) > 120 {
		reason = reason[:117] + "…"
	}
	m := noiseTile(width, height, int64(width*31+height*7))
	cy := height / 2
	drawText(m, width/2, cy-height*10/100, "NO SIGNAL", true, math.Max(18, float64(width)*0.06), color.RGBA{120, 120, 128, 255})
	drawText(m, width/2, cy-height*2/100, "камера оффлайн", false, math.Max(13, float64(width)*0.042), color.RGBA{90, 90, 96, 255})
	errLine := ""
	if code > 0 {
		errLine = "error " + itoa(code)
	}
	if reason != "" {
		if errLine != "" {
			errLine += " — "
		}
		errLine += reason
	}
	if errLine != "" {
		fitText(m, 20, cy+height*8/100, safeText(errLine), false, math.Max(10, float64(width)*0.032), color.RGBA{70, 70, 76, 255}, width-40)
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, m, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

// offlineJPEGResponse — 200 + NO SIGNAL (режим OFFLINE_RESPONSE=nosignal).
func (s *Server) offlineJPEGResponse(w http.ResponseWriter, code int, reason, label string, width, height int) bool {
	reasonS := reason
	if len(reasonS) > 120 {
		reasonS = reasonS[:117] + "…"
	}
	jpeg := noSignalJPEG(width, height, code, reasonS)
	if len(jpeg) == 0 {
		return false
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Camera-Status", "no-signal")
	w.Header().Set("X-Error", itoa(code))
	w.Header().Set("X-Error-Reason", safeText(reasonS)[:200])
	if label != "" {
		w.Header().Set("X-Camera", label)
	}
	s.log.Info("камера офлайн -> NO SIGNAL", "w", width, "h", height, "code", code, "reason", reasonS[:80])
	_, _ = w.Write(jpeg)
	return true
}

// gradient — вертикальный градиент (10,10,14) поверх тайла.
// top=true: сверху почти непрозрачный → к низу прозрачный (шапка).
func gradient(m *image.RGBA, x0, y0, w, h int, top bool) {
	for yy := 0; yy < h; yy++ {
		var a float64
		if top {
			a = 250 * math.Pow(1-float64(yy)/float64(h), 1.2)
		} else {
			a = 235 * math.Pow(float64(yy+1)/float64(h), 1.6)
		}
		fa := a / 255
		y := y0 + yy
		for xx := 0; xx < w; xx++ {
			dst := m.RGBAAt(x0+xx, y)
			m.Set(x0+xx, y, color.RGBA{
				R: uint8(10*fa + float64(dst.R)*(1-fa)),
				G: uint8(10*fa + float64(dst.G)*(1-fa)),
				B: uint8(14*fa + float64(dst.B)*(1-fa)),
				A: 255,
			})
		}
	}
}

// nowStamp — "dd.mm.yyyy HH:MM" (как в шапке /preview).
func nowStamp() string {
	return time.Now().Format("02.01.2006 15:04")
}
