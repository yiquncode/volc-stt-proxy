# volc-stt-proxy

一个本地 HTTP 代理：对外提供 OpenAI 语音转文字接口（`POST /v1/audio/transcriptions`），
内部把音频用 ffmpeg 转成 16 kHz 单声道 PCM，通过 WebSocket 发给火山引擎「豆包流式语音识别模型 2.0」的
**一句话识别**模式（`wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream`），返回整句结果。
用途是让 Spokenly 等只支持 OpenAI 接口的听写工具使用豆包语音识别。

> 早期版本使用的「大模型录音文件极速版识别」（flash，`volc.bigasr.auc_turbo`）已不再支持。

依赖：Go（唯一第三方库 `github.com/coder/websocket`），本机 ffmpeg（`brew install ffmpeg`）。

## 构建

```sh
go build -o bin/volc-stt-proxy .   # 或 make build
go test ./...                      # 或 make test
```

## 火山引擎控制台准备

1. 在豆包语音控制台开通 **豆包流式语音识别模型 2.0**，二选一：
   - 小时版 → 资源 ID `volc.seedasr.sauc.duration`（默认值）
   - 并发版 → 资源 ID `volc.seedasr.sauc.concurrent`（需设置 `VOLC_RESOURCE_ID`）

   20 小时免费额度需要在控制台点击「试用」领取，不会自动生效。
2. 获取密钥，二选一（两种密钥**不能混用**；火山方舟 / Ark 的 API Key **不能用**）：
   - 新版控制台「API Key 管理」里的 API Key → `VOLC_API_KEY`（请求头 `X-Api-Key`）
   - 旧版控制台的 APP ID + Access Token → `VOLC_APP_KEY` + `VOLC_ACCESS_KEY`
     （请求头 `X-Api-App-Key` / `X-Api-Access-Key`；两者都填时优先使用）

握手返回 401/403 时，代理会返回 502 并提示检查密钥类型与资源是否开通。

## 配置

复制 `.env.example` 为 `.env`（放在运行程序的工作目录下）并填写凭证。已存在的环境变量优先于 `.env`。
未配置凭证或找不到 ffmpeg 时启动即报错退出。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `VOLC_API_KEY` / `VOLC_APP_KEY` + `VOLC_ACCESS_KEY` | – | 凭证，见上 |
| `VOLC_RESOURCE_ID` | `volc.seedasr.sauc.duration` | 小时版 / 并发版 |
| `VOLC_ENDPOINT` | `wss://…/api/v3/sauc/bigmodel_nostream` | 一般无需修改 |
| `VOLC_MODEL_NAME` | `bigmodel` | |
| `VOLC_ENABLE_DDC` | `true` | 语义顺滑（去掉语气词、重复等）；`enable_itn`、`enable_punc` 始终开启 |
| `VOLC_LANGUAGE` | 空 | 指定语种，如 `zh-CN`、`en-US`，作为 `audio.language` 发送 |
| `LISTEN_ADDR` | `127.0.0.1:8090` | |
| `PROXY_API_KEY` | 空 | 设置后客户端须带 `Authorization: Bearer <值>` |
| `FFMPEG_PATH` | 自动查找 | |
| `REQUEST_TIMEOUT` | `60s` | 基础超时：读取上传、等待空闲 + 转码各自受此限制 |
| `TIMEOUT_PER_AUDIO_SECOND` | `0.5` | 识别会话超时 = `REQUEST_TIMEOUT` + 音频秒数 × 此值（10 分钟音频为 6 分钟） |
| `MAX_AUDIO_SECONDS` | `600` | 单次最长 10 分钟，超出部分截断 |
| `MAX_UPLOAD_MB` | `128` | 请求体上限（10 分钟 48 kHz 立体声 WAV 约 115 MB）；上传流式写入临时文件，不占内存 |

**关于语言**：客户端传来的 OpenAI `language` 字段会被忽略（OpenAI 用 `zh`，火山用 `zh-CN` 等，代码不一致）。
默认由模型自动识别（中文含多种方言、英语）；如需指定，设置 `VOLC_LANGUAGE`。

## 运行

```sh
./bin/volc-stt-proxy
```

默认监听 `127.0.0.1:8090`，日志输出到 stderr。每个请求一行：文件扩展名、Content-Type、大小、
解码后时长、`model`、`response_format`、是否带 `language`/`prompt`（只记是否存在）、上游耗时、状态码、
火山 logid（握手响应头 `X-Tt-Logid`）、错误码、文本长度。不记录识别文本、音频、prompt/language 的内容和密钥。

## Spokenly 设置

选择 “OpenAI Compatible API”：

- URL：`http://127.0.0.1:8090`
- API Key：留空（如果设置了 `PROXY_API_KEY`，则填同样的值）
- Model：任意，例如 `volc-bigasr`

## curl 测试

```sh
say -v Tingting -o /tmp/hello.aiff "你好世界"
curl -s http://127.0.0.1:8090/v1/audio/transcriptions \
  -F file=@/tmp/hello.aiff -F model=volc-bigasr
# {"text":"你好世界。"}

curl -s http://127.0.0.1:8090/v1/models
curl -s http://127.0.0.1:8090/health
```

支持的 `response_format`：`json`（默认）、`text`、`verbose_json`；`srt`/`vtt` 不支持（返回 400）。
静音 / 没有说话时返回 200 和空文本。

限制与安全：

- 上传上限 `MAX_UPLOAD_MB`（默认 128 MB，超出返回 413）。
- 音频超过 `MAX_AUDIO_SECONDS`（默认 600 秒）的部分会被截断：仍返回前 600 秒的识别结果，
  并带响应头 `X-Audio-Truncated: true`，日志里记一条 WARN。
- 支持的文件扩展名：wav、mp3、m4a/mp4/mov、aac、ogg/opus/oga、webm、flac、aif/aiff、caf。
  扩展名缺失、未知或与内容不符时由 ffmpeg 自动识别，但只允许上述格式的解码器（`-format_whitelist`）
  且只能读本地文件（`-protocol_whitelist file`），因此 m3u8/concat 等播放列表无法引用其他文件。
- 最多同时处理 4 个识别请求，其余排队（受 `REQUEST_TIMEOUT` 约束）。

## 接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/audio/transcriptions`、`/audio/transcriptions` | 语音识别 |
| GET | `/v1/models`、`/models` | 返回模型 `volc-bigasr` |
| GET | `/health` | 返回 `ok`（不鉴权） |

## 开机自启

尚未配置 launchd 自启动，目前需要手动运行。
