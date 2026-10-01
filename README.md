# 微信人物画像助手

一个 Windows 桌面常驻小工具，配合微信 PC 版使用：

1. **人物画像**：从程序启用那一刻开始，持续积累某个微信联系人的单聊消息，达到设定条数后自动调用大模型生成并更新这个人的画像（性格、沟通风格、兴趣、情绪模式、关系等）。
2. **实时意图分析**：复制一段聊天记录后点击悬浮按钮，程序结合已积累的画像和近期对话，分析对方最新消息的表面意思、潜在意图、情绪、潜台词，并给出建议回复。

- 只支持**单聊**，不处理群聊
- **不导入历史记录**，只积累启用程序之后复制的消息
- 数据保存在本地 SQLite（`wechat_profile.db`），聊天内容仅在你点击"识别"时发送给配置的云端大模型

## 功能特性

- **人物画像**：自动分析联系人的性格、沟通风格、兴趣爱好、情绪模式、典型意图、重要事实
- **意图分析**：复制聊天记录后一键分析对方最新意图，给出建议回复
- **手动补充画像**：支持手动输入对方生日、性别、星座、职业等信息，AI 合并到画像
- **联系人备注**：给联系人添加个人备注，方便区分，不影响识别匹配
- **昵称合并**：对方修改微信昵称后，手动关联新旧昵称，历史数据自动归位
- **画像历史**：每次画像更新都会保留历史版本，可随时查看和对比
- **本地存储**：所有数据保存在本地 SQLite，不上传任何服务器（仅识别时发送给配置的 LLM）

## 下载与安装

### 方式一：直接下载（推荐）

去 [Releases](https://github.com/caodabao99/wechat-profile/releases) 页面下载最新版 [wechat-profile-v1.0.zip](https://github.com/caodabao99/wechat-profile/releases/download/v1.0/wechat-profile-v1.0.zip)，解压到任意目录。

### 方式二：自行编译

需要 Go 1.21+，Windows 环境（或交叉编译）：

```bat
:: 1. 拉取依赖
go mod tidy

:: 2. （可选但推荐）嵌入现代控件样式清单，需要先安装 rsrc：go install github.com/akavel/rsrc@latest
rsrc -manifest app.manifest -o rsrc.syso

:: 3. 编译，-H windowsgui 隐藏黑色控制台窗口
go build -ldflags="-H windowsgui" -o wechat-profile.exe
```

## 配置说明

首次运行会在 exe 同目录自动生成 `config.json`：

```json
{
  "myName": "你的微信昵称",
  "llm": {
    "apiKey": "sk-xxx",
    "baseURL": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "disableThinking": false
  },
  "profile": {
    "coldStartCount": 20,
    "updateInterval": 10
  },
  "context": {
    "recentMessageCount": 30
  }
}
```

| 字段 | 含义 |
|---|---|
| `myName` | **你自己的微信昵称**，用于区分复制记录里哪条是你发的，务必填对 |
| `llm.apiKey` | 大模型 API Key，默认 DeepSeek，也可换成任意 OpenAI 兼容接口 |
| `llm.baseURL` | 接口地址，不带末尾斜杠 |
| `llm.model` | 模型名 |
| `llm.disableThinking` | 是否关闭模型的推理思考模式（如 deepseek-v4、qwen3 系列默认开思考，填 `true`） |
| `profile.coldStartCount` | 累计多少条**对方**消息后首次生成画像，默认 20 |
| `profile.updateInterval` | 之后每新增多少条对方消息更新一次画像，默认 10 |
| `context.recentMessageCount` | 意图分析时携带的近期对话条数，默认 30 |

## 使用方法

1. **启动程序**：双击 `wechat-profile.exe`，屏幕右下角出现「识 别」悬浮按钮（可左键拖动、右键菜单退出）。
2. **填写配置**：首次运行会提示填写配置，编辑同目录 `config.json`，填入 `myName` 和 `llm.apiKey` 后重启。
3. **复制聊天记录**：在微信 PC 版中打开某个**单聊**窗口，用鼠标选中一批聊天记录，**Ctrl+C** 复制（微信复制结果包含「昵称 + 时间」消息头）。
4. **点击识别**：点击悬浮窗的「识 别」按钮。
5. **查看结果**：弹出结果窗口查看意图分析和建议回复，可点「查看完整画像」进入画像管理窗口。
6. **画像管理**：
   - 画像窗左侧是联系人列表，右侧分「画像」「消息」「历史」「统计」四个页签
   - 达到 `coldStartCount` 条后自动生成画像，之后每隔 `updateInterval` 条自动更新
   - 历史页可回看每次更新的画像版本

### 常用操作

- **改备注**：画像窗左下角「改备注…」，给联系人设置个人备注（如「同事小王」），方便区分，不影响识别匹配
- **补充画像**：画像窗左下角「补充画像…」，手动输入对方生日、性别、星座、职业等信息，AI 合并到画像
- **关联昵称**：画像窗左下角「关联昵称…」，当对方修改微信昵称时，把新昵称（源）合并到旧联系人（目标），历史数据自动归位

> 点击识别按钮前不要复制其他内容；同一段内容重复点击会提示「无新内容」，重新复制即可。

## 支持的聊天记录类型

目前只支持**文字消息**的解析。图片、表情、文件、语音等非文字消息，从微信复制到剪贴板时，微信会转成文字占位符（如 `[图片]`、`[表情]`、`[文件]`、`[语音]`），这些占位符会被当作普通文字消息正常识别和存储，但程序不会解析图片/语音的实际内容。

## 模型适配

程序支持任何 **OpenAI 兼容接口** 的大模型。只需修改 `config.json` 中的三项：

- `apiKey`：你的 API Key
- `baseURL`：模型接口地址（如 `https://api.deepseek.com`、`https://dashscope.aliyuncs.com/compatible-mode/v1`、`https://api.moonshot.cn` 等）
- `model`：模型名称（如 `deepseek-chat`、`qwen-plus`、`moonshot-v1-8k` 等）

如果对方模型默认开启推理思考模式（如 deepseek-v4、qwen3 系列），把 `disableThinking` 设为 `true`；普通模型填 `false` 或删掉该字段。

## 数据与隐私

- 全部消息、画像存放在程序目录的 `wechat_profile.db`（SQLite），不会被上传到任何服务器。
- 但**每次点击「识别」，近期对话文本会发送给 `config.json` 中配置的大模型接口**（默认 DeepSeek 云端），请知悉并自行评估隐私风险。
- 删除 `wechat_profile.db` 即可清空所有积累数据。

## 注意事项

1. 本工具仅通过系统剪贴板读取你主动复制的内容，不读取、不 hook 微信进程数据。
2. 仅供个人学习研究使用；分析他人聊天应事先征得对方同意，遵守相关法律法规与社交平台规则。
3. 个别安全软件可能拦截剪贴板读取或无窗口程序，如遇异常请添加信任。
4. 微信版本更新若导致复制格式变化，解析器内置降级逻辑（整段文本按对方消息处理），但消息时间与发言人识别可能失效，需按新版格式调整 `parser.go` 中的正则。

## 开源协议

[MIT](LICENSE)
