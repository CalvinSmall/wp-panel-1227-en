## v1.2.4 改动记录

### 安全：新建网站默认开启异常访问日志
- 新建网站 `access_log_mode` 从 `off` 改为 `error_only`，默认只记录 4xx/5xx 异常请求
- 原因：Fail2ban 的 wppanel/wppanel-404 jail 依赖 access.log，`off` 模式下完全无防护
- 建站时立即创建空 access.log/error.log，避免 Fail2ban 因文件缺失而跳过监控
- 建站后自动 reload Fail2ban，确保新日志文件被 Fail2ban 感知
- 更新访问日志空状态提示文案，准确描述 error_only 行为

### 安全：Fail2ban 封禁即时写入数据库
- Fail2ban action 新增 `--record-fail2ban <ip> --ban-jail <name>` CLI 参数
- 每次 Fail2ban 触发封禁时立即写入 firewall_bans 表，不再等管理员查看页面时才同步
- 同一 IP 多次违规保留多条历史记录，支持渐进升级审计
- 通过 action `[name=...]` 参数区分 wppanel 和 wppanel-404 jail 来源

### 修复：Nginx HTTP/2 模板适配 Nginx 1.26
- Nginx 1.25.1+ 弃用 `listen 443 ssl http2` 语法
- 改为 `listen 443 ssl;` + `http2 on;`，兼容目标环境 Nginx 1.26
