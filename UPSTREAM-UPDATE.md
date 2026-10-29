# 跟随上游更新

本仓库是 ithtelab/workbuddy-manager 发布包中 `upstream/`（workbuddy2api 源码）的
fork，只加了一处改动：`POST /v1/search/messages`。

上游版本记在 `UPSTREAM-VERSION`。同步步骤：

1. 查上游最新版本：
   `gh api repos/ithtelab/workbuddy-manager/releases/latest --jq .tag_name`
2. 与 `UPSTREAM-VERSION` 比较，相同则无需更新。
3. 下载对应发布包并解出 `upstream/`：
   `gh release download <tag> -R ithtelab/workbuddy-manager -p "*.tar.gz" -O /tmp/rel.tgz`
   `tar xzf /tmp/rel.tgz -C /tmp && ls /tmp/workbuddy-manager-*/upstream`
4. 用新源码覆盖本仓库，**保留**这四处改动：
   - `internal/upstream/search.go`（新增，上游调用）
   - `internal/server/search.go`（新增，HTTP 处理器）
   - `internal/server/handler.go` 里的路由注册：
     `h.mux.HandleFunc("POST "+searchMessagesPath, h.withAuth(h.searchMessages))`
   - `.gitignore` 末尾的 `!internal/prompt/defaultprompt.md`（否则 //go:embed 构建失败）
   以及 `.github/workflows/build.yml`、`UPSTREAM-VERSION`、本文件。
5. 更新 `UPSTREAM-VERSION`，提交推送 → Actions 自动构建镜像。
6. 到 ZhiFlo 仓库把 `workloads/apps/production/workbuddy.yaml` 两处镜像 digest 换成
   新构建的 digest，提交等 Flux 收敛，验证 `/healthz` 与 `/v1/search/messages`。
