# AgentTower Agent Connector

AgentTower 独立 Go 连接器，采用 [MIT + 标识保留补充条款](LICENSE) 自定义许可。允许商用、修改和分发，不要求公开业务源码或修改代码；必须保留版权、完整许可及服务标识中已有的 AgentTower 来源信息，可并列添加自己的产品名称。这不是标准 MIT 许可。

连接本机 AI 编程 Agent 与自行指定的中转，提供状态同步及受支持的会话、历史和审批能力。各 Agent 能力不同，以实际声明为准。

本仓库只包含 Connector 源码、嵌入式适配器和测试。手机端、服务端、品牌图标及生产部署配置不包含在内。

## 构建与验证

需要 Go 1.23 或以上版本。在仓库根目录运行：

```sh
go mod download
go build -trimpath -o agenttower ./connector
go test ./connector -skip TestCodebuddyDesktopSessionsFromRealLayout
```

Windows 可将输出名称改为 `agenttower.exe`。编译后的程序无需 Node.js。

`TestCodebuddyDesktopSessionsFromRealLayout` 需要真实的本机 CodeBuddy 安装和可发现的历史会话，应另行验收。

## 使用

```sh
./agenttower start --server https://relay.example.com --name "My computer"
./agenttower install opencode
```

必须将示例地址替换为自己的中转服务。开源发行的默认地址仅为示例，不连接生产服务。安装需要的适配器后重启对应 Agent，通过连接同一服务的兼容客户端确认绑定码。

安装入口包括 `opencode`、`opencode-cli`、`claude`、`cursor`、`cursor-cli`、`codex`、`codex-cli`、`codebuddy`、`codebuddy-cli`、`codebuddy-desktop`、`dsh`。Codex CLI notify 仅提供状态通知；DSH 插件需按安装输出以 `--patch` 加载。安装命令会修改对应 Agent 的用户级配置，请查看命令输出。

默认仅监听 `127.0.0.1:17654`，主动连接中转。配置默认保存在 `~/.agenttower/device.json`，包含设备凭证，不得公开。没有端到端加密，中转可读取转发内容。不要提交真实会话、凭证或生产信息。

依赖遵循各自许可证，参见 [THIRD_PARTY.md](THIRD_PARTY.md)。
