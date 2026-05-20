# AI 人格包

这个目录用于存放可复用的 AI 人格包。人格只负责语气、称呼、表达风格和角色背景，不承载技能 SOP、管理员指令、事实知识或权限策略。

## 使用方式

1. 在本目录创建一个单文件人格，或一个三段式人格目录。
2. 在 `config/config.json` 的全局配置中确认：

```json
"ai_persona_dir": "config/personas"
```

3. 在对应群组配置中填写人格 ID：

```json
"ai_persona_id": "ai_chan"
```

## 单文件人格

单文件人格适合较短设定。文件名去掉后缀就是人格 ID：

```text
config/personas/ai_chan.md
```

对应配置：

```json
"ai_persona_id": "ai_chan"
```

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

## 三段式目录人格

目录人格适合从外部人格包迁移。目录名就是人格 ID，加载器会按固定顺序合并：

```text
config/personas/aeloria/
├── IDENTITY.md
├── USER.md
└── SOUL.md
```

对应配置：

```json
"ai_persona_id": "aeloria"
```

加载顺序固定为：

1. `IDENTITY.md`
2. `USER.md`
3. `SOUL.md`

`IDENTITY.md` 可以带 front matter，用于设置人格名和简介：

```markdown
---
name: Aeloria
description: 明亮、好奇、带一点仪式感的群聊人格
---

这里放身份设定。
```

`USER.md` 放它如何看待用户、怎么称呼用户、互动关系边界。

`SOUL.md` 放深层性格、价值观、长期风格设定。
