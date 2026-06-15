# 更新说明

## v1.2.15

### 新功能

- 新增网站级 CDN Real IP 功能：每个网站可单独开启 CDN 真实 IP，并绑定对应 CDN 配置组。
- 新增 CDN 真实 IP 配置组管理：支持 Cloudflare 内置组、通用 CDN 兼容模式，以及阿里云 ESA、腾讯 EdgeOne、又拍云等自定义 CDN 配置组。
- Cloudflare 配置组自动使用官方 IP 段，无需手动维护回源 IP。
- 通用 CDN（兼容模式）可在不会填写 CDN 回源 IP 段时直接使用，默认信任 `X-Forwarded-For`。
- 自定义 CDN 配置组支持填写真实 IP Header 和可信 CDN 回源 IP/CIDR；填写回源 IP 后自动进入更严格的可信 CDN 模式。
- CDN 可信回源 IP 段会同时用于网站真实 IP 判断和 Web 防护白名单，避免误封 CDN 节点。
- 安全设置页面新增独立的“CDN 真实 IP 配置组”卡片，并展示真实 IP 获取与自动封禁的使用结论。

### 安全

- 修复 Cloudflare IP 缓存为空时可能退化为信任所有来源 Header 的风险；Cloudflare 官方 IP 段未缓存时会阻止启用并返回错误。
- Fail2ban 白名单只将 CDN 回源 IP 加入 Web 防护 jail，不再扩展到 SSH 防护。
- 站点启用或关闭 CDN Real IP 后会同步刷新 Fail2ban 配置，避免 CDN 节点被误封或旧白名单残留。
- Fail2ban 配置写入失败或 reload/start 失败时，会恢复旧配置文件，降低配置文件与面板状态不一致的风险。
- CDN 配置组更新或删除后，如果部分网站 Nginx 重建失败，会尝试回滚数据库、Fail2ban 和 Nginx 配置，并返回明确错误。
- 队列任务增加 panic 兜底，避免异常导致请求一直等待或队列 worker 中断。

### 优化

- Nginx 配置生成改为显式返回 CDN Real IP 解析错误，避免无效配置被静默忽略。
- 批量重建网站 Nginx 配置时会汇总部分失败并反馈给调用方。
- 后台异步刷新单站点 Nginx 配置时记录失败日志，便于排查配置未生效问题。
- CDN 配置组创建表单默认 Header 调整为 `X-Forwarded-For`，更符合国内 CDN 常见用法。
- 强化 CDN 配置组失败路径测试，覆盖 Fail2ban 失败、Nginx 重建失败、删除回滚、绑定恢复等场景。

### 使用说明

- Cloudflare 网站：在网站详情启用 CDN Real IP，并选择内置 Cloudflare 配置组。
- 不会填写 CDN 回源 IP 段的普通 CDN 网站：选择“通用 CDN（兼容模式）”。
- 阿里云 ESA、腾讯 EdgeOne、又拍云等国内 CDN：建议新增自定义配置组，Header 通常填写 `X-Forwarded-For`，并在“可信 CDN 回源 IP/CIDR”中填写厂商回源 IP 段。
- 启用 CDN Real IP 后，网站日志、异常访问分析、自动封禁等功能会尽量按真实访客 IP 工作；兼容模式允许少量 Header 伪造风险，严格模式更可靠。
