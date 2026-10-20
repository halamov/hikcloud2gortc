// Package decode — NAL/AES-ядро, 1:1-порт cloudcam/decrypt_proxy.py.
//
// Применяются три варианта шифра NAL:
//   A)       header ЧИСТЫЙ, body зашифрован/чистый      -> clear=hdr, drop=0
//   B-hint)  чистый индикатор типа + зашифрованный NAL  -> clear=0,   drop=hdr
//   B-whole) весь NAL зашифрован (без индикатора, PS)   -> clear=0,   drop=0
//
// СЕЛЕКТИВНЫЙ шифр: камеры шифруют только I-фрейм/IRAP + param-set, inter
// (P/B) slice оставляют чистыми — тип определяется по потоку (образцы).
package decode

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"strconv"
)

const (
	// NalEncPrefix — зашифрованы первые ровно 4096 байт каждого NAL.
	NalEncPrefix = 4096
	// InterSamples — образцов inter NAL для решения об inter-шифре.
	InterSamples = 8
	// InterMargin — «дешифр» только если заметно более структурирован.
	InterMargin = 0.25
	// InterMaxPending — если inter NAL не приходит, сдаёмся после таких байт.
	InterMaxPending = 4 << 20
	// IRAPMaxWait — принудительное открытие IRAP-шлюза.
	IRAPMaxWait = 400
	// MaxPSBuf — предел буферов MPEG-PS demux (resync при превышении).
	MaxPSBuf = 4 << 20
)

// Start — 4-байтовый Annex-B start code.
var Start = []byte{0x00, 0x00, 0x00, 0x01}

var (
	profiles     = map[byte]bool{66: true, 77: true, 88: true, 100: true, 110: true, 122: true, 244: true, 44: true, 83: true, 86: true, 118: true, 128: true}
	h264EncTypes = map[int]bool{5: true, 7: true, 8: true}
	hevcEncTypes = map[int]bool{16: true, 17: true, 18: true, 19: true, 20: true, 21: true, 32: true, 33: true, 34: true}
	h264IRAP     = map[int]bool{5: true}
	hevcIRAP     = map[int]bool{16: true, 17: true, 18: true, 19: true, 20: true, 21: true}
	h264Pset     = map[int]bool{7: true, 8: true}
	hevcPset     = map[int]bool{32: true, 33: true, 34: true}
)

type pendItem struct {
	kind byte // 0 = plain data, 'i' = inter NAL
	data []byte
	hdr  int
}

// NalBase — общее состояние NAL-декодера (AES-ECB, classify, inter, IRAP).
type NalBase struct {
	key        [16]byte
	cipher     cipher.Block
	decrypt    *bool // nil=неизвестно, true=зашифровано, false=чисто
	KeyError   bool
	Codec      string // 'h264' | 'hevc' | ''
	EncOff     int    // offset начала шифра (PS-демодек): 0|1|2
	Enc        bool   // зашифрован ли поток (PS-демодек)
	clear      int
	drop       int
	cacheDir   string
	cacheKey   string
	interEnc   *bool
	verifying  bool
	interClear []byte
	interDec   []byte
	pending    []pendItem // nil = решение есть
	pendingBytes int
	InterNote  string
	GateNote   string
	seenIRAP   bool
	gated      int
}

// NewNalBase — ключ (ljust/trim до 16), решение об inter из кеша (если есть).
func NewNalBase(key, cacheDir, cacheKey string) *NalBase {
	k := make([]byte, 16)
	kr := []byte(key)
	copy(k, kr)
	c := &NalBase{cacheDir: cacheDir, cacheKey: cacheKey}
	c.key = [16]byte(k)
	c.cipher, _ = aes.NewCipher(c.key[:])
	if cacheKey != "" {
		if v := EncCacheLoad(cacheDir, cacheKey); v != nil {
			c.interEnc = v
			c.verifying = true
		}
	}
	return c
}

func (c *NalBase) encCacheStore(v bool) {
	if c.cacheKey != "" {
		EncCacheStore(c.cacheDir, c.cacheKey, v)
	}
}

// Encrypted — зашифрован ли поток (nil = ещё не определено).
func (c *NalBase) Encrypted() *bool { return c.decrypt }

func (c *NalBase) setDecrypt(v bool) { c.decrypt = &v }

// aes16 — AES-ECB расшифровка первых 16 байт.
func (c *NalBase) aes16(data []byte) []byte {
	out := make([]byte, 16)
	c.cipher.Decrypt(out, data[:16])
	return out
}

