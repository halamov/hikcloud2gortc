// VTM/VTDU TCP-сессия — порт pyezvizapi/stream.py (VtmStreamClient).
//
// Flow (как в мобильном приложении, проверено pyezvizapi и LE-EZVIZ-VS):
//  1. Bootstrap: pagelist (VTM-ресурс + сервер) + VTDU token (оба из кеша).
//  2. TCP-подключение к VTM, StreamInfoReq(ysproto URL).
//  3. StreamInfoRsp: либо result=0 + streamssn (готово), либо result!=0 +
//     новый streamurl + vtmstreamkey — REDIRECT на VTDU (до 3 раз).
//  4. Канал 0x01 — медиа-пакеты (RTP/PS/TS). Keepalive каждые 5 с +
//     ответ на KEEPALIVE_REQ сервера.
//
// Зашифрованные каналы (0x0a/0x0b, E2EE/ECDH) НЕ поддерживаются —
// для plaintext NVR-поток облако идёт по 0x00/0x01.
package cloudcam

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	vtmMagic        = 0x24
	vtmHeaderSize   = 8
	clientVersion   = "v3.6.3.20221124"
	keepaliveEvery  = 5 * time.Second // pyezvizapi: keepalive_interval=5.0
	maxRedirects    = 3
	maxCtrlPackets  = 20
	vtmClientType   = 9 // cln= в URL (как pyezvizapi open_cloud_stream)
)

const (
	chanMessage         = 0x00
	chanStream          = 0x01
	chanEncryptedMsg    = 0x0A
	chanEncryptedStream = 0x0B
)

const (
	msgKeepaliveReq    = 0x132
	msgKeepaliveRsp    = 0x133
	msgStreamInfoReq   = 0x13B
	msgStreamInfoRsp   = 0x13C
)

// ErrE2EE — облако переключилось на зашифрованный канал (E2EE).
var ErrE2EE = errors.New("encrypted VTM channel (E2EE) — не поддерживается; " +
	"камера требует ECDH-рукопожатие, plaintext-мост работает только с обычными NVR")

// Retcode — перевод кодов результата StreamInfoRsp в человекочитаемые.
var retcodeNames = map[int32]string{
	0:     "ok",
	10001: "повторный запрос (try again)",
	1013:  "неверный email или пароль",
	1014:  "неверный email или пароль",
	1015:  "нужна CAPTCHA",
	20002: "нужен 2FA-код",
	2009:  "устройство не подключено (offline)",
}

func retcodeText(code int32) string {
	if s, ok := retcodeNames[code]; ok {
		return s
	}
	return fmt.Sprintf("retcode %d", code)
}

