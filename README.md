# hikcloud2gortc — Hik-Connect NVR → локальный HTTP-сервис потока и снапшотов (Go)

Один контейнер `hikcloud2gortc`: логин в облако Hik-Connect, VTM-воркеры
расшифровки (HEVC/H.264, горутины + ring buffer + ffmpeg-remux) и отдача
потока и снапшотов по HTTP. RTSP-агрегатор (go2rtc) в стеке нет — поток
MPEG-TS по HTTP играют VLC/ffplay.

Полный порт на Go [`hikcloud2rtc`](https://github.com/halamov/hikcloud2rtc)
(FastAPI + `cloudcam/decrypt_proxy` + ffmpeg) — один и тот же набор эндпоинтов
и переменных окружения. Go-сервис занимает порт **8000** (python-версия
остановлена после A/B-сравнения; во время сравнения Go стоял на 8001).

## Проекты-основы

| Проект | Ссылка | Что взято |
|---|---|---|
| hikcloud2rtc (Python) | https://github.com/halamov/hikcloud2rtc | оригинал: `cloudcam/decrypt_proxy` (AES-128 по NAL, IRAP-gate, правила сессии) — портирован в `internal/decode` + `internal/worker` |
| LE-EZVIZ-VS | https://github.com/LethalEthan/LE-EZVIZ-VS | протокольный слой VTM/VTDU (бинарный протокол Hikvision, `ezproto/ezvs.go`) — **LGPL-2.1**: приватное использование допустимо, при дистрибуции бинарника LGPL-обязательства сохраняются (сообщить, дать ссылку на исходники, не приватизировать модификации) |
| cloud-cam-viewer | https://github.com/Bahrombekk/cloud-cam-viewer | движок видео, из которого в Python-оригинале вырезан `cloudcam/decrypt_proxy` |
| pyezvizapi | https://github.com/RenierM26/pyEzvizApi | протокол облака (login/token, VTM relay) — зависимость Python-оригинала, в Go переписан заново (`internal/cloudcam`) |

## Запуск

```bash
cp .env.example .env        # вписать HIK_USER / HIK_PASSWORD / HIKCLOUD2GORTC_TAG
docker compose up -d --build
```

Тег образа — время сборки: `HIKCLOUD2GORTC_TAG=0.$(date +%y%m%d.%H)`
(напр. `0.261020.00`), `latest` не использовать.

## Получение видео (поток)

Поток — **MPEG-TS, HEVC, по HTTP**. Параметры:
- `channel=N` — номер канала NVR (16 каналов; cam8 = `channel=8`)
- `stream=sub` — 640×360, ~0.2 Мбит/с
- `stream=main` — 1920×1080 (FullHD), ~1.75 Мбит/с

**ffplay (терминал):**
```bash
ffplay http://<host>:8000/stream.ts?channel=8&stream=sub
ffplay http://<host>:8000/stream.ts?channel=8&stream=main
```

**VLC:** Медиа → Открыть URL →
`http://<host>:8000/stream.ts?channel=8&stream=main`

**Записать 60 с:**
```bash
ffmpeg -i "http://<host>:8000/stream.ts?channel=8&stream=main" -t 60 -c copy rec.ts
```

Первые кадры появляются через 2–15 с (воркер открывает VTM-сессию и ждёт
ключевой кадр); дальше стрим тёплый.

## Получение снэпшота

**Из облака** — быстро (~1.5–2 с), фиксированное разрешение ~352×288:
```bash
curl -o cam8.jpg "http://<host>:8000/snapshot?channel=8"
```
(или просто открыть URL в браузере)

**FullHD-кадр из main-потока** — реальный декодированный кадр 1920×1080,
~4–6 с (ожидание ключевого кадра):
```bash
curl -o cam8_hd.jpg "http://<host>:8000/frame?channel=8&stream=main"
```

### Камера недоступна — что отвечать (опция `OFFLINE_RESPONSE`)

Если камера не отвечает (оффлайн, облако вернуло 2009 设备网络异常, 502 релея,
нет ключевого кадра), поведение `/snapshot` и `/frame` задаётся переменной
`OFFLINE_RESPONSE` (из `.env`, дефолт `error`):

- **`error`** (по умолчанию, как было исторически) — честная HTTP-ошибка:
  `/snapshot` → `502` «snapshot failed: …», `/frame` → `502` (воркер не
  открыл VTM) / `504` (нет I-кадра / таймаут). Код ошибки в body ответа.
- **`nosignal`** — **всё равно JPEG**: тёмный блок «NO SIGNAL / камера
  оффлайн» со строкой `error <код> — причина` (тот же стиль, что
  NO SIGNAL-плитка в `/preview`). URL тогда всегда отдаёт картинку —
  `<img src=…>` в дашборде рендерит её без «битой» иконки. Код ошибки и
  причина дублируются в заголовках (статус остаётся `200`):

```
X-Camera-Status: no-signal
X-Error: 2009                      # код ошибки облака / HTTP-статус причины
X-Error-Reason: API error 2009 ... # полный текст (в кадре — укороченный)
X-Camera: CH 11
```

Для «живой» камеры в обоих режимах заголовков нет, возвращается реальный
кадр. Размер NO SIGNAL-кадра: у `/snapshot` — 352×288, у `/frame` — нативное
разрешение потока (1920×1080 main / 640×360 sub).

> ⏱ Время ответа на **оффлайн**-камеру одинаково в обоих режимах — его
> задаёт ожидание облака (snapshot ~20 с, frame с ретраями ~2 мин), режим
> меняет только ФОРМАТ ответа (ошибка vs картинка), а не длительность.

## Сетка всех камер: /preview

Один JPEG со снапшотами всех камер облака:
```bash
curl -o wall.jpg "http://<host>:8000/preview"
# параметры: cell=120..640   — ширина ячейки, px (дефолт 320)
#            workers=1..16   — сколько снапшотов снимать параллельно (дефолт 8)
#            quality=40..95  — качество JPEG (дефолт 88)
```

- **Авто-раскладка** под число камер в облаке: 16 → 4×4, 12 → 4×3,
  10 → 5×2, 8 → 4×2 — равномерная сетка без «хвоста» из одиночных ячеек.
- **Недоступные камеры** не выкидываются из сетки, а рисуются блоком
  «NO SIGNAL / камера оффлайн» — видно, какие каналы молчат, на фоне
  живых кадров.
- **Шапка**: название NVR, время снимка, «онлайн N/M»; подпись под
  каждой ячейкой — `CH N` + имя камеры (длинные имена обрезаются
  многоточием по ширине ячейки).
- **Время ответа складывается из времени получения снапшотов**
  (самих рендер сетки — <0.1 с): снапшоты снимаются параллельно
  (`workers`), время = самый медленный из них. Онлайн-камера —
  ~1.5 с; оффлайн-камера тянет ~20 с. 16 камер → ~24 с.

## API

| Эндпоинт | Что |
|---|---|
| `GET /health` | статус, движок, сессия |
| `GET /cameras` | список камер NVR |
| `GET /stream.ts?channel=N&stream=sub\|main` | живой MPEG-TS |
| `GET /snapshot?channel=N` | JPEG из облака (~352×288) |
| `GET /frame?channel=N&stream=main` | FullHD JPEG из потока (1920×1080) |
| `GET /preview?cell=320&workers=8&quality=88` | сетка снапшотов всех камер (авто-раскладка, NO SIGNAL для недоступных) |
| `GET /prime?channel=N&stream=main` | прогреть воркер заранее (тёплый старт) |
| `GET /workers` | диагностика: воркеры, активные клиенты, простой (idle-timeout) |
| `GET /offline?channel=N` | проверка достижимости камеры |
| `GET /logs?tail=N` | хвост app.log; `?worker=worker_<serial>_<ch>_<mode>` — лог воркера |
| `POST /keys/request` | запрос ключа AES-ECB у облака (2FA-сценарий) |
| `POST /keys/set` | задать ключ AES-ECB камеры вручную (debug) |

Другая камера — меняется `channel=`.

## Если нужен RTSP / HLS / WebRTC

Официальный go2rtc отдельным контейнером (в стек не входит):
```yaml
services:
  go2rtc:
    image: ghcr.io/alexxit/go2rtc:1.9.14
    ports: ["8554:8554", "1984:1984"]
    environment:
      GO2RTC_CONFIG_STREAMS: >
        cam8: "ffmpeg:http://<host>:8000/stream.ts?channel=8&stream=sub#video=copy"
        cam8_main: "ffmpeg:http://<host>:8000/stream.ts?channel=8&stream=main#video=copy"
```
Потом: `rtsp://<host>:8554/cam8`, `rtsp://<host>:8554/cam8_main`,
HLS `:8080/cam8/index.m3u8`, Web-плеер `:1984`.

## Задержка: PACE_FRAMES

Облако отдаёт кадры неровно: весь GOP приходит за один раз, затем пауза ~2 с.
PacedWriter в воркере сглаживает это, выдавая кадры ровным потоком
(очередь `PACE_QUEUE_MB`, дефолт 8 МБ; при переполнении — drop до keyframe).

- `PACE_FRAMES=1` (дефолт в `docker-compose.yml`) — сглаживание: кадры
  выдаются ровным потоком, плавная картинка (глубина буфера ~1 с)
- `PACE_FRAMES=0` — кадры уходят сразу при получении: минимальная задержка,
  но 2-секундные застытия и «перемотка» в момент прихода GOP

Устанавливается в `.env`, перезапуск — `docker compose up -d`.

## Режим воркеров: WORKER_MODE

Внутри воркера (горутина на канал) работает **фоновая VTM-сессия**: облачный
поток держится открытым постоянно, данные складываются в кольцо (хвост от
последнего keyframe). Подключение `/stream.ts` или `/frame` к тёплому
воркеру — мгновенное; `/frame` ждёт только следующий I-кадр (GOP, ~4 с).
За это платим трафиком: пока воркер жив, облако гоняет ему поток 24/7
(замер: main ~1.7 Мбит/с ≈ 18 ГБ/день, sub ~0.11 Мбит/с ≈ 1.2 ГБ/день).

Два режима (`.env` → `WORKER_MODE`):

- **`always`** (по умолчанию) — воркеры живут 24/7. Любой запрос тёплый,
  минимальная задержка. Цена — постоянный трафик с облака и VTM-сессия на
  каждую используемую камеру.
- **`idle`** — воркер убивается через `WORKER_IDLE_TIMEOUT` (по умолчанию
  90 с) без клиентов: трафик только вокруг опросов. Сценарий
  «звонок в домофон → распознавание → открыть»:

  ```
  триггер звонка → GET /prime?channel=8&stream=main   # ~1–3 с, воркер грейтся
               → GET /frame?channel=8&stream=main     # ~4 с, FullHD JPEG
  ... распознавание ...
  (90 с тишины → воркер и VTM-сессия закрыты)
  ```

  Без `/prime` первый `/frame` после простоя — холодный старт (~5–8 с).
  `/prime` нужен только в режиме `idle`; в `always` воркер и так тёплый.

Выбор: камеры смотрят постоянно (VLC, дашборд) → `always`; опрос по
событию (до 1–2 раз в сутки) → `idle`.

## Токены

`token.json` / `cam_keys.json` — в volume `data` (после деплоя
`hikcloud2gortc_data`), переживают перезапуск. Плюс кеш VTM-сессий
(TTL `CLOUDCAM_VTM_TTL`, дефолт 1 ч) и кеш межключевых AES-ECB-ключей
`enc-<serial>-<ch>.json` — в том же каталоге.

## Логи

Два уровня (в `DATA_DIR`, по умолчанию `/data`):

- **app.log** — сервис (запросы, воркеры, reaper, 502-ретраи). Ротация.
  Дублируется в console → `docker logs`.
- **`worker_<serial>_<ch>_<mode>.log`** — stderr каждой VTM-сессии
  (обрывы, ключи).

Смотреть:

```bash
# из контейнера / на хосте
docker logs hikcloud2gortc --tail 100 -f
# через API (без доступа к хосту)
curl "http://<host>:8000/logs?tail=50"
curl "http://<host>:8000/logs?worker=worker_J88212190_8_main&tail=20"
```

## Сборка

```bash
docker build -t hikcloud2gortc:0.261020.00 .
# или без Docker (нужен Go 1.24):
go build -o hikcloud2gortc .
```

Многоэтапная сборка: `golang:1.24` (CGO off) → `debian:bookworm-slim`
+ ffmpeg 5.1 (remux Annex-B → TS, извлечение I-кадра) + curl (healthcheck).

## Структура

```
main.go                  входная точка
internal/config          переменные окружения
internal/cloudcam        клиент Hik-Connect: protobuf-код, VTM-сессии, снапшоты
internal/decode          дешифраторы: NAL-база (AES-ECB, IRAP), RTP, PS
internal/worker          воркеры: кольцо данных, пейсинг, менеджер, reaper
internal/api             HTTP-эндпоинты (net/http, без фреймворков)
fonts/                   DejaVu (go:embed) для NO SIGNAL-плиток и /preview
```

## Ресурсы (A/B против Python-оригинала, cam8, idle-режим, 2026-10-20)

Замер в одну сессию, запущен из Hermes-Sandbox на home-nuc2, контейнеры
только передеплоены начисто (stop → delete → deploy): Go, пауза 100 с
(reaper), затем Python. Режимы: idle-статс → cold frame → warm frame →
snapshot → sub 12 с → main 12 с (статс под нагрузкой) → reaper 100 с.

| Метрика | Python (`:8000`) | Go (`:8001` в A/B-период) |
|---|---|---|
| Память: idle / под нагрузкой / после reaper, МБ | 53 / 124 / 75 | 2.6 / 27 / 28 |
| CPU под нагрузкой (main-стрим 12 с), % | 16.7 | 13.7 |
| `/frame` холодный (воркер поднят с нуля), с | 8.0 | 4.25 |
| `/frame` тёплый, с | 1.3 (последний кадр из буфера) | 3.5 (ожидание следующего I-кадра, GOP) |
| `/snapshot`, с | 1.9 | 1.6 |
| `/stream.ts` sub, первый байт, с | 1.9 | 0.8 |
| `/stream.ts` main, первый байт, с | 0.46 | 0.47 |
| `/stream.ts` main 12 с, байты | 4 014 176 | 3 247 136 |
| Reaper (воркеры → 0), 100 с | да | да |
| Образ Docker, МБ | 633 | 548 |

Примечания: Go idle 2.6 МБ — свежий контейнер до первого воркера; холодный
frame Python (8.0 с) включает логин в Hik-Connect (контейнер только
поднят, «not logged in yet»). Предыдущий замер (контейнеры, жившие 18 ч):
Py 95/157/114 МБ, CPU 23.7% vs Go 59/71/71 МБ, CPU 13.9% — выводы те же.

Вывод для дома: Go — память в 3–5 раз меньше (idle и под нагрузкой), CPU
ниже, холодный frame вдвое быстрее, стримы паритет. Разница в `/frame`
тёплом: Python отдаёт последний кадр из буфера сразу, Go ждёт следующий
ключевой кадр (GOP ~4 с) — честный FullHD I-кадр. RTSP-агрегатор при
переезде теряется — go2rtc, если нужен, поднимается отдельным
контейнером (см. выше).
