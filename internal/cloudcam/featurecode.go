// Идентификатор терминала (featureCode) — порт cloudcam/identity.py.
//
// Облако узнаёт «терминал» по этому коду: лимит входа, CAPTCHA и 2FA
// «новое устройство» завязаны на него. Код должен быть привязан к ЭТОЙ
// УСТАНОВКЕ: берётся из MAC-адреса (на одном хосте всегда один и тот же) и
// пишется в файл рядом с token.json — при смене адаптера/реконтейнера
// терминал не меняется.
package cloudcam

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const fcFile = "feature_code.txt"

var (
	fcCache   string
	fcCacheMu sync.Mutex
)

// MAC из первого не-virtual сетевого интерфейса; fallback — hostname-hash.
func macFromSystem() string {
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, ifc := range ifaces {
			mac := ifc.HardwareAddr
			if len(mac) == 6 {
				return mac.String()
			}
		}
	}
	// Fallback: стабильный псевдо-MAC из hostname
	h, _ := os.Hostname()
	sum := md5.Sum([]byte("hikcloud2gortc:" + h))
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		sum[0], sum[1], sum[2], sum[3], sum[4], sum[5])
}

func featureCodeFromMAC() string {
	sum := md5.Sum([]byte(macFromSystem()))
	return hex.EncodeToString(sum[:])
}

// FeatureCode — код терминала этой установки (32 hex).
// Приоритет: CLOUDCAM_FEATURE_CODE -> файл рядом с tokenFile -> расчёт из MAC.
func FeatureCode(tokenFile string) string {
	fcCacheMu.Lock()
	defer fcCacheMu.Unlock()
	if fcCache != "" {
		return fcCache
	}
	if env := strings.TrimSpace(os.Getenv("CLOUDCAM_FEATURE_CODE")); env != "" {
		fcCache = env
		return fcCache
	}
	path := filepath.Join(filepath.Dir(tokenFile), fcFile)
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			fcCache = s
			return fcCache
		}
	}
	code := featureCodeFromMAC()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(code), 0o644) // даже если не сможем — MAC стабилен
	fcCache = code
	return fcCache
}
