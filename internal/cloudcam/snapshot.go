// SnapClient — снапшоты облака (порт hikcloudstream: login v3/users/login/v2,
// devices pagelist + cameras/info, capture v3/devices/security/capture).
// Отдельный от VTM-клиента: свой featureCode/сессия, как в Python-версии.
package cloudcam

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Camera — канал камеры в аккаунте.
type Camera struct {
	Index       int
	Name        string
	Serial      string // deviceSerial
	Channel     int    // channelNo
	DeviceName  string
}

// SnapClient — REST-клиент снапшотов Hik-Connect.
type SnapClient struct {
	user, pass string
	baseURL    string
	feature    string
	sessionID  string
	loginAt    time.Time

	log *slog.Logger

	mu   sync.Mutex
	http *http.Client
}

// snapSessionTTL — проактивный перезалив: сессия снапшот-клиента облака
// живёт ограниченное время (~4–5 мин без активности; активный опрос —
// 14+ мин, замер 2026-10-20). После истечения pagelist молча возвращает
// meta 200 с нулём устройств вместо 401, и снапшоты падают «no active
// cameras in cloud». Перезаливаемся сами, пока сессия ещё в пределах TTL.
const snapSessionTTL = 4 * time.Minute

func NewSnapClient(user, pass string, log *slog.Logger) *SnapClient {
	fc, _ := randHex(16)
	if log == nil {
		log = slog.Default()
	}
	return &SnapClient{
		user:    user,
		pass:    pass,
		baseURL: "https://api.hik-connect.com",
		feature: fc,
		log:     log,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *SnapClient) headers(withSession bool) http.Header {
	h := http.Header{}
	h.Set("clientType", "55")
	h.Set("clientVersion", "6.0.0.20250101")
	h.Set("featureCode", s.feature)
	h.Set("lang", "en-US")
	h.Set("clientNo", "hikcloudstream")
	h.Set("clientName", "hikcloudstream")
	h.Set("osVersion", "Linux")
	h.Set("netType", "WIFI")
	if withSession && s.sessionID != "" {
		h.Set("sessionId", s.sessionID)
	}
	return h
}

func snapMetaCode(payload map[string]any) (int, string) {
	m, _ := payload["meta"].(map[string]any)
	code := 0
	msg := ""
	if m != nil {
		if c, ok := m["code"].(float64); ok {
			code = int(c)
		}
		if x, ok := m["message"].(string); ok {
			msg = x
		} else if x, ok := m["langMsg"].(string); ok {
			msg = x
		}
	}
	return code, msg
}

func checkMeta(payload map[string]any) error {
	code, msg := snapMetaCode(payload)
	if code == 200 || len(payload) == 0 {
		return nil
	}
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Errorf("API %d: %s", code, msg)
}

// Login — (пере)логин с редиректом loginArea (meta 1100).
func (s *SnapClient) Login() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.loginOnce()
	if err != nil {
		s.log.Error("snap: логин в облако не удался", "err", err)
		return err
	}
	s.log.Info("snap: логин в облако ok", "domain", s.baseURL)
	return nil
}

// ensureSession — живая сессия: либо свежая (моложе snapSessionTTL), либо
// новый логин. Вызывать вне s.mu.
func (s *SnapClient) ensureSession() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" && time.Since(s.loginAt) < snapSessionTTL {
		return nil
	}
	err := s.loginOnce()
	if err != nil {
		s.log.Error("snap: ре-логин не удался", "err", err)
		return err
	}
	s.log.Info("snap: ре-логин в облако ok (сессия протухла/нет)", "domain", s.baseURL)
	return nil
}

