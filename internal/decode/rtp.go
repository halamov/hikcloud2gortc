// RTP-декодеры: RTP/HEVC и RTP/H.264 → Annex-B + NAL-AES.
// Порт HevcRtpDecryptor / H264RtpDecryptor / _RtpSeqMixin из decrypt_proxy.py.
package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// HevcVideoPT — RTP payload type видео в Hik-Connect потоках.
const HevcVideoPT = 96

// RtpPayload — payload после фиксированного, CSRC, extension и padding заголовков.
func RtpPayload(data []byte) ([]byte, error) {
	if len(data) < 12 {
		return nil, errors.New("RTP-пакет слишком короткий")
	}
	if data[0]>>6 != 2 {
		return nil, errors.New("неподдерживаемая версия RTP")
	}
	hasPad := data[0]&0x20 != 0
	hasExt := data[0]&0x10 != 0
	csrc := int(data[0] & 0x0F)
	offset := 12 + csrc*4
	if len(data) < offset {
		return nil, errors.New("CSRC-заголовок больше пакета")
	}
	if hasExt {
		if len(data) < offset+4 {
			return nil, errors.New("extension-заголовок больше пакета")
		}
		words := int(data[offset+2])<<8 | int(data[offset+3])
		offset += 4 + words*4
		if len(data) < offset {
			return nil, errors.New("extension больше пакета")
		}
	}
	payload := data[offset:]
	if hasPad {
		if len(payload) == 0 {
			return nil, errors.New("RTP padding без payload")
		}
		padLen := int(payload[len(payload)-1])
		if padLen == 0 || padLen > len(payload) {
			return nil, errors.New("неверная длина RTP padding")
		}
		payload = payload[:len(payload)-padLen]
	}
	return payload, nil
}

// DetectRtpCodec — codec по первому байту RTP video payload.
// Только по однозначным маркерам (FU/param-set); неясное — "" (следующий пакет).
func DetectRtpCodec(payload0 byte) string {
	h264t := int(payload0 & 0x1F)
	hevcT := (payload0 >> 1) & 0x3F
	if h264t == 28 || h264t == 24 || payload0 == 0x67 || payload0 == 0x68 || payload0 == 0x65 {
		return "h264"
	}
	if hevcT == 49 || hevcT == 48 || payload0 == 0x40 || payload0 == 0x42 || payload0 == 0x44 {
		return "hevc"
	}
	return ""
}

// DetectTransport — best-effort транспорт медиа-бодов: "ps"|"ts"|"rtp"|"unknown".
func DetectTransport(data []byte) string {
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0, 0, 1, 0xBA}) {
		return "ps"
	}
	if len(data) >= 1 && data[0] == 0x47 {
		return "ts"
	}
	if len(data) >= 1 && data[0]>>6 == 2 {
		return "rtp"
	}
	return "unknown"
}

type rtpKey struct {
	ssrc uint32
	pt   byte
}

// RtpDecryptor — RTP → Annex-B + NAL-AES (HEVC или H.264).
type RtpDecryptor struct {
	*NalBase
	isHEVC  bool
	cur     []byte
	lastSeq map[rtpKey]uint16
	SeqGaps int
	Dups    int
}

func NewHevcRtp(key, cacheDir, cacheKey string) *RtpDecryptor {
	d := &RtpDecryptor{NalBase: NewNalBase(key, cacheDir, cacheKey), isHEVC: true}
	d.lastSeq = map[rtpKey]uint16{}
	return d
}

func NewH264Rtp(key, cacheDir, cacheKey string) *RtpDecryptor {
	d := &RtpDecryptor{NalBase: NewNalBase(key, cacheDir, cacheKey)}
	d.lastSeq = map[rtpKey]uint16{}
	return d
}

