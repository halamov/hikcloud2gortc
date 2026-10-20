// Облачный клиент Hik-Connect / EZVIZ — порт cloudcam/client.py.
//
// Сессия по трём правилам (как в Python-версии, проверено проектами
// ezviz-ha-addon / hikconnect / pyezvizapi):
//  1. featureCode привязан к установке (FeatureCode).
//  2. Повторный login при каждом запуске НЕ выполняется: сохранённый token
//     восстанавливается и обновляется (refresh); login — крайняя мера
//     (вызывает CAPTCHA 1015 и 2FA «новое устройство»).
//  3. При неудачном login пауза растёт 60s -> 30 минут.
package cloudcam

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MfaRequired — нужен код 2FA (email) для получения ключа.
type MfaRequired struct{ Msg string }

func (e *MfaRequired) Error() string { return e.Msg }

// Pлатформы: clientType и стартовый домен.
var platforms = map[string]struct {
	ClientType string
	Domain     string
	Lang       string
}{
	"hikconnect": {"55", "api.hik-connect.com", "en-US"},
	"ezviz":      {"1", "apiisgp.ezvizlife.com", "en_US"},
}

const (
	// Сессия обновляется по возрасту (не по таймеру): корректно и после
	// повторного подключения.
	sessionMaxAge = 1500.0
	loginBackoffMin = 60.0
	loginBackoffMax = 1800.0
)

