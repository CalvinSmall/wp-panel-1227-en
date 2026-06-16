# 更新说明

## v1.2.17

- 新增“爬虫限速”安全设置：可按站点对常见 Bot UA 进行统一限速，用于缓解海量 IP 伪装 Facebook/Meta 等爬虫持续扫描造成的 WordPress 动态 404 压力。
- 爬虫限速默认关闭：新装和老用户升级后均不会自动启用，需管理员在“安全设置”页面手动开启。
- 搜索引擎豁免更严格：Googlebot/Bingbot 只有在来源 IP 命中官方 IP 段时才豁免新增 Bot 限速，假冒搜索引擎 UA 会进入限速桶。
- Nginx 限速拒绝状态码统一为 429：新增全局 `wppanel-limit-status.conf`，避免 IP 限速和 Bot 限速互相误删 `limit_req_status` 配置。
- 数据库升级新增 `bot_limit_*`、`googlebot_ips`、`bingbot_ips` 设置项；无 schema 变更，使用默认关闭策略保持现有站点访问行为不变。