// decryptBody — расшифровка зашифрованного префикса NAL body.
func (c *NalBase) decryptBody(body []byte) []byte {
	if c.decrypt == nil || !*c.decrypt {
		return append([]byte(nil), body...)
	}
	n := (minInt(len(body), NalEncPrefix) / 16) * 16
	if n == 0 {
		return append([]byte(nil), body...)
	}
	out := make([]byte, len(body))
	copy(out, body)
	for i := 0; i < n; i += 16 {
		c.cipher.Decrypt(out[i:i+16], body[i:i+16])
	}
	return out
}

// Classify — определяет вариант шифра по param-set NAL.
func (c *NalBase) Classify(nal []byte, hdr int, isParamset func(byte) bool, bodyMarker func(byte) bool) {
	if c.decrypt != nil {
		return
	}
	ps := isParamset(nal[0])
	// A) header чистый
	if ps && len(nal) > hdr && bodyMarker(nal[hdr]) {
		c.clear, c.drop = hdr, 0
		c.setDecrypt(false)
		return
	}
	if ps && len(nal) >= hdr+16 && bodyMarker(c.aes16(nal[hdr:hdr+16])[0]) {
		c.clear, c.drop = hdr, 0
		c.setDecrypt(true)
		return
	}
	// B-hint) чистый индикатор типа + зашифрованный полный NAL
	if len(nal) >= hdr+16 {
		dec := c.aes16(nal[hdr : hdr+16])
		if isParamset(dec[0]) && len(dec) > hdr && bodyMarker(dec[hdr]) {
			c.clear, c.drop = 0, hdr
			c.setDecrypt(true)
			return
		}
	}
	// B-whole) весь NAL зашифрован, без индикатора
	if len(nal) >= 16 {
		dec := c.aes16(nal[:16])
		if isParamset(dec[0]) && len(dec) > hdr && bodyMarker(dec[hdr]) {
			c.clear, c.drop = 0, 0
			c.setDecrypt(true)
			return
		}
	}
	if ps && len(nal) >= hdr+16 {
		c.KeyError = true
	}
}

// irapGate — выдать ли NAL (slice до первого IRAP отбрасываются).
func (c *NalBase) irapGate(ntype, hdr int) bool {
	if c.seenIRAP {
		return true
	}
	var irap, pset map[int]bool
	if hdr == 1 {
		irap, pset = h264IRAP, h264Pset
	} else {
		irap, pset = hevcIRAP, hevcPset
	}
	if irap[ntype] {
		c.seenIRAP = true
		if c.gated > 0 {
			c.GateNote = fmt.Sprintf("до IRAP отброшено %d slice", c.gated)
		}
		return true
	}
	if pset[ntype] {
		return true
	}
	c.gated++
	if c.gated > IRAPMaxWait {
		c.seenIRAP = true
		c.GateNote = fmt.Sprintf("IRAP не пришёл (%d slice) — шлюз открыт принудительно", c.gated)
		return true
	}
	return false
}

func (c *NalBase) put(out *bytes.Buffer, data []byte) {
	if c.pending == nil {
		out.Write(data)
		return
	}
	c.pending = append(c.pending, pendItem{kind: 0, data: data})
	c.pendingBytes += len(data)
	c.guardPending(out)
}

func (c *NalBase) interBytes(nal []byte, hdr int) []byte {
	out := make([]byte, 0, len(nal)+4)
	out = append(out, Start...)
	if c.interEnc != nil && *c.interEnc {
		out = append(out, nal[:hdr]...)
		out = append(out, c.decryptBody(nal[hdr:])...)
		return out
	}
	out = append(out, nal...)
	return out
}

func (c *NalBase) putInter(out *bytes.Buffer, nal []byte, hdr int) {
	if c.pending == nil {
		out.Write(c.interBytes(nal, hdr))
		return
	}
	c.pending = append(c.pending, pendItem{kind: 'i', data: nal, hdr: hdr})
	c.pendingBytes += len(nal)
	if c.interEnc != nil {
		c.flush(out)
	} else {
		c.guardPending(out)
	}
}

func (c *NalBase) guardPending(out *bytes.Buffer) {
	if c.pending != nil && c.pendingBytes > InterMaxPending {
		f := false
		c.interEnc = &f
		c.InterNote = "образцов не хватило -> принято как ЧИСТОЕ"
		c.flush(out)
	}
}

func (c *NalBase) flush(out *bytes.Buffer) {
	pend := c.pending
	c.pending = nil
	c.pendingBytes = 0
	for _, it := range pend {
		if it.kind == 0 {
			out.Write(it.data)
		} else {
			out.Write(c.interBytes(it.data, it.hdr))
		}
	}
}

