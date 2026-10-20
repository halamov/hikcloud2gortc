// Package config — настройки сервиса из переменных окружения.
//
// Пользовательские имена (как в hikcloud2rtc): HIK_*/WORKER_*/STREAM/CHANNEL...
// CLOUDCAM_* — внутренние имена файлов сессии (token/cam_keys), их же читает
// пакет cloudcam.
package config

import (
	"os"
	"strconv"
)

type Config struct {
	// Учётная запись
	User     string // HIK_USER
	Password string // HIK_PASSWORD
	Platform string // HIK_PLATFORM: hikconnect (дефолт) | ezviz

	// Сервер
	Port int // PORT, дефолт 8000

	// Поток по умолчанию
	Channel int    // CHANNEL (0 = первая активная)
	Stream  string // STREAM: sub|main|auto (дефолт sub)

	// Ответы на недоступную камеру: error (дефолт) | nosignal
	OfflineResponse string

	// ALLOW_LOGIN_FAIL=1 — стартовать, если логин в облаке не удался
	// (сервис будет работать, когда облако вернётся / токен валиден).
	AllowLoginFail bool

	// Режим воркеров: always (дефолт) | idle
	WorkerMode        string
	WorkerIdleTimeout float64 // WORKER_IDLE_TIMEOUT, дефолт 90

	// Сглаживание GOP-bursts: PACE_FRAMES (1=вкл по умолчанию, 0=выкл)
	PaceFrames bool

	// Пути (volume data)
	DataDir     string // DATA_DIR, дефолт /app/data
	TokenFile   string // CLOUDCAM_TOKEN_FILE
	CamKeysFile string // CLOUDCAM_CAMKEY_FILE
}

// PaceFrames из env: "0"/"false"/"off"/"" = выкл, иначе вкл.
func paceFramesFromEnv() bool {
	v := os.Getenv("PACE_FRAMES")
	switch v {
	case "0", "false", "off", "":
		return false
	}
	return true
}

func load() Config {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	c := Config{
		User:            os.Getenv("HIK_USER"),
		Password:        os.Getenv("HIK_PASSWORD"),
		Port:            8000,
		Channel:         0,
		Stream:          "sub",
		OfflineResponse: "error",
		WorkerMode:      "always",
		WorkerIdleTimeout: 90,
		PaceFrames:      paceFramesFromEnv(),
		DataDir:         dataDir,
		TokenFile:       dataDir + "/token.json",
		CamKeysFile:     dataDir + "/cam_keys.json",
	}
	if v := os.Getenv("HIK_PLATFORM"); v != "" {
		c.Platform = v
	} else {
		c.Platform = "hikconnect"
	}
	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Port = p
		}
	}
	if v := os.Getenv("CHANNEL"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Channel = p
		}
	}
	if v := os.Getenv("STREAM"); v != "" {
		c.Stream = v
	}
	if v := os.Getenv("OFFLINE_RESPONSE"); v != "" {
		c.OfflineResponse = v
	}
	if os.Getenv("ALLOW_LOGIN_FAIL") == "1" {
		c.AllowLoginFail = true
	}
	if v := os.Getenv("WORKER_MODE"); v != "" {
		c.WorkerMode = v
	}
	if v := os.Getenv("WORKER_IDLE_TIMEOUT"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			c.WorkerIdleTimeout = p
		}
	}
	if v := os.Getenv("CLOUDCAM_TOKEN_FILE"); v != "" {
		c.TokenFile = v
	}
	if v := os.Getenv("CLOUDCAM_CAMKEY_FILE"); v != "" {
		c.CamKeysFile = v
	}
	// Приведение значений в валидные (как в app.py)
	if c.OfflineResponse != "nosignal" {
		if c.OfflineResponse != "" && c.OfflineResponse != "error" {
			c.OfflineResponse = "error"
		}
	}
	if c.WorkerMode != "idle" {
		c.WorkerMode = "always"
	}
	if c.Stream != "sub" && c.Stream != "main" && c.Stream != "auto" {
		c.Stream = "sub"
	}
	return c
}

var C = load()
