// Файловый кеш облачных метаданных для УЧЁТНОЙ ЗАПИСИ — порт vtm_cache.py.
//
// Pagelist (многостраничный, ~4s) и VTDU token (~1.8s) относятся к АККАУНТУ,
// а не к камере. Тёплый кеш: открытие потока 0.2s вместо 5.8s.
// Кеш инвалидируется ТОЛЬКО когда поток вообще не открылся — если хотя бы
// один пакет дошёл, кеш был верен.
package cloudcam

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultVTMTTL = 3600.0

func vtmTTL() time.Duration {
	if v := os.Getenv("CLOUDCAM_VTM_TTL"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return time.Duration(defaultVTMTTL * float64(time.Second))
}

type VtmCache struct {
	dir     string // рядом с token.json
	account string // имя файла токена без расширения
}

func NewVtmCache(tokenFile string) *VtmCache {
	dir := filepath.Dir(tokenFile)
	name := filepath.Base(tokenFile)
	return &VtmCache{dir: dir, account: strings.TrimSuffix(name, filepath.Ext(name))}
}

func (c *VtmCache) path(kind string) string {
	return filepath.Join(c.dir, "vtm-"+safeName(c.account)+"-"+kind+".json")
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type cacheBlob struct {
	Ts   float64 `json:"ts"`
	Data any     `json:"data"`
}

// Load — значение, если кеш свежий, иначе false.
func (c *VtmCache) Load(kind string, dst any) bool {
	b, err := os.ReadFile(c.path(kind))
	if err != nil {
		return false
	}
	var blob struct {
		Ts   float64         `json:"ts"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &blob); err != nil {
		return false
	}
	if time.Since(time.Unix(0, int64(blob.Ts*float64(time.Second)))) > vtmTTL() {
		return false
	}
	return json.Unmarshal(blob.Data, dst) == nil
}

// Store — атомарная запись; ошибки не возвращает (кеш — оптимизация).
func (c *VtmCache) Store(kind string, data any) {
	blob := cacheBlob{Ts: float64(time.Now().Unix()), Data: data}
	b, err := json.Marshal(blob)
	if err != nil {
		return
	}
	_ = os.MkdirAll(c.dir, 0o755)
	tmp, err := os.CreateTemp(c.dir, "."+kind+"-*.tmp")
	if err != nil {
		return
	}
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path(kind))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

// Invalidate — снос кеша (pagelist+vtdu). Вызывать ТОЛЬКО когда поток не открыт.
func (c *VtmCache) Invalidate() {
	os.Remove(c.path("pagelist"))
	os.Remove(c.path("vtdu"))
}

// Pagelist — полный (все страницы) VTM pagelist, из кеша или облака.
func (c *Client) Pagelist(cache *VtmCache) (*VTMPageList, error) {
	if cache != nil {
		var cached VTMPageList
		if cache.Load("pagelist", &cached) && !isErrorData(&cached) {
			return &cached, nil
		}
	}
	pl, err := c.GetVtmPageList()
	if err != nil {
		return nil, err
	}
	if cache != nil && !isErrorData(pl) {
		cache.Store("pagelist", pl)
	}
	return pl, nil
}

func isErrorData(pl *VTMPageList) bool {
	// облако отдало пустой список при ошибке — не кешируем пустоту
	return len(pl.ResourceInfos) == 0
}

// VTDUTokens — VTDU token v2, из кеша или облака.
func (c *Client) VTDUTokensCached(cache *VtmCache) ([]string, error) {
	if cache != nil {
		var cached struct {
			Tokens []string `json:"tokens"`
		}
		if cache.Load("vtdu", &cached) && len(cached.Tokens) > 0 {
			return cached.Tokens, nil
		}
	}
	tokens, err := c.VTDUTokenV2()
	if err != nil {
		return nil, err
	}
	if cache != nil && len(tokens) > 0 {
		cache.Store("vtdu", struct {
			Tokens []string `json:"tokens"`
		}{Tokens: tokens})
	}
	return tokens, nil
}

// --- кеш решения об inter-шифре — см. internal/decode/enc_cache.go ---------