func (c *NalBase) isInterSlice(nal []byte, hdr int) bool {
	if hdr == 1 {
		t := nal[0] & 0x1F
		return t == 1 || t == 2
	}
	return (nal[0]>>1)&0x3F <= 9
}

func (c *NalBase) sampleInter(nal []byte, hdr int) {
	body := nal[hdr:]
	if len(body) < 16 {
		return
	}
	c.interClear = append(c.interClear, body[0])
	c.interDec = append(c.interDec, c.aes16(body)[0])
	if len(c.interClear) >= InterSamples {
		c.decideInter()
	}
}

func (c *NalBase) decideInter() {
	n := len(c.interClear)
	dClear := float64(distinctCount(c.interClear)) / float64(n)
	dDec := float64(distinctCount(c.interDec)) / float64(n)
	fresh := dDec+InterMargin < dClear
	if c.verifying {
		c.verifying = false
		if fresh != *c.interEnc {
			c.interEnc = &fresh
			c.InterNote = fmt.Sprintf("кеш был НЕВЕРНЫМ -> исправлено: inter=%s (чист=%.2f расшифр=%.2f)",
				strconv.FormatBool(fresh), dClear, dDec)
			c.encCacheStore(fresh)
		}
		return
	}
	c.interEnc = &fresh
	c.InterNote = fmt.Sprintf("inter=%s (чист=%.2f расшифр=%.2f, %d образцов)",
		strconv.FormatBool(fresh), dClear, dDec, n)
	c.encCacheStore(fresh)
}

func distinctCount(b []byte) int {
	m := map[byte]bool{}
	for _, v := range b {
		m[v] = true
	}
	return len(m)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// EmitNal — выдача NAL с учётом варианта шифра и IRAP-шлюза.
func (c *NalBase) EmitNal(out *bytes.Buffer, nal []byte) {
	if c.decrypt == nil {
		return
	}
	if c.pending != nil && !(*c.decrypt && c.clear != 0 && c.drop == 0) {
		c.pending = nil
	}
	if !*c.decrypt { // чистый поток
		h := c.clear
		if h == 0 {
			h = 2
		}
		var nt int
		if h == 1 {
			nt = int(nal[0] & 0x1F)
		} else {
			nt = int((nal[0] >> 1) & 0x3F)
		}
		if !c.irapGate(nt, h) {
			return
		}
		c.put(out, append(append([]byte(nil), Start...), nal...))
		return
	}
	if c.drop != 0 {
		hdr := c.drop
		if len(nal) >= hdr+16 && bytes.Equal(c.aes16(nal[hdr:hdr+16])[:hdr], nal[:hdr]) {
			c.put(out, append(append([]byte(nil), Start...), c.decryptBody(nal[hdr:])...))
		} else if len(nal) >= 2*hdr && bytes.Equal(nal[hdr:2*hdr], nal[:hdr]) {
			c.put(out, append(append([]byte(nil), Start...), nal[hdr:]...))
		} else {
			c.put(out, append(append([]byte(nil), Start...), nal...))
		}
		return
	}
	hdr := c.clear
	if hdr == 0 { // B-whole
		c.put(out, append(append([]byte(nil), Start...), c.decryptBody(nal)...))
		return
	}
	if len(nal) <= hdr {
		c.put(out, append(append([]byte(nil), Start...), nal...))
		return
	}
	var nt int
	if hdr == 1 {
		nt = int(nal[0] & 0x1F)
	} else {
		nt = int((nal[0] >> 1) & 0x3F)
	}
	var enc map[int]bool
	if hdr == 1 {
		enc = h264EncTypes
	} else {
		enc = hevcEncTypes
	}
	if !enc[nt] && c.isInterSlice(nal, hdr) && (c.interEnc == nil || c.verifying) {
		c.sampleInter(nal, hdr)
	}
	if c.interEnc != nil && c.pending != nil {
		c.flush(out)
	}
	if !c.irapGate(nt, hdr) {
		return
	}
	if enc[nt] {
		c.put(out, append(append(append([]byte(nil), Start...), nal[:hdr]...), c.decryptBody(nal[hdr:])...))
	} else {
		c.putInter(out, nal, hdr)
	}
}

// Emit — классификация + выдача (RTP-варианты: 1- или 2-байтовый NAL header).
func (c *NalBase) Emit(out *bytes.Buffer, nal []byte, hdr int, isParamset func(byte) bool, bodyMarker func(byte) bool) {
	if len(nal) < hdr {
		return
	}
	c.Classify(nal, hdr, isParamset, bodyMarker)
	c.EmitNal(out, nal)
}
