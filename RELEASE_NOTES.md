# 更新说明

## v1.2.11

### 优化
- 遥测心跳改为 UTC 00:00 统一上报，解决不同时区面板统计窗口不一致的问题
- 活跃统计从 48 小时窗口改为精确 24 小时（UTC 当日），数值更准确
- 新装面板首次启动立即上报，后续更新或重启不再重复立即上报
- Nginx 模板新增 FastCGI 缓冲指令（buffer_size 128k、buffers 8 128k、busy_buffers_size 256k），解决大响应头被截断的问题
- WP Panel Optimizer 升级到 1.1.2，修复启用 open_basedir 时 www/裸域配置探测可能写入 PHP Warning 的问题
