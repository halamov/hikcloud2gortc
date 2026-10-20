// PacedWriter — сглаживание облачных GOP-bursts + backpressure.
// Порт PacedWriter из decrypt_proxy.py: кадры (ещё сжатые) буферизуются и
// выдаются с измеренной скоростью; при заполнении очереди кадры отбрасываются
// до границы keyframe. Облачный reader никогда не блокируется.
package worker

import (
	"bytes"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	paceTargetS     = 1.0
	paceWindowS     = 4.0
	paceMinStep     = 1.0 / 60.0
	paceMaxStep     = 1.0 / 4.0
	paceMaxBytesDef = 8 << 20
)

func paceMaxBytes() int {
	if v := os.Getenv("PACE_QUEUE_MB"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return int(f * 1024 * 1024)
		}
	}
	return paceMaxBytesDef
}

type paceItem struct {
	data  []byte
	frame bool
	key   bool
}

// PacedWriter — пишет в w, сглаживая bursts (jitter buffer).
type PacedWriter struct {
	w     interface{ Write([]byte) (int, error) }
	paced bool
	hevc  bool

	mu       sync.Mutex
	q        []paceItem
	qBytes   int
	rx       []time.Time
	cur      []byte
	curKey   bool
	curHasVCL bool
	stop     bool
	ev       chan struct{}
	done     chan struct{}
}

func NewPacedWriter(w interface{ Write([]byte) (int, error) }, codec string, paced bool) *PacedWriter {
	return &PacedWriter{
		w:     w,
		paced: paced,
		hevc:  codec != "h264",
		ev:    make(chan struct{}, 16),
		done:  make(chan struct{}),
	}
}

func (p *PacedWriter) Start() *PacedWriter {
	go p.run()
	return p
}

// nalKind — (vcl, key) по типу NAL.
func (p *PacedWriter) nalKind(b0, b1 byte) (bool, bool) {
	if p.hevc {
		t := (b0 >> 1) & 0x3F
		return t <= 31, t >= 16 && t <= 21
	}
	t := b0 & 0x1F
	return t >= 1 && t <= 5, t == 5
}

// Feed — расшифрованные байты: разбиваем на кадры, кладём в очередь.
func (p *PacedWriter) Feed(data []byte) {
	if len(data) == 0 {
		return
	}
	if !p.paced {
		p.push(append([]byte(nil), data...), true, false)
		return
	}
	pos := 0
	for {
		i := bytes.Index(data[pos:], []byte{0, 0, 1})
		if i < 0 {
			p.cur = append(p.cur, data[pos:]...)
			break
		}
		i += pos
		hdr := i + 3
		if hdr < len(data) {
			vcl, key := p.nalKind(data[hdr], data[hdr+1])
			// Новый VCL NAL — предыдущий кадр закончился.
			if vcl && p.curHasVCL {
				start := i
				if i > 0 && data[i-1] == 0 {
					start = i - 1
				}
				p.cur = append(p.cur, data[pos:start]...)
				p.flushFrame()
				pos = start
				continue
			}
			if key {
				p.curKey = true
			}
			if vcl {
				p.curHasVCL = true
			}
		}
		p.cur = append(p.cur, data[pos:hdr]...)
		pos = hdr
	}
	if len(p.cur) > (2 << 20) { // защита: граница не найдена
		p.flushFrame()
	}
}

func (p *PacedWriter) flushFrame() {
	if len(p.cur) == 0 {
		return
	}
	p.push(p.cur, true, p.curKey)
	p.cur = nil
	p.curKey = false
	p.curHasVCL = false
}

func (p *PacedWriter) push(data []byte, frame, key bool) {
	p.mu.Lock()
	p.q = append(p.q, paceItem{data: data, frame: frame, key: key})
	p.qBytes += len(data)
	if frame {
		p.rx = append(p.rx, time.Now())
	}
	// Переполнение — отбрасываем кадры ДО СЛЕДУЮЩЕГО KEYFRAME.
	if p.qBytes > paceMaxBytes() {
		for len(p.q) > 1 {
			p.qBytes -= len(p.q[0].data)
			p.q = p.q[1:]
			if p.q[0].key {
				break
			}
		}
	}
	select {
	case p.ev <- struct{}{}:
	default:
	}
	p.mu.Unlock()
}

func (p *PacedWriter) step() float64 {
	now := time.Now()
	for len(p.rx) > 0 && now.Sub(p.rx[0]) > paceWindowS*time.Second {
		p.rx = p.rx[1:]
	}
	var span float64
	if len(p.rx) > 0 {
		span = now.Sub(p.rx[0]).Seconds()
	}
	nominal := 0.04
	if len(p.rx) > 20 && span > 2.0 {
		nominal = span / float64(len(p.rx))
	}
	if nominal < paceMinStep {
		nominal = paceMinStep
	}
	if nominal > paceMaxStep {
		nominal = paceMaxStep
	}
	depth := float64(len(p.q)) * nominal
	if depth > paceTargetS*2 {
		return nominal * 0.85
	}
	if depth < paceTargetS * 0.5 {
		return nominal * 1.15
	}
	return nominal
}

func (p *PacedWriter) run() {
	defer close(p.done)
	due := time.Now()
	for {
		p.mu.Lock()
		var item *paceItem
		if len(p.q) > 0 {
			item = &p.q[0]
			p.q = p.q[1:]
			p.qBytes -= len(item.data)
		}
		p.mu.Unlock()
		if item == nil {
			if p.stop {
				return
			}
			select {
			case <-p.ev:
			case <-time.After(50 * time.Millisecond):
			}
			// слей остаток сигнала
			for {
				select {
				case <-p.ev:
				default:
					goto drained
				}
			}
		drained:
			due = time.Now()
			continue
		}
		if _, err := p.w.Write(item.data); err != nil {
			return
		}
		if p.paced && item.frame {
			due = due.Add(time.Duration(p.step() * float64(time.Second)))
			delay := time.Until(due)
			if delay > 0 {
				time.Sleep(min(delay, time.Second))
			} else if delay < -time.Second {
				due = time.Now()
			}
		}
	}
}

// Close — остановка после опорожнения очереди (не дольше drain).
func (p *PacedWriter) Close(drain time.Duration) {
	p.flushFrame()
	p.mu.Lock()
	p.stop = true
	select {
	case p.ev <- struct{}{}:
	default:
	}
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(drain):
	}
}
