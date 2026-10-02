# OpenAI-compatible transcription API for Doubao ASR

让 Spokenly 等听写工具用上豆包语音识别 2.0，填个地址就能用。

本地运行的 Go 小程序：对外提供 OpenAI 语音转文字接口（`POST /v1/audio/transcriptions`），
把上传的 WAV 转给豆包的**一句话识别**模式，返回整句结果。

## Quick Start

1. 在豆包语音控制台开通 **豆包流式语音识别模型 2.0**（点「试用」领 20 小时免费额度），
   在「API Key 管理」里拿到 API Key（火山方舟 / Ark 的 Key 不能用）。
2. 从 [Releases](../../releases) 下载对应平台的压缩包（Apple 芯片 Mac 选 `darwin_arm64`）并解压，
   在解压目录里配置并运行：

   ```sh
   cp .env.example .env   # 填入 VOLC_API_KEY=...
   ./volc-stt-proxy       # 监听 127.0.0.1:8090
   ```

   macOS 提示“无法验证开发者”时，先执行 `xattr -d com.apple.quarantine volc-stt-proxy`。
   也可以从源码运行：`make run`（需要 Go）。

3. Spokenly 里选 “OpenAI Compatible API”，URL 填 `http://127.0.0.1:8090`，API Key 留空，Model 随意。

## 可选配置

在 `.env` 中设置，完整列表和说明见 [`.env.example`](.env.example)。常用的：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `VOLC_RESOURCE_ID` | `volc.seedasr.sauc.duration` | 开通的是并发版时改为 `volc.seedasr.sauc.concurrent` |
| `VOLC_APP_KEY` + `VOLC_ACCESS_KEY` | – | 旧版控制台的 APP ID + Access Token，代替 `VOLC_API_KEY` |
| `VOLC_LANGUAGE` | 空（自动识别） | 指定语种，如 `zh-CN`、`en-US` |
| `VOLC_ENABLE_DDC` | `true` | 语义顺滑（去掉语气词、重复） |
| `LISTEN_ADDR` | `127.0.0.1:8090` | |
| `PROXY_API_KEY` | 空 | 设置后客户端须带 `Authorization: Bearer <值>` |

## 说明

- 音频需为 16 kHz 单声道 16-bit WAV（Spokenly 默认即是），不做转码。
- `response_format` 支持 `json`、`text`、`verbose_json`。
- 其他接口：`GET /v1/models`、`GET /health`。
- 发布新版本：`git tag v0.1.0 && git push origin v0.1.0`，GitHub Actions 会自动编译并上传到 Releases。