// checkSeq — False = дубликат (отбросить). При разрыве собираемый FU аннулируется.
// Seq отслеживается по ПАРЕ (SSRC, PT): у видео/аудио/metadata свои счётчики.
func (d *RtpDecryptor) checkSeq(pkt []byte) bool {
	if len(pkt) < 12 {
		return true
	}
	seq := uint16(pkt[2])<<8 | uint16(pkt[3])
	k := rtpKey{ssrc: binary.BigEndian.Uint32(pkt[8:12]), pt: pkt[1] & 0x7F}
	last, ok := d.lastSeq[k]
	d.lastSeq[k] = seq
	if !ok {
		return true
	}
	if seq == last {
		d.Dups++
		return false
	}
	if seq != (last+1) && k.pt == HevcVideoPT {
		d.SeqGaps++
		d.cur = nil
	}
	return true
}

func (d *RtpDecryptor) emit(out *bytes.Buffer, nal []byte) {
	if d.isHEVC {
		d.NalBase.Emit(out, nal, 2, func(b byte) bool { return (b>>1)&0x3F == 32 }, func(x byte) bool { return x == 0x0C })
		return
	}
	d.NalBase.Emit(out, nal, 1, func(b byte) bool { return b&0x1F == 7 }, func(x byte) bool { return profiles[x] })
}

// Feed — один RTP-пакет → байты Annex-B (может быть пусто).
func (d *RtpDecryptor) Feed(pkt []byte) []byte {
	if !d.checkSeq(pkt) {
		return nil
	}
	if pkt[1]&0x7F != HevcVideoPT {
		return nil
	}
	pl, err := RtpPayload(pkt)
	minLen := 3
	if !d.isHEVC {
		minLen = 1
	}
	if err != nil || len(pl) < minLen {
		return nil
	}
	out := &bytes.Buffer{}
	if d.isHEVC {
		nt := (pl[0] >> 1) & 0x3F
		switch {
		case nt == 49: // FU — фрагментированный NAL
			if len(pl) < 3 {
				return nil
			}
			fu := pl[2]
			start := fu >> 7
			end := (fu >> 6) & 1
			fuType := fu & 0x3F
			if start != 0 {
				nh0 := byte((pl[0]&0x81)|(fuType<<1)) & 0xFF
				d.cur = make([]byte, 0, 256)
				d.cur = append(d.cur, nh0, pl[1])
				d.cur = append(d.cur, pl[3:]...)
			} else if d.cur != nil {
				d.cur = append(d.cur, pl[3:]...)
			}
			if end != 0 && d.cur != nil {
				d.emit(out, d.cur)
				d.cur = nil
			}
		case nt == 48: // AP — несколько NAL в одном пакете
			i := 2
			for i+2 <= len(pl) {
				sz := int(pl[i])<<8 | int(pl[i+1])
				i += 2
				if i+sz > len(pl) {
					break
				}
				d.emit(out, pl[i:i+sz])
				i += sz
			}
		default: // одиночный NAL (VPS/SPS/PPS/slice)
			d.emit(out, pl)
		}
	} else {
		t := pl[0] & 0x1F
		switch {
		case t == 28: // FU-A
			if len(pl) < 2 {
				return nil
			}
			fu := pl[1]
			start := fu >> 7
			end := (fu >> 6) & 1
			fuType := fu & 0x1F
			if start != 0 {
				d.cur = make([]byte, 0, 256)
				d.cur = append(d.cur, byte(pl[0]&0xE0|fuType))
				d.cur = append(d.cur, pl[2:]...)
			} else if d.cur != nil {
				d.cur = append(d.cur, pl[2:]...)
			}
			if end != 0 && d.cur != nil {
				d.emit(out, d.cur)
				d.cur = nil
			}
		case t == 24: // STAP-A — несколько NAL в одном пакете
			i := 1
			for i+2 <= len(pl) {
				sz := int(pl[i])<<8 | int(pl[i+1])
				i += 2
				if i+sz > len(pl) {
					break
				}
				d.emit(out, pl[i:i+sz])
				i += sz
			}
		default:
			d.emit(out, pl)
		}
	}
	return out.Bytes()
}
