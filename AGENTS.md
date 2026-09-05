# 万模（Wanmo）项目归属

- 本仓库服务于 **万模 / 万模 API（Wanmo）**。
- GitHub：`https://github.com/lori-super/wanmo-sub2api`。
- 官方上游：`https://github.com/Wei-Shaw/sub2api`。
- 生产环境：OVH，SSH 别名 `worldcodes-ovh-test`（历史主机别名，不代表站点名称）。
- 入口：`https://api.llmroute.cc`、`https://direct-api.llmroute.cc`。
- 服务器源码：`/opt/sub2api-deploy/source`。
- `lori-super/worldcodes-sub2api` 是另一个站点的独立仓库，不得向其推送万模代码或使用其生产配置。
- 万模新增版本、镜像、发布记录使用 `wanmo` / `万模` 标识。历史代码中的共享命名不做无关批量替换。

升级和发布：保留定制补丁及用户修改；已发布 SQL 不可修改；采用备份、预检、候选验证、入口切换、连接排空的蓝绿流程。只运行与改动相关的检查。

中文回复。Git 提交信息包含中文「问题或需求描述」及「修复或实现思路」。
