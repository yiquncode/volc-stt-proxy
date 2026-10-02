# volc-stt-proxy

让 Spokenly 等只支持 OpenAI 接口的听写工具用上火山引擎「豆包流式语音识别模型 2.0」。

这是一个本地 HTTP 代理：对外提供 OpenAI 语音转文字接口（`POST /v1/audio/transcriptions`），
把上传的 WAV 原样通过 WebSocket 发给豆包的**一句话识别**模式，返回整句结果。

依赖：Go（唯一第三方库 `github.com/coder/websocket`）。

## 火山引擎控制台准备

1. 在豆包语音控制台开通 **豆包流式语音识别模型 2.0**，二选一：
   - 小时版 → 资源 ID `volc.seedasr.sauc.duration`（默认值）
   - 并发版 → 资源 ID `volc.seedasr.sauc.concurrent`（需设置 `VOLC_RESOURCE_ID`）

   20 小时免费额度需要在控制台点击「试用」领取。
2. 获取密钥，二选一（火山方舟 / Ark 的 API Key **不能用**）：
   - 新版控制台「API Key 管理」里的 API Key → `VOLC_API_KEY`（请求头 `X-Api-Key`）
   - 旧版控制台的 APP ID + Access Token → `VOLC_APP_KEY` + `VOLC_ACCESS_KEY`
     （请求头 `X-Api-App-Key` / `X-Api-Access-Key`；两者都填时优先使用）

握手返回 401/403 时，代理会返回 502 并提示检查密钥类型与资源是否开通。

## 构建

```sh
go build -o bin/volc-stt-proxy .   # 或 make build
go test ./...                      # 或 make test
```

## 配置

复制 `.env.example` 为 `.env`（放在运行程序的工作目录下）并填写凭证。已存在的环境变量优先于 `.env`。
未配置凭证时启动即报错退出。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `VOLC_API_KEY` / `VOLC_APP_KEY` + `VOLC_ACCESS_KEY` | – | 凭证，见上 |
| `VOLC_RESOURCE_ID` | `volc.seedasr.sauc.duration` | 小时版 / 并发版 |
| `VOLC_ENDPOINT` | `wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream` | 一般无需修改 |
| `VOLC_MODEL_NAME` | `bigmodel` | |
| `VOLC_ENABLE_DDC` | `true` | 语义顺滑（去掉语气词、重复等）；`enable_itn`、`enable_punc` 始终开启 |
| `VOLC_LANGUAGE` | 空 | 语种，如 `zh-CN`、`en-US`；留空由模型自动识别（中文含方言、英语） |
| `LISTEN_ADDR` | `127.0.0.1:8090` | |
| `PROXY_API_KEY` | 空 | 设置后客户端须带 `Authorization: Bearer <值>` |
| `REQUEST_TIMEOUT` | `60s` | 基础超时：读取上传、排队等待（最多同时处理 4 个识别请求）各自受此限制 |
| `TIMEOUT_PER_AUDIO_SECOND` | `0.5` | 识别会话超时 = `REQUEST_TIMEOUT` + 估算音频秒数（文件字节数 / 32000）× 此值 |
| `MAX_UPLOAD_MB` | `24` | 上传上限，约 12 分钟 16 kHz 单声道 WAV；超出返回 413 |

客户端传来的 OpenAI `language` 字段会被忽略（OpenAI 用 `zh`，火山用 `zh-CN`），需要指定语种时用 `VOLC_LANGUAGE`。

## 运行

```sh
./bin/volc-stt-proxy
```

日志输出到 stderr，每个请求记一行元数据（大小、音频时长、上游耗时、状态码、火山 logid 等），
不记录识别文本、音频、prompt/language 内容和密钥。

## Spokenly 设置

选择 “OpenAI Compatible API”：

- URL：`http://127.0.0.1:8090`
- API Key：留空（如果设置了 `PROXY_API_KEY`，则填同样的值）
- Model：任意，例如 `volc-bigasr`

## 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/audio/transcriptions`、`/audio/transcriptions` | 语音识别 |
| GET | `/v1/models`、`/models` | 返回模型 `volc-bigasr` |
| GET | `/health` | 返回 `ok`（不鉴权） |

- 音频：上传内容不做转换，一律以 `audio.format: "wav"` 发给火山，应为 16 kHz、单声道、16-bit WAV
  （Spokenly 发送的正是这种格式）；其他格式火山会报错，代理返回 502。
- `response_format`：`json`（默认）、`text`、`verbose_json`；`srt`/`vtt` 返回 400。
- 静音或没有说话时返回 200 和空文本。

## curl 测试

```sh
say -v Tingting -o /tmp/hello.aiff "你好世界"
afconvert -f WAVE -d LEI16@16000 -c 1 /tmp/hello.aiff /tmp/hello.wav   # 转成 16 kHz 单声道 WAV
curl -s http://127.0.0.1:8090/v1/audio/transcriptions \
  -F file=@/tmp/hello.wav -F model=volc-bigasr
# {"text":"你好世界。"}

curl -s http://127.0.0.1:8090/v1/models
curl -s http://127.0.0.1:8090/health
```
