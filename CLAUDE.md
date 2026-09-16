# Project Guidelines

## Fork conventions (this fork overrides upstream where they differ)

This is a hard fork of `xpzouying/xiaohongshu-mcp`. The sections below in Chinese are upstream's and are kept for reference; where this section disagrees with them, this section wins.

- **Comments in English for code this fork writes.** Leave upstream's Chinese comments exactly as they are — translating them is churn and wrecks the diff against upstream. Chinese *string literals* (selectors, UI labels like `仅自己可见`, `暂存离开`, log text) are data the site requires, never translate those. This overrides upstream's `使用中文注释` below.
- **Commit to `main` directly.** No feature branches, no PR flow. Push only when the owner asks. This overrides upstream's branch-and-PR rules below.
- **Never `git add -A`, `git add .`, or `git commit -a`.** Stage explicit paths; prefer `git commit -- <paths>`. Concurrent agents have swallowed each other's changesets three times; pathspec-scoped commits are the only safe form here.
- **Never commit `cookies.json` or `profile/`.** Both are gitignored and hold a live logged-in session. `profile/` was untracked-but-unignored at one point, one `git add -A` away from publishing an account session to a public repo.
- **Read `#16` before adding any Chrome flag or CDP call.** It records what is broken, dangerous or silently ineffective on the bundled browser build — including a flag that crashes it outright and an input field that triggers a 15,000-event-per-second keystroke storm.
- **Before touching anything LLM-, fingerprint- or site-shaped, check what has already been measured.** A great deal of this codebase's behaviour was established empirically against a live account, and the issues carry the evidence. Re-deriving it costs real account risk.

### Environment

- `go env GOARCH` may be `amd64` on an arm64 host here; run go commands with `GOARCH=arm64` or the bundled browser refuses to launch.
- **Do not run bare `go mod tidy`.** pgx is pinned at v5.7.6 deliberately: newer versions raise the module's Go directive past what the Dockerfile builds on, and tidy raises it on its own because of a transitive test dependency.
- `go test ./...` must stay green and hermetic — no database, no browser, no network.
- `go test -tags integration ./xiaohongshu/` currently has two known failures (`TestSearch`, `TestSearchWithFilters`) that drive the live search page.

### Deployments

The tool supports two: `xiaohongshu.com` and `rednote.com`. Accounts registered outside China live on rednote and **cannot log into xiaohongshu.com at all**. Selection is `-site` / `XHS_SITE` / the session file's `site` field / a cookie-domain sniff, and a mismatch is fatal at startup. Do not reintroduce a hardcoded host.

##  本地开发规范

- 要求每次修改完后,需要帮我格式化 Go 源码文件.
- 测试过程中产生的脚本和build中间文件,如果没有必要,则删除.
- 所有的feature变更,都需要使用分支进行开发.
- 在我未同意之前, 你不能推送到远程.
- 我需要: 1.本地 review; 2.远程 PR review.
- 不要过度设计, 保持代码的简洁和易读.
- 使用中文注释，一定要简洁明了.专业名词可以用英文.

## 发版规范

- 发版=打语义化 tag `vX.Y.Z` 推上去（触发 Release 与 Docker 镜像），破坏性变更进 major；main 每次推送自动生成的日期 tag `vYYYY.MM.DD.HHMM-sha` 不是发版，别跟它混。

## PR Review 重点

- 重点：PR 代码中如果出现大量的 JS 注入的行为，要检查一下是否是必须的，如果可以用 Go 的 go-rod 替代的话，则直接评论需要用 go-rod 行为替代。