type Client struct {
	mu          sync.Mutex
	Email       string
	Password    string
	Platform    string // "hikconnect" | "ezviz"
	apiURL      string // host: api.hik-connect.com
	featureCode string
	tokenFile   string

	sessionID    string
	rfSessionID  string
	sessionTime  time.Time
	userCode    string

	loginFailAt  time.Time
	loginBackoff float64

	http *http.Client
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NewClient — клиент платформы (неизвестная платформа -> hikconnect).
func NewClient(email, password, platform, tokenFile string) *Client {
	if _, ok := platforms[platform]; !ok {
		platform = "hikconnect"
	}
	return &Client{
		Email:       email,
		Password:    password,
		Platform:    platform,
		apiURL:      platforms[platform].Domain,
		featureCode: FeatureCode(tokenFile),
		tokenFile:   tokenFile,
		http:        &http.Client{Timeout: 25 * time.Second},
		loginBackoff: loginBackoffMin,
	}
}

// --- HTTP-обёртка --------------------------------------------------------

func (c *Client) baseHeaders() http.Header {
	p := platforms[c.Platform]
	h := http.Header{}
	h.Set("clientType", p.ClientType)
	h.Set("lang", p.Lang)
	h.Set("language", p.Lang)
	h.Set("featureCode", c.featureCode)
	h.Set("netType", "WIFI")
	h.Set("customno", "1000001")
	h.Set("clientNo", "web_site")
	h.Set("appId", "ys7")
	h.Set("User-Agent", "okhttp/3.12.1")
	if c.sessionID != "" {
		h.Set("sessionId", c.sessionID)
	}
	return h
}

// doJSON — request с заголовками; метод/form/query задаёт вызывающий.
func (c *Client) doJSON(method, path string, query url.Values, form url.Values) (map[string]any, error) {
	u := "https://" + c.apiURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header = c.baseHeaders()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func metaCode(data map[string]any) int {
	meta, _ := data["meta"].(map[string]any)
	if meta == nil {
		return 0
	}
	code, _ := meta["code"].(float64)
	return int(code)
}

// --- login ----------------------------------------------------------------

func (c *Client) loginOnce() error {
	// Пауза после неудачных попыток (облако блокирует частые логины).
	c.mu.Lock()
	failAt, backoff := c.loginFailAt, c.loginBackoff
	c.mu.Unlock()
	if wait := time.Until(failAt.Add(time.Duration(backoff) * time.Second)); wait > 0 {
		return fmt.Errorf("login повторится через %ds (предыдущая попытка не удалась)", int(wait.Seconds()))
	}
	var err error
	if c.Platform == "ezviz" {
		err = c.loginEzviz()
	} else {
		err = c.loginHikconnect()
	}
	c.mu.Lock()
	if err != nil {
		c.loginFailAt = time.Now()
		c.loginBackoff = min(loginBackoffMax, max(loginBackoffMin, c.loginBackoff*2))
	} else {
		c.loginFailAt = time.Time{}
		c.loginBackoff = 0
	}
	c.mu.Unlock()
	return err
}

func (c *Client) loginHikconnect() error {
	// endpoint /v2, глобальный домен, redirect по region (code 1100)
	var data map[string]any
	var err error
	for range 4 {
		data, err = c.doJSON("POST", "/v3/users/login/v2", nil, url.Values{
			"account":     {c.Email},
			"password":    {md5hex(c.Password)},
			"featureCode": {c.featureCode},
		})
		if err != nil {
			return err
		}
		switch metaCode(data) {
		case 1100: // перенаправление на другой region
			area, _ := data["loginArea"].(map[string]any)
			dom, _ := area["apiDomain"].(string)
			if dom == "" {
				return errors.New("login 1100 без apiDomain")
			}
			c.mu.Lock()
			c.apiURL = strings.TrimPrefix(dom, "https://")
			c.mu.Unlock()
			continue
		case 1013, 1014, 1226:
			return errors.New("ошибка логина: неправильный email или пароль")
		case 1015:
			return errors.New("запрошен CAPTCHA — сначала войдите в приложение Hik-Connect, затем повторите")
		}
		break
	}
	return c.finishLogin(data)
}

func (c *Client) loginEzviz() error {
	// endpoint /v5, clientType 1 (обходит terminal limit)
	data, err := c.doJSON("POST", "/v3/users/login/v5", nil, url.Values{
		"account":     {c.Email},
		"password":    {md5hex(c.Password)},
		"featureCode": {c.featureCode},
		"msgType":     {"0"},
		"cuName":      {"SSmartPhone_Android"},
	})
	if err != nil {
		return err
	}
	return c.finishLogin(data)
}

func (c *Client) finishLogin(data map[string]any) error {
	if data == nil || metaCode(data) != 200 {
		return errors.New("Ошибка логина: " + truncate(toJSON(data), 200))
	}
	sess, _ := data["loginSession"].(map[string]any)
	sid, _ := sess["sessionId"].(string)
	rf, _ := sess["rfSessionId"].(string)
	if sid == "" {
		return errors.New("login: нет sessionId")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = sid
	c.rfSessionID = rf
	if area, ok := data["loginArea"].(map[string]any); ok {
		if dom, _ := area["apiDomain"].(string); dom != "" {
			c.apiURL = strings.TrimPrefix(dom, "https://")
		}
	}
	if user, ok := data["loginUser"].(map[string]any); ok {
		c.userCode, _ = user["userCode"].(string)
	}
	c.sessionTime = time.Now()
	return nil
}

// Login — полный вход (крайняя мера).
func (c *Client) Login() error { return c.loginOnce() }

// --- token-файл (persist сессии) ------------------------------------------

type tokenFile struct {
	SessionID    string  `json:"session_id"`
	RfSessionID  string  `json:"rf_session_id"`
	Username     string  `json:"username"`
	APIURL       string  `json:"api_url"`
	Platform     string  `json:"platform"`
	FeatureCode  string  `json:"feature_code"`
	SessionTime  float64 `json:"session_time"`
}

// LoadToken — восстановить сессию (НЕ login).
// Чужой featureCode (token с другого хоста) не восстанавливается.
func (c *Client) LoadToken() bool {
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return false
	}
	var t tokenFile
	if err := json.Unmarshal(b, &t); err != nil || t.SessionID == "" {
		return false
	}
	if t.FeatureCode != "" && t.FeatureCode != c.featureCode {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = t.SessionID
	c.rfSessionID = t.RfSessionID
	c.apiURL = t.APIURL
	if t.Platform != "" {
		c.Email = t.Username
		if _, ok := platforms[t.Platform]; ok {
			c.Platform = t.Platform
		}
	}
	c.sessionTime = time.Unix(0, int64(t.SessionTime*float64(time.Second)))
	return true
}

// SaveToken — атомарная запись (mkstemp + rename).
func (c *Client) SaveToken() error {
	c.mu.Lock()
	t := tokenFile{
		SessionID:   c.sessionID,
		RfSessionID: c.rfSessionID,
		Username:    c.Email,
		APIURL:      c.apiURL,
		Platform:    c.Platform,
		FeatureCode: c.featureCode,
		SessionTime: float64(c.sessionTime.Unix()),
	}
	c.mu.Unlock()
	b, _ := json.Marshal(t)
	dir := dirOrDot(c.tokenFile)
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); cerr == nil {
		err = os.Rename(tmp.Name(), c.tokenFile)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func dirOrDot(p string) string {
	d := strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(d, "/"); i > 0 {
		return d[:i]
	}
	return "."
}

// --- refresh по возрасту ---------------------------------------------------

// IsRefreshNeeded — пора ли обновить сессию (возраст >= maxAge).
func (c *Client) IsRefreshNeeded(maxAge time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID == "" {
		return true
	}
	return time.Since(c.sessionTime) >= maxAge
}

// SessionAge — возраст текущей сессии.
func (c *Client) SessionAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID == "" {
		return time.Duration(1e18)
	}
	return time.Since(c.sessionTime)
}

// SessionID — текущий sessionId ("" если не залогинены).
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// RefreshSession — refresh token без пароля/MFA.
// allowLogin=false — бросает ошибку, если refresh не сработал.
func (c *Client) RefreshSession(allowLogin bool) error {
	c.mu.Lock()
	rf := c.rfSessionID
	c.mu.Unlock()
	data, err := c.doJSON("PUT", "/v3/apigateway/login", nil, url.Values{
		"refreshSessionId": {rf},
		"featureCode":      {c.featureCode},
	})
	if err == nil {
		if metaCode(data) == 200 {
			info, _ := data["sessionInfo"].(map[string]any)
			c.mu.Lock()
			if sid, ok := info["sessionId"].(string); ok {
				c.sessionID = sid
			}
			if rs, ok := info["refreshSessionId"].(string); ok {
				c.rfSessionID = rs
			}
			c.sessionTime = time.Now()
			c.mu.Unlock()
			return nil
		}
	}
	if !allowLogin {
		return errors.New("не удалось обновить сессию (refresh token устарел)")
	}
	return c.Login()
}

// Connect — привесть сессию в готовый режим (без login по возможности).
// Возвращает: "resumed" | "refreshed" | "login".
func (c *Client) Connect() (string, error) {
	if c.LoadToken() && !c.IsRefreshNeeded(time.Duration(sessionMaxAge) * time.Second) {
		return "resumed", nil
	}
	if c.LoadToken() {
		if err := c.RefreshSession(false); err == nil {
			_ = c.SaveToken()
			return "refreshed", nil
		}
	}
	if err := c.Login(); err != nil {
		return "", err
	}
	_ = c.SaveToken()
	return "login", nil
}

// --- service URLs / VTDU token --------------------------------------------

// ServiceURLs — GET /v3/configurations/system/info (кэшируется).
func (c *Client) ServiceURLs() (map[string]any, error) {
	data, err := c.doJSON("GET", "/v3/configurations/system/info", nil, nil)
	if err != nil {
		return nil, err
	}
	su, _ := data["systemConfigInfo"].(map[string]any)
	return su, nil
}

var apiHostRe = regexp.MustCompile(`apii([a-z]+)\.`)

// AuthAddr — адрес VTDU-token сервиса.
// Hik-Connect отдаёт authAddr = "https://null" — выводим из region:
// apiisgp.hik-connect.com -> sgpauth.ezvizlife.com.
func (c *Client) AuthAddr() (string, error) {
	su, err := c.ServiceURLs()
	if err != nil {
		return "", err
	}
	addr, _ := su["authAddr"].(string)
	if addr == "" || strings.Contains(strings.ToLower(addr), "null") {
		c.mu.Lock()
		api := c.apiURL
		c.mu.Unlock()
		if m := apiHostRe.FindStringSubmatch(api); m != nil {
			return "https://" + m[1] + "auth.ezvizlife.com", nil
		}
		return "", errors.New("authAddr пуст, а region из api_url не извлечь")
	}
	if !strings.HasPrefix(addr, "http") {
		addr = "https://" + addr
	}
	return addr, nil
}

// SessionSign — claim "s" из JWT sessionId (для /vtdutoken2).
func (c *Client) SessionSign() (string, error) {
	sid := c.SessionID()
	parts := strings.Split(sid, ".")
	if len(parts) < 2 {
		return "", errors.New("sessionId не JWT")
	}
	payload := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
	raw, err := base64URLDecode(payload)
	if err != nil {
		return "", err
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", err
	}
	s, _ := claims["s"].(string)
	if s == "" {
		return "", errors.New("в sessionId JWT нет claim 's'")
	}
	return s, nil
}

// VTDUTokenV2 — GET {auth}/vtdutoken2?ssid=&sign= -> tokens[].
func (c *Client) VTDUTokenV2() ([]string, error) {
	sign, err := c.SessionSign()
	if err != nil {
		return nil, err
	}
	auth, err := c.AuthAddr()
	if err != nil {
		return nil, err
	}
	sid := c.SessionID()
	req, err := http.NewRequest("GET",
		auth+"/vtdutoken2?"+url.Values{
			"ssid": {sid}, "sign": {sign},
		}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.baseHeaders()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Message string   `json:"msg"`
		Tokens  []string `json:"tokens"`
		Retcode int      `json:"retcode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("vtdutoken2: %w (%s)", err, truncate(string(data), 200))
	}
	if out.Retcode != 0 {
		return nil, fmt.Errorf("vtdutoken2 retcode %d", out.Retcode)
	}
	return out.Tokens, nil
}

// --- pagelists --------------------------------------------------------------

// VTMResource — запись VTM-релея из pagelist.
type VTMResource struct {
	ExternalIP string `json:"externalIp"`
	InternalIP string `json:"internalIp"`
	Port       int    `json:"port"`
	Domain     string `json:"domain"`
	PublicKey  struct {
		Key     string `json:"key"`
		Version int    `json:"version"`
	} `json:"publicKey"`
}

// VTMResourceInfo — канал (ресурс) из pagelist.
type VTMResourceInfo struct {
	ResourceID     string `json:"resourceId"`
	ResourceName   string `json:"resourceName"`
	DeviceSerial   string `json:"deviceSerial"`
	LocalIndex     string `json:"localIndex"`
	StreamBizUrl   string `json:"streamBizUrl"`
	ChannelNo      *int   `json:"channelNo"`
	DeviceName     string `json:"deviceName"`
}

// VTMPageList — полный (все страницы) VTM pagelist.
type VTMPageList struct {
	ResourceInfos []VTMResourceInfo
	VTM           map[string]VTMResource
}

// GetVtmPageList — все страницы /v3/userdevices/v1/resources/pagelist?filter=VTM.
// clientType в заголовке (без него pagelist не вернёт все ресурсы).
func (c *Client) GetVtmPageList() (*VTMPageList, error) {
	out := &VTMPageList{VTM: map[string]VTMResource{}}
	offset := 0
	for range 40 { // 40 x 50 = 2000 ресурсов — защита от цикла
		data, err := c.doJSON("GET", "/v3/userdevices/v1/resources/pagelist",
			url.Values{
				"filter":  {"VTM"},
				"groupId": {"-1"},
				"limit":   {"50"},
				"offset":  {strconv.Itoa(offset)},
			}, nil)
		if err != nil {
			return nil, err
		}
		res, _ := data["resourceInfos"].([]any)
		for _, r := range res {
			var ri VTMResourceInfo
			b, _ := json.Marshal(r)
			_ = json.Unmarshal(b, &ri)
			out.ResourceInfos = append(out.ResourceInfos, ri)
		}
		if vtm, ok := data["VTM"].(map[string]any); ok {
			for k, v := range vtm {
				b, _ := json.Marshal(v)
				var vr VTMResource
				if json.Unmarshal(b, &vr) == nil {
					out.VTM[k] = vr
				}
			}
		}
		page, _ := data["page"].(map[string]any)
		hasNext, _ := page["hasNext"].(bool)
		if len(res) == 0 || !hasNext {
			break
		}
		offset += len(res)
	}
	return out, nil
}

// ListCameras — список реальных камер: (serial, channel, имя).
// channel = localIndex (0 = само устройство, не камера).
// Имя = «Имя устройства - имя канала» (как в приложении).
func (c *Client) ListCameras() ([]Camera, error) {
	pl, err := c.GetVtmPageList()
	if err != nil {
		return nil, err
	}
	devNames, _ := c.DeviceNames()
	seen := map[string]bool{}
	out := []Camera{}
	for _, ri := range pl.ResourceInfos {
		ch, err := strconv.Atoi(ri.LocalIndex)
		if err != nil || ri.DeviceSerial == "" {
			continue
		}
		if ch < 1 { // localIndex 0 = само устройство (NVR), не камера
			continue
		}
		k := ri.DeviceSerial + "#" + strconv.Itoa(ch)
		if seen[k] {
			continue
		}
		seen[k] = true
		chName := strings.TrimSpace(ri.ResourceName)
		devName := devNames[ri.DeviceSerial]
		label := chName
		if devName != "" && chName != "" &&
			!strings.Contains(strings.ToLower(chName), strings.ToLower(devName)) {
			label = devName + " - " + chName
		}
		if label == "" {
			label = fmt.Sprintf("%s CH%d", ri.DeviceSerial, ch)
		}
		out = append(out, Camera{
			Serial:     ri.DeviceSerial,
			Channel:    ch,
			Name:       label,
			DeviceName: devName,
		})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].Serial != out[j-1].Serial || out[j].Channel < out[j-1].Channel); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// DeviceNames — {serial: имя} из devices/pagelist (имена из приложения).
func (c *Client) DeviceNames() (map[string]string, error) {
	names := map[string]string{}
	offset := 0
	for range 40 {
		data, err := c.doJSON("GET", "/v3/userdevices/v1/devices/pagelist",
			url.Values{
				"filter":  {"CONNECTION"},
				"groupId": {"-1"},
				"limit":   {"50"},
				"offset":  {strconv.Itoa(offset)},
			}, nil)
		if err != nil {
			return names, err
		}
		di, _ := data["deviceInfos"].([]any)
		for _, d := range di {
			m, _ := d.(map[string]any)
			serial, _ := m["deviceSerial"].(string)
			name, _ := m["name"].(string)
			if serial != "" {
				names[serial] = strings.TrimSpace(name)
			}
		}
		page, _ := data["page"].(map[string]any)
		hasNext, _ := page["hasNext"].(bool)
		if len(di) == 0 || !hasNext {
			break
		}
		offset += len(di)
	}
	return names, nil
}

// --- ключ камеры (2FA elevation) --------------------------------------------

// GetCamKey — шифровальный ключ камеры (encryptkey).
// resultCode 0 -> ключ; 20002 -> MfaRequired; 2009 -> устройство не подключено.
func (c *Client) GetCamKey(serial string, channel int, mfaCode string) (string, error) {
	form := url.Values{
		"checkcode":   {mfaCode},
		"serial":      {serial},
		"clientNo":    {"web_site"},
		"clientType":  {"3"},
		"netType":     {"WIFI"},
		"featureCode": {c.featureCode},
		"sessionId":   {c.SessionID()},
	}
	data, err := c.doJSON("POST", "/api/device/query/encryptkey", nil, form)
	if err != nil {
		return "", err
	}
	code := strconv.Itoa(int(metaCode(data)))
	if rc, ok := data["resultCode"]; ok {
		if f, ok := rc.(float64); ok {
			code = strconv.Itoa(int(f))
		}
	}
	switch code {
	case "0":
		key, _ := data["encryptkey"].(string)
		return key, nil
	case "20002":
		return "", &MfaRequired{Msg: "требуется код 2FA (resultCode 20002)"}
	case "10001":
		return "", errors.New("повторный запрос — подождите 1-2 минуты и повторите (10001)")
	case "2009":
		return "", errors.New("устройство не подключено (2009)")
	}
	return "", fmt.Errorf("не удалось получить ключ (resultCode %s)", code)
}

// SendMfaCode — отправка кода 2FA (elevation) на email.
func (c *Client) SendMfaCode() error {
	_, err := c.doJSON("POST", "/v3/sms/nologin/checkcode", nil, url.Values{
		"from":    {c.Email},
		"bizType": {"TERMINAL_BIND"},
	})
	return err
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func base64URLDecode(s string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(s)
}
