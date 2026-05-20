# AI 人格包

这个目录用于存放可复用的 AI 人格包。人格只负责语气、称呼、表达风格和角色背景，不承载技能 SOP、管理员指令、事实知识或权限策略。

## 使用方式

1. 在本目录创建一个 `.md` 或 `.txt` 文件，例如 `ai_chan.md`。
2. 在 `config/config.json` 的全局配置中确认：

```json
"ai_persona_dir": "config/personas"
```

3. 在对应群组配置中填写文件名去掉后缀后的 ID：

```json
"ai_persona_id": "ai_chan"
```

## 推荐格式

```markdown
---
name: AI酱
description: 活泼但可靠的群聊人格
---

# 角色设定
你说话自然，回复简洁。

# 表达边界
不要编造事实；遇到权限、隐私、管理指令和工具调用时，必须服从系统规则。
```

也可以直接放纯文本人格内容。文件名会作为人格 ID，文件正文会被注入到 system prompt 的人格区域。
