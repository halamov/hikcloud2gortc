// Кеш решения об inter-шифре — файл enc-<serial>-<channel>.json рядом с
// кешем VTM. Решение — неизменное свойство КАМЕРЫ; с кешом первый кадр
// уходит сразу (экономия ~0.7s на каждом открытии), и всё равно проверяется —
// при смене поведения камеры кеш сам себя исправляет.
package decode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func encCachePath(dir, key string) string {
	var b strings.Builder
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return filepath.Join(dir, "enc-"+b.String()+".json")
}

// EncCacheLoad — решение из прошлого открытия (nil = отсутствует/повреждено).
func EncCacheLoad(dir, key string) *bool {
	b, err := os.ReadFile(encCachePath(dir, key))
	if err != nil {
		return nil
	}
	var v struct {
		InterEnc bool `json:"inter_enc"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return &v.InterEnc
}

// EncCacheStore — атомарная запись; ошибка не возвращает (кеш — оптимизация).
func EncCacheStore(dir, key string, value bool) {
	b, _ := json.Marshal(map[string]any{"inter_enc": value})
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".enc-*.tmp")
	if err != nil {
		return
	}
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), encCachePath(dir, key))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}