func (s *SnapClient) loginOnce() error {
	sum := md5.Sum([]byte(s.pass))
	form := url.Values{}
	form.Set("account", s.user)
	form.Set("password", hex.EncodeToString(sum[:]))
	form.Set("featureCode", s.feature)
	req, err := http.NewRequest("POST", s.baseURL+"/v3/users/login/v2", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header = s.headers(false)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return fmt.Errorf("login: не-JSON ответ (%d)", resp.StatusCode)
	}
	code, _ := snapMetaCode(payload)
	switch code {
	case 1100:
		area, _ := payload["loginArea"].(map[string]any)
		domain, _ := area["apiDomain"].(string)
		if domain == "" {
			return errors.New("login: редирект без apiDomain")
		}
		s.baseURL = "https://" + domain
		return s.loginOnce()
	case 1013, 1014:
		return errors.New("неверный логин или пароль")
	case 1015:
		return errors.New("нужен CAPTCHA: войдите в приложение Hik-Connect один раз")
	}
	if err := checkMeta(payload); err != nil {
		return err
	}
	sess, _ := payload["loginSession"].(map[string]any)
	sid, _ := sess["sessionId"].(string)
	if sid == "" {
		return errors.New("login: нет sessionId")
	}
	s.sessionID = sid
	s.loginAt = time.Now()
	return nil
}

// requestJSON — GET/POST с проверкой meta.
func (s *SnapClient) requestJSON(method, path string, query url.Values, body string) (map[string]any, error) {
	u := path
	if !strings.HasPrefix(path, "http") {
		u = s.baseURL + path
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header = s.headers(true)
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("не-JSON ответ %s (%d): %s", path, resp.StatusCode, string(b[:min(len(b), 200)]))
	}
	return payload, checkMeta(payload)
}

func (s *SnapClient) requestJSONAuth(method, path string, query url.Values, body string) (map[string]any, error) {
	if err := s.ensureSession(); err != nil {
		return nil, err
	}
	p, err := s.requestJSON(method, path, query, body)
	if err == nil {
		return p, nil
	}
	// 401/403 от облака — сессия истекла: перезалогин и один повтор
	if isAuthReject(err) {
		if err := s.Login(); err != nil {
			return nil, err
		}
		return s.requestJSON(method, path, query, body)
	}
	return nil, err
}

func isAuthReject(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden")
}

const deviceFilter = "TIME_PLAN,CONNECTION,SWITCH,STATUS,STATUS_EXT,WIFI,NODISTURB,P2P,KMS,HIDDNS"

// ListCameras — все активные каналы аккаунта (портировка list_cameras).
// Пустой результат — подозрительный: облако молча роняет просроченную
// сессию (meta 200, deviceInfos=[]), поэтому один раз форсируем ре-логин
// и повторяем. Найдено в проде 2026-10-20: снапшоты падали «no active
// cameras in cloud» через ~9 мин после запуска контейнера.
func (s *SnapClient) ListCameras() ([]Camera, error) {
	out, err := s.listCamerasOnce()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		s.log.Info("snap: список камер пуст — принудительный ре-логин")
		if err := s.Login(); err != nil {
			return nil, err
		}
		out, err = s.listCamerasOnce()
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			s.log.Warn("snap: камер нет даже после ре-логина")
		}
	}
	return out, nil
}

func (s *SnapClient) listCamerasOnce() ([]Camera, error) {
	out := []Camera{}
	offset := 0
	for {
		q := url.Values{}
		q.Set("groupId", "-1")
		q.Set("limit", "50")
		q.Set("offset", fmt.Sprint(offset))
		q.Set("filter", deviceFilter)
		payload, err := s.requestJSONAuth("GET", "/v3/userdevices/v1/devices/pagelist", q, "")
		if err != nil {
			return nil, err
		}
		devices, _ := payload["deviceInfos"].([]any)
		for _, dv := range devices {
			d, _ := dv.(map[string]any)
			serial, _ := d["deviceSerial"].(string)
			if serial == "" {
				continue
			}
			name, _ := d["name"].(string)
			if name == "" {
				name = serial
			}
			cams, err := s.camerasForDevice(serial, name)
			if err == nil {
				out = append(out, cams...)
			}
		}
		page, _ := payload["page"].(map[string]any)
		hasNext, _ := page["hasNext"].(bool)
		if !hasNext {
			break
		}
		offset += 50
	}
	for i := range out {
		out[i].Index = i + 1
	}
	return out, nil
}

