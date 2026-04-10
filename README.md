# Agent Proxy - Claude Code Telegram Bot

通过 Telegram 与本地 Claude Code CLI 进行双向对话的代理服务。

## 功能

- 在 Telegram 中与 Claude Code 实时对话
- 支持所有 Claude Code 斜杠命令（`/compact`, `/model`, `/cost` 等）
- 流式输出，实时更新消息
- 会话管理，支持上下文连续对话
- 用户鉴权，仅允许指定用户使用
- 支持切换工作目录

## 前置要求

1. 已安装 [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) 并可通过命令行 `claude` 使用
2. 一个 Telegram Bot Token（通过 [@BotFather](https://t.me/BotFather) 创建）
3. Go 1.22+ 运行环境

## 安装

```bash
# 克隆项目
cd /path/to/agent-proxy

# 安装依赖
go mod tidy

# 构建
go build -o agent-proxy .
# 或使用构建脚本
chmod +x scripts/build.sh
./scripts/build.sh
```

## 配置

### 方式一：配置文件

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml 填入你的配置
```

### 方式二：环境变量

```bash
export TELEGRAM_BOT_TOKEN="your-bot-token"
export ALLOWED_USERS="123456789,987654321"
export DEFAULT_WORK_DIR="/Users/you/projects"
export CLAUDE_PATH="claude"           # 可选
export CLAUDE_TIMEOUT="300"           # 可选
```

环境变量优先级高于配置文件。

### 获取 Telegram User ID

1. 在 Telegram 中搜索 `@userinfobot`，发送任意消息获取你的 User ID
2. 或者先用一个空的 `allowed_users` 启动 Bot，然后发送 `/id` 命令查看

## 运行

```bash
# 使用配置文件
./agent-proxy -config config.yaml

# 或使用环境变量
TELEGRAM_BOT_TOKEN="xxx" ALLOWED_USERS="123" ./agent-proxy
```

## 使用

### Bot 专有命令

| 命令 | 说明 |
|------|------|
| `/start` | 显示欢迎信息 |
| `/help` | 显示帮助信息 |
| `/newsession` | 创建新会话（重置对话历史） |
| `/setdir <path>` | 设置 Claude Code 工作目录 |
| `/status` | 显示当前会话状态 |
| `/id` | 显示你的 Telegram User ID |

### Claude Code 命令

直接在 Telegram 中输入 Claude Code 的斜杠命令即可，会被直接转发给 CLI：

| 命令 | 说明 |
|------|------|
| `/compact` | 切换紧凑模式 |
| `/model` | 查看/切换模型 |
| `/cost` | 查看 token 用量 |
| `/doctor` | 运行诊断 |
| `/init` | 初始化 CLAUDE.md |
| `/memory` | 编辑 CLAUDE.md |
| `/clear` | 清除会话（同时重置本地 session） |

### 普通对话

直接发送任何文字消息，会作为对话内容发送给 Claude Code。支持连续对话上下文。

## 架构

```
Telegram User ←→ Telegram API ←→ Agent Proxy Bot ←→ Claude Code CLI
                                      ↑
                              Long Polling + 
                              Session Management +
                              User Authentication
```

## 许可证

MIT
