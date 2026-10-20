// PsDecryptor — MPEG-PS demux + NAL-AES → ES (Annex-B).
// Порт PsStreamDecryptor из decrypt_proxy.py: автоопределение H.264/HEVC
// и offset шифра по 4 трактовкам (header чистый/зашифрован × codec).
package decode

import (
	"bytes"
)

var psSC3 = []byte{0x00, 0x00, 0x01}

type decideKind int

const (
	resNone decideKind = iota
	resKeyError
	resOK
)

type decideRes struct {
	kind  decideKind
	codec string
	off   int
	enc   bool
}

// PsDecryptor — демультиплексор MPEG-PS с NAL-дешифрованием.
type PsDecryptor struct {
	*NalBase
	buf        []byte
	es         []byte
	undecided  [][]byte
	EncOff     int // offset начала шифра: 0=весь NAL, 1=H.264, 2=после HEVC header
	BufResyncs int
}

func NewPs(key, cacheDir, cacheKey string) *PsDecryptor {
	return &PsDecryptor{NalBase: NewNalBase(key, cacheDir, cacheKey)}
}

var h264ValidSet = map[byte]bool{1: true, 5: true, 6: true, 7: true, 8: true, 9: true}

func h264Valid(b byte) bool { return (b & 0x80) == 0 && h264ValidSet[b&0x1F] }

func hevcValid(b byte) bool {
	if (b & 0x80) != 0 {
		return false
	}
	switch (b >> 1) & 0x3F {
	case 0, 1, 2, 4, 19, 20, 21, 32, 33, 34, 39, 40:
		return true
	}
	return false
}

// corrob — сколько из big NAL «валидные» по first-byte ок-проверке.
func (d *PsDecryptor) corrob(big [][]byte, ok func(byte) bool) bool {
	hits := 0
	for _, n := range big {
		if len(n) > 0 && ok(n[0]) {
			hits++
		}
	}
	m := len(big) / 2
	if m < 2 {
		m = 2
	}
	return hits >= m
}

// decide — по собранным NAL: codec + offset шифра + зашифрован/чист.
func (d *PsDecryptor) decide(nals [][]byte) decideRes {
	var big [][]byte
	for _, n := range nals {
		if len(n) >= 18 {
			big = append(big, n)
		}
	}
	if len(big) < 3 {
		return decideRes{kind: resNone}
	}
	decB := make([][]byte, len(big))
	for i, n := range big {
		decB[i] = d.aes16(n)
	}
	for i, n := range big {
		// A) header ЧИСТЫЙ, body зашифрован/чистый
		if (n[0]&0x80) == 0 && n[0]&0x1F == 7 { // H.264 SPS
			if profiles[n[1]] && d.corrob(big, h264Valid) {
				return decideRes{kind: resOK, codec: "h264", off: 1, enc: false}
			}
			if profiles[d.aes16(n[1:17])[0]] && d.corrob(big, h264Valid) {
				return decideRes{kind: resOK, codec: "h264", off: 1, enc: true}
			}
		}
		if (n[0]&0x80) == 0 && (n[0]>>1)&0x3F == 32 { // HEVC VPS
			if n[2] == 0x0C && d.corrob(big, hevcValid) {
				return decideRes{kind: resOK, codec: "hevc", off: 2, enc: false}
			}
			if d.aes16(n[2:18])[0] == 0x0C && d.corrob(big, hevcValid) {
				return decideRes{kind: resOK, codec: "hevc", off: 2, enc: true}
			}
		}
		// B) ВЕСЬ NAL зашифрован (включая header)
		dn := decB[i]
		if dn[0]&0x1F == 7 && profiles[dn[1]] && d.corrob(decB, h264Valid) {
			return decideRes{kind: resOK, codec: "h264", off: 0, enc: true}
		}
		if (dn[0]>>1)&0x3F == 32 && dn[2] == 0x0C && d.corrob(decB, hevcValid) {
			return decideRes{kind: resOK, codec: "hevc", off: 0, enc: true}
		}
	}
	if len(big) >= 24 {
		return decideRes{kind: resKeyError}
	}
	return decideRes{kind: resNone}
}