// BuildVtmURL — ysproto URL (как pyezvizapi build_vtm_url).
// streamType: 1=main, 2=sub.
func BuildVtmURL(host string, port int, serial, biz, vtduToken string,
	channel, streamType int) string {
	// biz — готовые query-параметры (streamBizUrl из pagelist)
	bizParams := parseBizParams(biz)
	q := make([]string, 0, 10)
	for k, v := range bizParams {
		q = append(q, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}
	// фиксированный порядок (как urlencode после dict: biz + dev...timestamp)
	add := func(k, v string) { q = append(q, url.QueryEscape(k)+"="+url.QueryEscape(v)) }
	add("dev", serial)
	add("chn", strconv.Itoa(channel))
	add("stream", strconv.Itoa(streamType))
	add("cln", strconv.Itoa(vtmClientType))
	add("isp", "0")
	add("auth", "1")
	add("ssn", vtduToken)
	add("vip", "0")
	add("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	return fmt.Sprintf("ysproto://%s:%d/live?%s", host, port, strings.Join(q, "&"))
}

// parseBizParams — "a=b&c=d" (или ?a=b...) → map.
func parseBizParams(biz string) map[string]string {
	out := map[string]string{}
	s := strings.TrimSpace(biz)
	if s == "" {
		return out
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[i+1:]
	}
	for _, part := range strings.Split(s, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// parseYsproto — host, port, query-параметры из ysproto:// URL.
func parseYsproto(raw string) (host string, port int, params map[string]string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, nil, fmt.Errorf("ysproto URL: %w", err)
	}
	if u.Scheme != "ysproto" {
		return "", 0, nil, fmt.Errorf("не ysproto: %q", u.Scheme)
	}
	host = u.Hostname()
	ps := u.Port()
	if host == "" || ps == "" {
		return "", 0, nil, errors.New("ysproto URL без host:port")
	}
	port, err = strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, nil, fmt.Errorf("ysproto URL: неверный порт %q", ps)
	}
	params = map[string]string{}
	_ = u.Query()
	for _, part := range strings.Split(u.RawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if k != "" {
			params[k] = v
		}
	}
	return host, port, params, nil
}

// Stream — открытая VTM/VTDU сессия.
type Stream struct {
	conn      net.Conn
	seq       uint16
	streamSSN string
	redirect  *redirectInfo

	packets chan []byte
	done    chan struct{}
	once    sync.Once

	readTimeout time.Duration
}

// OpenParams — параметры открытия.
type OpenParams struct {
	Serial     string
	Channel    int
	StreamType int // 1 main / 2 sub
	Timeout    time.Duration // read-таймаут каждого read (0 = 10s)
}

// Open — полный handshake + запуск reader/keepalive горутины.
// Возвращает *Stream с Packets() — канал медиа-бодов (RTP/PS/TS).
func (c *Client) Open(ctx context.Context, p OpenParams, cache *VtmCache) (*Stream, error) {
	if p.Timeout <= 0 {
		p.Timeout = 10 * time.Second
	}
	// 1) Bootstrap-метаданные
	pl, err := c.Pagelist(cache)
	if err != nil {
		return nil, fmt.Errorf("VTM pagelist: %w", err)
	}
	ri, ok := findVtmResource(pl, p.Serial, p.Channel)
	if !ok {
		return nil, fmt.Errorf("could not find VTM resource for serial %s channel %d", p.Serial, p.Channel)
	}
	vtm, ok := pl.VTM[ri.ResourceID]
	if !ok {
		return nil, fmt.Errorf("VTM сервер для ресурса %s не найден", ri.ResourceID)
	}
	host := firstNonEmpty(vtm.ExternalIP, vtm.Domain, vtm.InternalIP)
	if host == "" {
		return nil, fmt.Errorf("нет VTM-эндпоинта для ресурса %s", ri.ResourceID)
	}
	if vtm.Port < 1 {
		return nil, fmt.Errorf("нет VTM-порта для ресурса %s", ri.ResourceID)
	}
	tokens, err := c.VTDUTokensCached(cache)
	if err != nil {
		return nil, fmt.Errorf("VTDU token: %w", err)
	}
	token := tokens[0]
	streamURL := BuildVtmURL(host, vtm.Port, p.Serial, ri.StreamBizUrl, token, p.Channel, p.StreamType)

	// 2) TCP + StreamInfoReq с редиректами (VTM -> VTDU)
	s := &Stream{
		packets:     make(chan []byte, 128),
		done:        make(chan struct{}),
		readTimeout: p.Timeout,
	}
	vtmKey := ""
	for redirect := 0; ; redirect++ {
		h, port, _, err := parseYsproto(streamURL)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(h, strconv.Itoa(port)))
		if err != nil {
			return nil, fmt.Errorf("VTM connect %s:%d: %w", h, port, err)
		}
		s.closeConn()
		s.conn = conn
		if err := s.handshake(ctx, streamURL, vtmKey); err != nil {
			s.closeConn()
			if redirect < maxRedirects {
				continue
			}
			return nil, err
		}
		if s.redirect != nil && redirect < maxRedirects {
			streamURL = s.redirect.URL
			vtmKey = s.redirect.Key
			s.redirect = nil
			s.closeConn()
			continue
		}
		break
	}

	// 3) reader + keepalive
	go s.readLoop()
	go s.keepaliveLoop()
	return s, nil
}

type redirectInfo struct{ URL, Key string }

func (s *Stream) closeConn() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// handshake — StreamInfoReq; читает до StreamInfoRsp (<=20 ctrl-пакетов).
func (s *Stream) handshake(ctx context.Context, streamURL, vtmKey string) error {
	req := &StreamInfoReq{
		Streamurl:    streamURL,
		Vtmstreamkey: vtmKey,
		Useragent:    clientVersion,
		Proxytype:    0,
		Clnversion:   clientVersion,
	}
	if err := s.send(chanMessage, msgStreamInfoReq, req.MarshalProto()); err != nil {
		return err
	}
	for i := 0; i < maxCtrlPackets; i++ {
		hdr, body, err := s.readPacket(ctx)
		if err != nil {
			return err
		}
		if hdr.msg == msgKeepaliveReq {
			// сервер запрашивает keepalive — отвечаем тем же бодом
			_ = s.send(chanMessage, msgKeepaliveRsp, body)
			continue
		}
		if hdr.msg != msgStreamInfoRsp {
			return fmt.Errorf("неожиданное сообщение 0x%04x в handshake", hdr.msg)
		}
		rsp, err := ParseStreamInfoRsp(body)
		if err != nil {
			return fmt.Errorf("StreamInfoRsp: %w", err)
		}
		if rsp.Result != 0 && rsp.Streamurl != "" && rsp.Vtmstreamkey != "" {
			s.redirect = &redirectInfo{URL: rsp.Streamurl, Key: rsp.Vtmstreamkey}
			return nil // редирект — вызывающий перезапустит handshake
		}
		if rsp.Result != 0 {
			return fmt.Errorf("StreamInfoRsp: %s", retcodeText(rsp.Result))
		}
		if rsp.Streamssn == "" {
			return errors.New("StreamInfoRsp без streamssn")
		}
		s.streamSSN = rsp.Streamssn
		return nil
	}
	return errors.New("таймаут ожидания StreamInfoRsp")
}

type vtmHdr struct {
	ch     byte
	length uint16
	seq    uint16
	msg    uint16
}

// readPacket — один полный VTM-пакет (header 8 + body).
func (s *Stream) readPacket(ctx context.Context) (vtmHdr, []byte, error) {
	var hdr vtmHdr
	hb := make([]byte, vtmHeaderSize)
	if _, err := s.readFull(ctx, hb); err != nil {
		return hdr, nil, err
	}
	if hb[0] != vtmMagic {
		return hdr, nil, fmt.Errorf("VTM magic 0x24 не найден (получен 0x%02x) — поток закрыт", hb[0])
	}
	switch hb[1] {
	case chanMessage, chanStream:
		// ок
	case chanEncryptedMsg, chanEncryptedStream:
		return hdr, nil, ErrE2EE
	default:
		return hdr, nil, fmt.Errorf("неизвестный VTM-канал 0x%02x", hb[1])
	}
	hdr.ch = hb[1]
	hdr.length = binary.BigEndian.Uint16(hb[2:4])
	hdr.seq = binary.BigEndian.Uint16(hb[4:6])
	hdr.msg = binary.BigEndian.Uint16(hb[6:8])
	body := make([]byte, hdr.length)
	if _, err := s.readFull(ctx, body); err != nil {
		return hdr, nil, err
	}
	return hdr, body, nil
}

// readFull — ровно n байт с таймаутом и учётом ctx.
func (s *Stream) readFull(ctx context.Context, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(s.readTimeout))
		n, err := s.conn.Read(buf[total:])
		total += n
		if n > 0 && err != nil {
			return total, err
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return total, fmt.Errorf("таймаут чтения VTM-потока (%s)", s.readTimeout)
			}
			return total, err
		}
	}
	return total, nil
}

