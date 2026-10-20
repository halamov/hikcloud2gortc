// Кольцевой лог воркера (порт _StreamSession из decrypt_proxy.py).
//
// Читатель-горутина держит облачный поток открытым постоянно и дописывает
// в лог уже преобразованные (расшифрованные) данные. Лог — КОЛЬЦО: окно от
// последнего KEYFRAME (Annex-B) или байтовое окно (TS). Клиент в любой
// момент снимает снимок лога (GOP-хвост с keyframe-входом) и дальше
// потребляет только новые записи. При (пере)запуске сессии gen растёт —
// клиенты берут новый снимок.
package worker

import (
	"bytes"
	"context"
	"sync"
	"time"
)

// RingMaxBytes — жёсткий байтовый лимит кольца.
const RingMaxBytes = 1.5 * 1024 * 1024

type ringItem struct {
	Seq   int
	Data  []byte
	IsKey bool
}

// Ring — общий лог воркера.
type Ring struct {
	mu       sync.Mutex
	log      []ringItem
	seq      int
	gen      int
	annexb   bool
	codec    string
	lastData time.Time
	subs     map[int]chan struct{}
	nextSub  int
}

func NewRing() *Ring {
	return &Ring{subs: map[int]chan struct{}{}}
}

func (r *Ring) signal() {
	for _, ch := range r.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Begin — (пере)запуск сессии: свежий лог, новая генерация.
func (r *Ring) Begin(annexb bool, codec string) {
	r.mu.Lock()
	r.gen++
	r.annexb = annexb
	r.codec = codec
	r.log = nil
	r.seq = 0
	r.lastData = time.Time{}
	r.mu.Unlock()
	r.signal()
}

// KeyframeIn — есть ли keyframe NAL в куске Annex-B (HEVC 16-21 / H.264 IDR=5).
func KeyframeIn(data []byte) bool {
	pos := 0
	for {
		i := bytes.Index(data[pos:], []byte{0, 0, 1})
		if i < 0 {
			return false
		}
		idx := pos + i
		hdr := idx + 3
		if hdr < len(data) {
			t := data[hdr]
			if ((t>>1)&0x3F) >= 16 && ((t>>1)&0x3F) <= 21 || t&0x1F == 5 {
				return true
			}
		}
		pos = hdr
	}
}

// Append — данные от читателя: в лог + разбудить клиентов.
func (r *Ring) Append(data []byte) {
	if len(data) == 0 {
		return
	}
	kf := false
	r.mu.Lock()
	if r.annexb {
		kf = KeyframeIn(data)
	}
	r.seq++
	r.log = append(r.log, ringItem{Seq: r.seq, Data: data, IsKey: kf})
	r.lastData = time.Now()
	r.trim()
	r.mu.Unlock()
	r.signal()
}

// trim — ПОД LOCK. Annex-B: лог = GOP-хвост от ПОСЛЕДНЕГО keyframe
// (новый клиент должен получать вхождение с I-кадра — иначе детект кодека
// и ffmpeg-remux ломаются на P-slice). Жёсткий байтовый лимит — fallback.
func (r *Ring) trim() {
	if r.annexb {
		last := -1
		for j, it := range r.log {
			if it.IsKey {
				last = j
			}
		}
		if last > 0 {
			r.log = r.log[last:]
		}
	}
	total := 0
	for _, it := range r.log {
		total += len(it.Data)
	}
	for len(r.log) > 0 && total > RingMaxBytes {
		total -= len(r.log[0].Data)
		r.log = r.log[1:]
	}
}

// Stats — для диагностики.
func (r *Ring) Stats() (seq, gen int, annexb bool, codec string, lastData time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq, r.gen, r.annexb, r.codec, r.lastData
}

// Write — io.Writer поверх Append (для PacedWriter).
func (r *Ring) Write(p []byte) (int, error) {
	r.Append(p)
	return len(p), nil
}

// WaitReady — ждать данные (ctx с deadline задаёт таймаут).
func (r *Ring) WaitReady(ctx context.Context) bool {
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	wake := make(chan struct{}, 1)
	r.subs[id] = wake
	has := r.seq > 0
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}()
	for !has {
		select {
		case <-ctx.Done():
			return false
		case <-wake:
		}
		r.mu.Lock()
		has = r.seq > 0
		r.mu.Unlock()
	}
	return true
}

// Subscribe — канал новых записей кольца; при смене gen шлёт новый снимок.
// Возвращает: канал (закрывается при ctx/тишине 30с), стартовый снимок, gen.
func (r *Ring) Subscribe(ctx context.Context) (<-chan []byte, int, int, [][]byte) {
	ch := make(chan []byte, 256)

	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	wake := make(chan struct{}, 1)
	r.subs[id] = wake
	gen := r.gen
	snap := make([][]byte, len(r.log))
	for i, it := range r.log {
		snap[i] = it.Data
	}
	snapLen := r.seq
	r.mu.Unlock()

	go func() {
		defer close(ch)
		defer func() {
			r.mu.Lock()
			delete(r.subs, id)
			r.mu.Unlock()
		}()
		g, lastSeq := gen, 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			default:
			}
			r.mu.Lock()
			var items []ringItem
			if r.gen != g {
				g = r.gen
				lastSeq = 0
				items = make([]ringItem, len(r.log))
				copy(items, r.log)
			} else {
				for _, it := range r.log {
					if it.Seq > lastSeq {
						items = append(items, it)
					}
				}
				lastSeq = r.seq
			}
			hasData := r.seq > 0
			lastData := r.lastData
			r.mu.Unlock()
			if hasData && time.Since(lastData) > 30*time.Second {
				return // тишина 30с — сессия упала, читатель перезапускает
			}
			if len(items) == 0 {
				// новых записей нет — спать (wake буферизован, ничего не теряем).
				// Без этой ветки цикл крутится на 100% CPU при «тихом» потоке.
				select {
				case <-ctx.Done():
					return
				case <-wake:
					continue
				case <-time.After(15 * time.Second):
					continue
				}
			}
			for _, it := range items {
				select {
				case ch <- it.Data:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, gen, snapLen, snap
}

// LastData — время последних данных.
func (r *Ring) LastData() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastData
}

// Codec — codec текущей сессии.
func (r *Ring) Codec() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codec
}

// IsAnnexB — формат текущей сессии.
func (r *Ring) IsAnnexB() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.annexb
}