func (s *SnapClient) camerasForDevice(serial, deviceName string) ([]Camera, error) {
	q := url.Values{}
	q.Set("deviceSerial", serial)
	payload, err := s.requestJSON("GET", "/v3/userdevices/v1/cameras/info", q, "")
	if err != nil {
		return nil, err
	}
	items, _ := payload["cameraInfos"].([]any)
	out := []Camera{}
	for _, it := range items {
		c, _ := it.(map[string]any)
		ch, ok := c["channelNo"].(float64)
		if !ok {
			continue
		}
		name, _ := c["cameraName"].(string)
		if name == "" {
			name = fmt.Sprintf("Channel %d", int(ch))
		}
		out = append(out, Camera{Name: name, Serial: serial, Channel: int(ch), DeviceName: deviceName})
	}
	// sort by channel
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Channel < out[j-1].Channel; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Capture — JPEG снапшота камеры (до 352×288). validateCode — для зашифрованных.
func (s *SnapClient) Capture(cam Camera, validateCode string) ([]byte, error) {
	var lastErr error
	var encPayload []byte
	for _, isEnc := range []int{0, 1} {
		raw, err := s.captureOnce(cam, isEnc)
		if err != nil {
			lastErr = err
			continue
		}
		if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xD8 {
			return trimJPEG(raw), nil
		}
		if isEnc == 1 {
			encPayload = raw
		}
	}
	if encPayload != nil {
		dec, err := decryptCapture(encPayload, validateCode)
		if err == nil && len(dec) >= 2 && dec[0] == 0xFF && dec[1] == 0xD8 {
			return trimJPEG(dec), nil
		}
		return nil, fmt.Errorf("снапшот зашифрован: нужен validate_code (%v)", lastErr)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("снапшот: картинка не возвращена")
}

func (s *SnapClient) captureOnce(cam Camera, isEncrypted int) ([]byte, error) {
	q := url.Values{}
	q.Set("deviceSerial", cam.Serial)
	q.Set("channelNo", fmt.Sprint(cam.Channel))
	q.Set("isEncrypted", fmt.Sprint(isEncrypted))
	payload, err := s.requestJSONAuth("PUT", "/v3/devices/security/capture", q, "")
	if err != nil {
		return nil, err
	}
	info, _ := payload["captureInfo"].(map[string]any)
	picURL, _ := info["picUrl"].(string)
	if picURL == "" {
		return nil, errors.New("capture: нет picUrl")
	}
	resp, err := s.http.Get(picURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("capture: пустой ответ")
	}
	return raw, nil
}

// trimJPEG — обрезать хвост после EOI (0xFFD9), как в Python-версии.
func trimJPEG(b []byte) []byte {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return b
	}
	i := bytesLastIndex(b, 0xFF, 0xD9)
	if i >= 0 {
		return b[:i+2]
	}
	return b
}

func bytesLastIndex(b []byte, a, c byte) int {
	for i := len(b) - 2; i >= 0; i-- {
		if b[i] == a && b[i+1] == c {
			return i
		}
	}
	return -1
}

// decryptCapture — AES-CBC «hikencodepicture»-снапшотов (как hikcloudstream).
func decryptCapture(raw []byte, validateCode string) ([]byte, error) {
	if !strings.HasPrefix(string(raw), "hikencodepicture") {
		return nil, errors.New("неизвестный формат снапшота")
	}
	if len(raw) <= 48 {
		return nil, errors.New("зашифрованный снапшот слишком мал")
	}
	key := []byte(validateCode)
	if len(key) < 16 {
		key = append(key, make([]byte, 16-len(key))...)
	}
	key = key[:16]
	iv := make([]byte, 16)
	copy(iv, "01234567") // как в Python: "01234567" + 8 нулей
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(raw)-48)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, raw[48:])
	// unpad PKCS7 (best effort)
	if n := int(out[len(out)-1]); n > 0 && n <= 16 {
		out = out[:len(out)-n]
	}
	return out, nil
}

// SessionSign — sign-claim из JWT sessionId (нужен для VTDU из снапшот-сессии).
func (s *SnapClient) SessionSign() (string, error) {
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	if sid == "" {
		return "", errors.New("не залогинен")
	}
	parts := strings.Split(sid, ".")
	if len(parts) < 2 {
		return "", errors.New("sessionId не JWT")
	}
	payload := parts[1] + strings.Repeat("=", (-len(parts[1]))%4)
	b, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", err
	}
	var claims map[string]any
	if err := json.Unmarshal(b, &claims); err != nil {
		return "", err
	}
	sign, _ := claims["s"].(string)
	if sign == "" {
		return "", errors.New("JWT без claim s")
	}
	return sign, nil
}