func (s *Stream) send(ch byte, msg uint16, body []byte) error {
	if len(body) > 0xFFFF {
		return errors.New("VTM-пакет слишком большой")
	}
	hdr := make([]byte, vtmHeaderSize)
	hdr[0] = vtmMagic
	hdr[1] = ch
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(body)))
	s.seq++
	binary.BigEndian.PutUint16(hdr[4:6], s.seq)
	binary.BigEndian.PutUint16(hdr[6:8], msg)
	buf := make([]byte, 0, vtmHeaderSize+len(body))
	buf = append(buf, hdr...)
	buf = append(buf, body...)
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := s.conn.Write(buf)
	return err
}

// keepaliveLoop — keepalive каждые 5 с (пока жив сокет).
func (s *Stream) keepaliveLoop() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			req := &StreamKeepAliveReq{Streamssn: []byte(s.streamSSN)}
			if err := s.send(chanMessage, msgKeepaliveReq, req.MarshalProto()); err != nil {
				return
			}
		}
	}
}

// readLoop — читает пакеты; медиа-боды — в Packets(), контроль — сам.
func (s *Stream) readLoop() {
	defer close(s.packets)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
	for {
		hdr, body, err := s.readPacket(ctx)
		if err != nil {
			return
		}
		switch {
		case hdr.msg == msgKeepaliveReq:
			_ = s.send(chanMessage, msgKeepaliveRsp, body)
		case hdr.ch == chanStream:
			select {
			case s.packets <- body:
			case <-s.done:
				return
			}
		default:
			// прочие ctrl-пакеты (notify и т.п.) — игнорируем
		}
	}
}

// Packets — канал медиа-данных (закрыт при завершении сессии).
func (s *Stream) Packets() <-chan []byte { return s.packets }

// Close — закрыть сессию.
func (s *Stream) Close() {
	s.once.Do(func() {
		close(s.done)
		s.closeConn()
	})
}

// --- bootstrap-помощники -----------------------------------------------------

func findVtmResource(pl *VTMPageList, serial string, channel int) (*VTMResourceInfo, bool) {
	var bySerial []VTMResourceInfo
	for i := range pl.ResourceInfos {
		if pl.ResourceInfos[i].DeviceSerial == serial {
			bySerial = append(bySerial, pl.ResourceInfos[i])
		}
	}
	if len(bySerial) == 0 {
		return nil, false
	}
	if channel <= 0 {
		return &bySerial[0], true
	}
	ch := strconv.Itoa(channel)
	for i := range bySerial {
		if bySerial[i].LocalIndex == ch {
			return &bySerial[i], true
		}
	}
	return nil, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
