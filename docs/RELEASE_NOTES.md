Connector binaries for Windows, macOS and Linux (amd64 / arm64).

下载对应系统与架构的 ZIP，解压后运行 `agenttower.exe` 或 `./agenttower`。程序无需安装 Node.js。macOS/Windows 包未使用商业代码签名。

开源版需要自行部署 Server，启动时指定自己的地址：

```sh
./agenttower start --server https://your-relay.example.com --name "My computer"
```

The bundled default endpoint is an example. Configure your own HTTPS relay and connect the Client to the same relay. See README.md / README.en.md for adapter capabilities and setup.

Each ZIP includes the complete project license and dependency license notices. Verify downloads against SHA256SUMS.txt. Automated tests run on all three operating systems; the local CodeBuddy history test requires a separate real installation and is excluded from CI. Automated compilation does not verify every Agent integration or real device.
