# FlowMusic2API

<div align="center">

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25%2B-blue.svg)](https://go.dev/)
[![Docker](https://img.shields.io/badge/docker-supported-blue.svg)](https://www.docker.com/)
[![Release](https://img.shields.io/github/v/release/genz27/FlowMusic2API)](https://github.com/genz27/FlowMusic2API/releases)

**FlowMusic 的 OpenAI 兼容 API 包装服务 — 音乐生成、账号池管理、Bearer 自动刷新**

</div>

---

## 核心特性

- 🎵 **AI 音乐生成** — 支持 lyria / lyria-fast / lyria-pro / lyria-pro-fast 模型
- 🔄 **Bearer 自动刷新** — 通过 refresh token / provider token / Cookie 协议自动换新，临近过期即时刷新
- 👥 **账号池管理** — 多 Token 负载均衡，连续错误自动禁用，智能选择低错误账号
- 📊 **余额与套餐展示** — 实时查询并显示账号 Credits 和订阅套餐
- 🚪 **游客试用页** — 无需 API Key 即可体验音乐生成，支持 IP 与全局每日限制
- 🌐 **代理支持** — HTTP / SOCKS5 代理，请求代理与媒体缓存代理独立配置
- 🗄️ **缓存支持** — 本地磁盘 / S3 / Cloudflare R2 缓存，可配置过期时间
- 🖥️ **Web 管理面板** — 完整的 Token、系统配置、请求日志、模型测试界面
- 🐳 **Docker 支持** — 一键部署，支持 SQLite / PostgreSQL

## 快速开始

### 前置要求

- Go 1.25+（本地开发）或 Docker（容器部署）
- 有效的 FlowMusic 账号（refresh token 或 Bearer token）

### 方式一：Docker 部署（推荐）

```bash
# 克隆项目
git clone https://github.com/genz27/FlowMusic2API.git
cd FlowMusic2API

# 复制环境变量并修改
cp .env.example .env

# 使用 SQLite 启动（无需 PostgreSQL）
docker compose -f docker-compose.sqlite.yml up -d

# 或使用 PostgreSQL
docker compose up -d
```

### 方式二：本地部署

```bash
# 克隆项目
git clone https://github.com/genz27/FlowMusic2API.git
cd FlowMusic2API

# 安装依赖并启动
go mod tidy
go run ./cmd/server
```

### 方式三：Docker 单容器

```bash
# 拉取镜像
docker pull ghcr.io/genz27/flowmusic2api:latest

# 运行（SQLite 模式）
docker run -d \
  --name flowmusic2api \
  -p 8000:8000 \
  -v ./data:/app/data \
  -v ./tmp:/app/tmp \
  -e FLOWMUSIC_ADMIN_PASSWORD=your-password \
  -e FLOWMUSIC_DEFAULT_API_KEY=your-api-key \
  -e FLOWMUSIC_ADMIN_JWT_SECRET=your-jwt-secret \
  ghcr.io/genz27/flowmusic2api:latest
```

### 首次访问

服务启动后，访问管理后台：**http://localhost:8000/manage**

| 项目 | 默认值 |
|------|--------|
| 管理员用户 | `admin` |
| 管理员密码 | `admin`（请立即修改） |
| API Key | `fm123456`（请立即修改） |

## 管理面板

- **Token 管理** — 添加/编辑/导入/导出 FlowMusic 账号，支持 refresh token、Bearer 直连、Cookie 协议三种模式
- **系统配置** — 安全配置（密码/API Key）、代理配置、生成超时、Token 轮询策略、缓存配置、游客试用
- **请求日志** — 实时查看运行中任务和完成日志，自动刷新
- **模型测试** — 选择模型、输入提示词，单次或并发测试

### 游客试用页

开启后访问 `/` 直接体验音乐生成，无需 API Key，支持：

- 按 IP 每日限制（默认 3 次/天）
- 全局每日限制（可选）
- 流式生成实时展示

## API 使用

### OpenAI 兼容接口

所有请求需携带 API Key：

```bash
curl -X POST "http://localhost:8000/v1/chat/completions" \
  -H "Authorization: Bearer fm123456" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "lyria",
    "messages": [
      {"role": "user", "content": "一首中文可爱猫猫电子流行歌曲，女声，轻快，副歌抓耳"}
    ],
    "stream": true
  }'
```

### 流式响应格式

服务返回标准 SSE 流，每个 `data:` 行包含推理过程和生成内容：

```
data: {"choices":[{"delta":{"reasoning_content":"正在分析提示词..."}}]}

data: {"choices":[{"delta":{"content":"## 生成结果"}}]}

data: [DONE]
```

### 支持的模型

| 模型 ID | 说明 |
|---------|------|
| `lyria` | 标准音乐生成 |
| `lyria-fast` | 快速音乐生成 |
| `lyria-pro` | 高质量音乐生成，更深入的作曲推理 |
| `lyria-pro-fast` | 高质量快速音乐生成 |

旧版兼容别名：`flowmusic`、`flowmusic-standard`、`flowmusic-producer-standard` 自动映射到 `lyria`。

### 其他接口

- `GET /v1/models` — 模型列表
- `GET /v1/models/{model}` — 模型详情
- `GET /v1/models/aliases` — 模型别名列表
- `POST /v1/audio/generations` — 音乐生成（兼容）
- `POST /v1/audio/speech` — OpenAI SDK 兼容别名
- `POST /v1/audio/results` — 查询生成结果
- `POST /v1/music/generations` — 音乐生成
- `POST /v1/music/results` — 查询生成结果
- `GET /health` — 健康检查

## 账号管理

### 添加方式

管理页支持三种协议添加账号：

| 协议 | 所需凭证 | 说明 |
|------|---------|------|
| `refresh_token` | Supabase refresh token + `SUPABASE_ANON_KEY` | 自动刷新 Bearer，推荐 |
| `bearer` | FlowMusic Bearer 直连 | 直接使用 Bearer，无自动刷新 |
| `protocol` | FlowMusic Cookie | Cookie 兼容兜底 |

### Token 刷新流程

1. Bearer 临近过期时自动通过 refresh token 换新
2. 无可用的 refresh token 时，尝试用 provider token 换取
3. 上述均失败时，使用 Cookie 协议通过 `/__api/auth/session` 获取最新 Bearer
4. 所有刷新方式都失败时跳过该账号，尝试下一个可用账号

### 账号选择策略

- **智能选择（默认）** — 优先使用连续错误数和总错误数更低的可用账号
- **顺序轮询** — 按 `last_used_at` / `id` 顺序依次使用

## 缓存配置

支持三种缓存模式：

| 模式 | 存储位置 | 适用场景 |
|------|---------|---------|
| `local` | 服务器本地磁盘 | 单机部署 |
| `s3` | S3 兼容对象存储 | 需要 CDN 分发 |
| `r2` | Cloudflare R2 | 全球加速，免公网访问前缀 |

在管理页 `/manage` 的缓存配置中设置，或通过环境变量预置。

## Docker 构建

```bash
# 构建镜像
docker build -t flowmusic2api .

# 查看可用的 compose 文件
docker compose up -d                                    # PostgreSQL
docker compose -f docker-compose.sqlite.yml up -d       # SQLite
```

## 开发

```bash
# 运行测试
go test ./...

# 代码检查
go vet ./...

# 构建
go build -o flowmusic2api.exe ./cmd/server

# 离线进程级 smoke 测试
go test ./cmd/server -run TestServerProcessSQLiteSmoke -count=1 -v -timeout 90s
```

### 环境变量说明

关键环境变量：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `FLOWMUSIC_HTTP_PORT` | `8000` | 监听端口 |
| `FLOWMUSIC_DB_DRIVER` | `sqlite` | 数据库驱动（sqlite/postgres） |
| `FLOWMUSIC_ADMIN_USER` | `admin` | 管理员用户名 |
| `FLOWMUSIC_ADMIN_PASSWORD` | `admin` | 管理员密码 |
| `FLOWMUSIC_DEFAULT_API_KEY` | `fm123456` | 默认 API Key |
| `FLOWMUSIC_ADMIN_JWT_SECRET` | `flowmusic2api-dev-secret` | JWT 签名密钥 |
| `FLOWMUSIC_SUPABASE_ANON_KEY` | — | Supabase 匿名密钥（Bearer 刷新必需） |
| `FLOWMUSIC_GUEST_DAILY_LIMIT` | `3` | 访客每 IP 每日限制 |
| `FLOWMUSIC_GUEST_GLOBAL_DAILY_LIMIT` | `0` | 访客全局每日限制（0 不限制） |

完整变量列表见 `.env.example`。

## 许可证

本项目采用 MIT 许可证。

## 致谢

- - 感谢所有贡献者和使用者的支持

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=genz27/FlowMusic2API&type=date)](https://www.star-history.com/#genz27/FlowMusic2API&type=date)
