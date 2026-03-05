# 知识库目录

在此目录下放置 `.md` 或 `.txt` 文件，Bot 启动时会自动加载作为 AI 回复的参考资料。

## 使用说明

1. 每个文件代表一个知识主题，文件名会作为章节标题
2. 支持 `.md`（Markdown）和 `.txt`（纯文本）格式
3. 单文件最大 20KB，总知识库最大 50KB
4. 修改后可通过 `/reload_kb` 命令热重载（需管理员权限）

## 示例

创建一个 `emby_faq.md` 文件：

```markdown
### 如何登录 Emby？

访问 https://your-emby-server.com，使用你的用户名和密码登录。

### 支持哪些客户端？

支持 Web、iOS、Android、Apple TV、Android TV、Roku 等主流平台。

### 忘记密码怎么办？

请联系管理员重置密码。
```