func (d *PsDecryptor) emit(out *bytes.Buffer, nal []byte) {
	if len(nal) == 0 {
		return
	}
	if d.decrypt == nil {
		d.undecided = append(d.undecided, nal)
		res := d.decide(d.undecided)
		switch res.kind {
		case resKeyError:
			d.KeyError = true
			d.undecided = nil
			return
		case resNone:
			return
		}
		d.Codec, d.EncOff, d.Enc = res.codec, res.off, res.enc
		enc := res.enc
		d.setDecrypt(enc)
		d.clear, d.drop = d.EncOff, 0
		for _, n := range d.undecided {
			d.EmitNal(out, n)
		}
		d.undecided = nil
		return
	}
	d.EmitNal(out, nal)
}

// demux — выделяет видео-PES (0xE0-0xEF) в _es.
func (d *PsDecryptor) demux() {
	b := d.buf
	n := len(b)
	pos := 0
	for {
		sc := bytes.Index(b[pos:], psSC3)
		if sc < 0 {
			break
		}
		sc += pos
		if sc+4 > n {
			break
		}
		code := b[sc+3]
		switch {
		case code == 0xBA: // pack header
			if sc+14 > n {
				break
			}
			end := sc + 14 + int(b[sc+13]&0x07)
			if end > n {
				break
			}
			pos = end
		case code == 0xB9: // MPEG end
			pos = sc + 4
		case code >= 0xE0 && code <= 0xEF: // video PES
			if sc+9 > n {
				break
			}
			length := int(b[sc+4])<<8 | int(b[sc+5])
			payloadStart := sc + 9 + int(b[sc+8])
			if payloadStart > n {
				break
			}
			if length > 0 {
				end := sc + 6 + length
				if end > n {
					break
				}
				d.es = append(d.es, b[payloadStart:end]...)
				pos = end
			} else { // бесконечный: до следующего start code
				nxt := bytes.Index(b[payloadStart:], psSC3)
				if nxt < 0 {
					break // неполный — ждём следующий chunk
				}
				nxt += payloadStart
				d.es = append(d.es, b[payloadStart:nxt]...)
				pos = nxt
			}
		default: // другой stream (audio/system/PSM) — пропускаем по длине
			if sc+6 > n {
				break
			}
			length := int(b[sc+4])<<8 | int(b[sc+5])
			end := sc + 6 + length
			if end > n {
				break
			}
			pos = end
		}
	}
	d.buf = b[pos:]
}

// processES — декодирует полные NAL в _es, возвращает Annex-B.
func (d *PsDecryptor) processES() []byte {
	e := d.es
	var starts []int
	i := bytes.Index(e, psSC3)
	for i >= 0 {
		starts = append(starts, i)
		j := bytes.Index(e[i+3:], psSC3)
		if j < 0 {
			break
		}
		i = i + 3 + j
	}
	if len(starts) < 2 {
		return nil
	}
	out := &bytes.Buffer{}
	for k := 0; k < len(starts)-1; k++ {
		nal := e[starts[k]+3 : starts[k+1]]
		if len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		d.emit(out, nal)
	}
	d.es = append([]byte(nil), e[starts[len(starts)-1]:]...)
	return out.Bytes()
}

// cap — resync на последний start code при превышении предела буферов.
func (d *PsDecryptor) cap() {
	if len(d.buf) > MaxPSBuf {
		if i := bytes.LastIndex(d.buf, psSC3); i > 0 {
			d.buf = append([]byte(nil), d.buf[i:]...)
		} else {
			d.buf = nil
		}
		d.BufResyncs++
	}
	if len(d.es) > MaxPSBuf {
		if i := bytes.LastIndex(d.es, psSC3); i > 0 {
			d.es = append([]byte(nil), d.es[i:]...)
		} else {
			d.es = nil
		}
		d.BufResyncs++
	}
}

// Feed — chunk MPEG-PS → Annex-B (может быть пусто).
func (d *PsDecryptor) Feed(chunk []byte) []byte {
	d.buf = append(d.buf, chunk...)
	d.demux()
	out := d.processES()
	d.cap()
	return out
}
