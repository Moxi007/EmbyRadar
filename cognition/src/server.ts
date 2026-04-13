import Database from "better-sqlite3";
import { randomUUID } from "node:crypto";
import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

type LLMConfig = {
  base_url: string;
  api_key: string;
  model: string;
  max_tokens: number;
  temperature: number;
};

type CognitionMessage = {
  role: string;
  text: string;
};

type RespondRequest = {
  llm: LLMConfig;
  engine_base_url: string;
  static_prompt: string;
  persona_seed: string;
  quiet_hours?: number[];
  proactive_cooldown_minutes?: number;
  relationship_decay_days?: number;
  chat_id: number;
  user_id?: number;
  user_name?: string;
  verified_role?: string;
  is_private: boolean;
  allow_sensitive: boolean;
  message_id?: number;
  reply_to_message_id?: number;
  text: string;
  recent_context?: CognitionMessage[];
  skill_summaries?: string[];
  job_summaries?: string[];
  knowledge_summary?: string;
};

type HostContext = {
  chat_id: number;
  sender_id?: number;
  message_id?: number;
  reply_to_message_id?: number;
  is_private: boolean;
  allow_sensitive: boolean;
};

type EventPayload = {
  type: string;
  chat_id: number;
  user_id?: number;
  user_name?: string;
  message_id?: number;
  text?: string;
  timestamp: string;
  metadata?: Record<string, unknown>;
};

const port = Number(process.env.PORT ?? "3400");
const dbPath = resolve(process.env.COGNITION_DB_PATH ?? "config/cognition.db");
mkdirSync(dirname(dbPath), { recursive: true });

const db = new Database(dbPath);
db.pragma("journal_mode = WAL");
db.exec(`
CREATE TABLE IF NOT EXISTS event_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_type TEXT NOT NULL,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  user_name TEXT,
  message_id INTEGER,
  text TEXT,
  timestamp_ms INTEGER NOT NULL,
  metadata_json TEXT
);
CREATE TABLE IF NOT EXISTS diary_entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  content TEXT NOT NULL,
  summary TEXT,
  created_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS beliefs (
  chat_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  user_tier TEXT DEFAULT 'regular',
  familiarity REAL DEFAULT 0.1,
  response_preference TEXT DEFAULT 'concise',
  interest_tags TEXT DEFAULT '',
  emby_preference TEXT DEFAULT '',
  trust_level REAL DEFAULT 0.3,
  last_emotional_signal TEXT DEFAULT 'neutral',
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY (chat_id, user_id)
);
CREATE TABLE IF NOT EXISTS relationships (
  chat_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  warmth REAL DEFAULT 0.2,
  reciprocity REAL DEFAULT 0.2,
  responsiveness REAL DEFAULT 0.2,
  familiarity REAL DEFAULT 0.2,
  last_interaction_ms INTEGER NOT NULL,
  last_proactive_ms INTEGER DEFAULT 0,
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY (chat_id, user_id)
);
CREATE TABLE IF NOT EXISTS narrative_threads (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  next_due_ms INTEGER DEFAULT 0,
  last_event_ms INTEGER NOT NULL,
  metadata_json TEXT DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS episodes (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  origin TEXT NOT NULL,
  status TEXT NOT NULL,
  voice TEXT NOT NULL,
  pressures_json TEXT NOT NULL,
  tool_call_count INTEGER DEFAULT 0,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS scheduled_actions (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  user_id INTEGER,
  kind TEXT NOT NULL,
  command TEXT NOT NULL,
  status TEXT NOT NULL,
  due_ms INTEGER NOT NULL,
  last_error TEXT DEFAULT '',
  metadata_json TEXT DEFAULT '{}',
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS persona_state (
  chat_id INTEGER PRIMARY KEY,
  diligence REAL DEFAULT 0.25,
  curiosity REAL DEFAULT 0.25,
  sociability REAL DEFAULT 0.25,
  caution REAL DEFAULT 0.25,
  updated_at_ms INTEGER NOT NULL
);
`);

const upsertBeliefStmt = db.prepare(`
INSERT INTO beliefs (
  chat_id, user_id, user_tier, familiarity, response_preference, interest_tags,
  emby_preference, trust_level, last_emotional_signal, updated_at_ms
) VALUES (
  @chat_id, @user_id, @user_tier, @familiarity, @response_preference, @interest_tags,
  @emby_preference, @trust_level, @last_emotional_signal, @updated_at_ms
)
ON CONFLICT(chat_id, user_id) DO UPDATE SET
  user_tier=excluded.user_tier,
  familiarity=excluded.familiarity,
  response_preference=excluded.response_preference,
  interest_tags=excluded.interest_tags,
  emby_preference=excluded.emby_preference,
  trust_level=excluded.trust_level,
  last_emotional_signal=excluded.last_emotional_signal,
  updated_at_ms=excluded.updated_at_ms
`);

const upsertRelationshipStmt = db.prepare(`
INSERT INTO relationships (
  chat_id, user_id, warmth, reciprocity, responsiveness, familiarity,
  last_interaction_ms, last_proactive_ms, updated_at_ms
) VALUES (
  @chat_id, @user_id, @warmth, @reciprocity, @responsiveness, @familiarity,
  @last_interaction_ms, @last_proactive_ms, @updated_at_ms
)
ON CONFLICT(chat_id, user_id) DO UPDATE SET
  warmth=excluded.warmth,
  reciprocity=excluded.reciprocity,
  responsiveness=excluded.responsiveness,
  familiarity=excluded.familiarity,
  last_interaction_ms=excluded.last_interaction_ms,
  last_proactive_ms=excluded.last_proactive_ms,
  updated_at_ms=excluded.updated_at_ms
`);

function nowMs() {
  return Date.now();
}

function normalizeText(text: string) {
  return text.trim().replace(/\s+/g, " ");
}

function clamp(value: number, min: number, max: number) {
  return Math.max(min, Math.min(max, value));
}

function parseJson<T>(value: string, fallback: T): T {
  try {
    return JSON.parse(value) as T;
  } catch {
    return fallback;
  }
}

async function readJson(req: IncomingMessage) {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(Buffer.from(chunk));
  }
  if (chunks.length === 0) return {};
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

function writeJson(res: ServerResponse, status: number, payload?: unknown) {
  res.statusCode = status;
  if (payload === undefined) {
    res.end();
    return;
  }
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(payload));
}

function detectEmotion(text: string) {
  if (/(难受|伤心|烦|崩溃|累|孤独|不开心|委屈)/.test(text)) return "negative";
  if (/(开心|高兴|哈哈|喜欢|爱|棒|爽)/.test(text)) return "positive";
  if (/\?/.test(text) || /吗|呢|如何|怎么/.test(text)) return "seeking";
  return "neutral";
}

function inferInterestTags(text: string) {
  const tags = new Set<string>();
  if (/(电影|电视剧|剧集|片子|动漫)/.test(text)) tags.add("影视");
  if (/(Emby|emby|洗版|资源|入库)/.test(text)) tags.add("Emby");
  if (/(音乐|歌|专辑)/.test(text)) tags.add("音乐");
  if (/(求片|想看|有没有)/.test(text)) tags.add("求片");
  return [...tags].join(",");
}

function inferEmbyPreference(text: string) {
  if (/(4K|HDR|蓝光|洗版|更高清)/i.test(text)) return "quality-first";
  if (/(剧|第\d+季|season)/i.test(text)) return "series";
  if (/(电影|电影版)/.test(text)) return "movie";
  return "";
}

function upsertBeliefAndRelationship(chatId: number, userId: number, userName: string, text: string, isPrivate = false) {
  const ts = nowMs();
  const existingBelief = db.prepare(`SELECT * FROM beliefs WHERE chat_id = ? AND user_id = ?`).get(chatId, userId) as any;
  const familiarity = clamp((existingBelief?.familiarity ?? 0.1) + 0.04, 0.1, 1);
  const trustLevel = clamp((existingBelief?.trust_level ?? 0.3) + (isPrivate ? 0.03 : 0.01), 0.2, 1);
  const userTier = existingBelief?.user_tier ?? "regular";
  upsertBeliefStmt.run({
    chat_id: chatId,
    user_id: userId,
    user_tier: userTier,
    familiarity,
    response_preference: isPrivate ? "warm" : "concise",
    interest_tags: inferInterestTags(text) || existingBelief?.interest_tags || "",
    emby_preference: inferEmbyPreference(text) || existingBelief?.emby_preference || "",
    trust_level: trustLevel,
    last_emotional_signal: detectEmotion(text),
    updated_at_ms: ts,
  });

  const existingRel = db.prepare(`SELECT * FROM relationships WHERE chat_id = ? AND user_id = ?`).get(chatId, userId) as any;
  upsertRelationshipStmt.run({
    chat_id: chatId,
    user_id: userId,
    warmth: clamp((existingRel?.warmth ?? 0.2) + 0.02, 0.1, 1),
    reciprocity: clamp((existingRel?.reciprocity ?? 0.2) + 0.01, 0.1, 1),
    responsiveness: clamp((existingRel?.responsiveness ?? 0.2) + 0.02, 0.1, 1),
    familiarity: clamp((existingRel?.familiarity ?? 0.2) + 0.03, 0.1, 1),
    last_interaction_ms: ts,
    last_proactive_ms: existingRel?.last_proactive_ms ?? 0,
    updated_at_ms: ts,
  });

  db.prepare(
    `INSERT INTO diary_entries (chat_id, user_id, content, summary, created_at_ms) VALUES (?, ?, ?, ?, ?)`,
  ).run(chatId, userId, `用户 ${userName || userId}: ${normalizeText(text)}`, normalizeText(text).slice(0, 240), ts);
}

function persistEvent(event: EventPayload) {
  const ts = Date.parse(event.timestamp) || nowMs();
  db.prepare(
    `INSERT INTO event_log (event_type, chat_id, user_id, user_name, message_id, text, timestamp_ms, metadata_json)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    event.type,
    event.chat_id,
    event.user_id ?? null,
    event.user_name ?? "",
    event.message_id ?? null,
    event.text ?? "",
    ts,
    JSON.stringify(event.metadata ?? {}),
  );

  if (event.user_id && event.text) {
    upsertBeliefAndRelationship(event.chat_id, event.user_id, event.user_name ?? "", event.text, Boolean(event.metadata?.is_private));
  }

  if (event.type.startsWith("request.")) {
    const metadata = event.metadata ?? {};
    const status = String(metadata.status ?? "open");
    const nextDue = ts + 6 * 60 * 60 * 1000;
    const threadId = `request:${event.chat_id}:${metadata.tmdb_id ?? event.message_id ?? ts}`;
    db.prepare(
      `INSERT INTO narrative_threads (id, chat_id, user_id, kind, title, status, summary, next_due_ms, last_event_ms, metadata_json)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET
         status=excluded.status,
         summary=excluded.summary,
         next_due_ms=excluded.next_due_ms,
         last_event_ms=excluded.last_event_ms,
         metadata_json=excluded.metadata_json`,
    ).run(
      threadId,
      event.chat_id,
      event.user_id ?? null,
      "request",
      String(metadata.title ?? "求片流程"),
      status === "fulfilled" || status === "rejected" ? "resolved" : "open",
      event.text ?? "求片线程更新",
      status === "fulfilled" || status === "rejected" ? 0 : nextDue,
      ts,
      JSON.stringify(metadata),
    );
  }
}

function getPersonaState(chatId: number) {
  const row = db.prepare(`SELECT * FROM persona_state WHERE chat_id = ?`).get(chatId) as any;
  if (row) return row;
  const fresh = { chat_id: chatId, diligence: 0.25, curiosity: 0.25, sociability: 0.25, caution: 0.25, updated_at_ms: nowMs() };
  db.prepare(`INSERT INTO persona_state (chat_id, diligence, curiosity, sociability, caution, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?)`)
    .run(chatId, fresh.diligence, fresh.curiosity, fresh.sociability, fresh.caution, fresh.updated_at_ms);
  return fresh;
}

function computePressures(req: RespondRequest) {
  const ts = nowMs();
  const recentCount = Number(
    (db.prepare(`SELECT COUNT(*) as count FROM event_log WHERE chat_id = ? AND timestamp_ms >= ?`).get(req.chat_id, ts - 60 * 60 * 1000) as any)
      ?.count ?? 0,
  );
  const openThreads = Number(
    (db.prepare(`SELECT COUNT(*) as count FROM narrative_threads WHERE chat_id = ? AND status = 'open'`).get(req.chat_id) as any)?.count ?? 0,
  );
  const rel = req.user_id
    ? (db.prepare(`SELECT * FROM relationships WHERE chat_id = ? AND user_id = ?`).get(req.chat_id, req.user_id) as any)
    : null;
  const deltaMs = rel ? ts - Number(rel.last_interaction_ms ?? ts) : 0;
  const p1 = clamp(recentCount * 0.35, 0.1, 4);
  const p2 = clamp(req.knowledge_summary ? 0.4 : 1.2, 0.2, 2.2);
  const p3 = clamp(deltaMs / (1000 * 60 * 60 * 24 * Math.max(req.relationship_decay_days ?? 14, 1)), 0.1, 3.5);
  const p4 = clamp(openThreads * 0.8, 0.1, 4);
  const p5 = clamp((req.is_private ? 1.2 : 0.5) + (/\?/.test(req.text) ? 1.1 : 0.2), 0.2, 3.5);
  const p6 = clamp(/(最新|推荐|想看|有没有|什么|推荐一下|新闻|发现)/.test(req.text) ? 1.4 : 0.4, 0.1, 2.5);
  return { P1: p1, P2: p2, P3: p3, P4: p4, P5: p5, P6: p6 };
}

function selectVoice(chatId: number, pressures: Record<string, number>) {
  const persona = getPersonaState(chatId);
  const scores = {
    diligence: persona.diligence * (pressures.P4 + pressures.P5),
    curiosity: persona.curiosity * (pressures.P2 + pressures.P6),
    sociability: persona.sociability * (pressures.P3 + (pressures.P1 * 0.5)),
    caution: persona.caution * (pressures.P5 + 0.6),
  };
  const voice = (Object.entries(scores).sort((a, b) => b[1] - a[1])[0] ?? ["diligence"])[0];
  return { voice, scores, persona };
}

function buildDynamicTail(req: RespondRequest) {
  const belief = req.user_id
    ? (db.prepare(`SELECT * FROM beliefs WHERE chat_id = ? AND user_id = ?`).get(req.chat_id, req.user_id) as any)
    : null;
  const rel = req.user_id
    ? (db.prepare(`SELECT * FROM relationships WHERE chat_id = ? AND user_id = ?`).get(req.chat_id, req.user_id) as any)
    : null;
  const diary = db
    .prepare(`SELECT summary FROM diary_entries WHERE chat_id = ? ORDER BY created_at_ms DESC LIMIT 5`)
    .all(req.chat_id) as Array<{ summary: string }>;
  const threads = db
    .prepare(`SELECT title, summary FROM narrative_threads WHERE chat_id = ? AND status = 'open' ORDER BY last_event_ms DESC LIMIT 5`)
    .all(req.chat_id) as Array<{ title: string; summary: string }>;
  const bits: string[] = [];
  if (belief) {
    bits.push(`当前用户画像：tier=${belief.user_tier}, familiarity=${Number(belief.familiarity).toFixed(2)}, trust=${Number(belief.trust_level).toFixed(2)}, emotion=${belief.last_emotional_signal}.`);
    if (belief.interest_tags) bits.push(`兴趣标签：${belief.interest_tags}.`);
    if (belief.emby_preference) bits.push(`资源偏好：${belief.emby_preference}.`);
  }
  if (rel) {
    bits.push(`关系向量：warmth=${Number(rel.warmth).toFixed(2)}, reciprocity=${Number(rel.reciprocity).toFixed(2)}, responsiveness=${Number(rel.responsiveness).toFixed(2)}, familiarity=${Number(rel.familiarity).toFixed(2)}.`);
  }
  if (threads.length > 0) {
    bits.push(`开放线程：${threads.map((t) => `${t.title}(${t.summary})`).join("；")}`);
  }
  if (diary.length > 0) {
    bits.push(`近期日记：${diary.map((d) => d.summary).join("；")}`);
  }
  if (req.knowledge_summary) {
    bits.push(`知识摘要：${req.knowledge_summary.slice(0, 1200)}`);
  }
  if ((req.skill_summaries?.length ?? 0) > 0) {
    bits.push(`可按需读取的技能：${req.skill_summaries!.join("；")}`);
  }
  if ((req.job_summaries?.length ?? 0) > 0) {
    bits.push(`当前任务摘要：${req.job_summaries!.join("；")}`);
  }
  return bits.join("\n");
}

function buildStaticPrompt(req: RespondRequest, voice: string, pressures: Record<string, number>) {
  return [
    req.static_prompt || "你是 EmbyRadar 的自治型 AI 助手。",
    `人格种子：${req.persona_seed || "谨慎、会长期记住关系变化、保持直接。"}。`,
    "你使用单一工具 run(command)。命令空间仅限：chat.say、chat.reply、chat.pin、web.search、tmdb.search、emby.search、emby.latest、embyboss.user、skill.read、job.read、job.write、memory.search、diary.write。",
    "复杂写入命令使用 JSON 作为参数体，例如：job.write {\"job_id\":\"daily-check\",\"content\":\"...\"}。",
    "绝对禁止编造系统事实。需要外部事实时先用 run(command) 取证。",
    "回复保持自然、简洁、像长期生活在群里的实体，不要自称在执行流程，也不要暴露 pressure、voice、tool budget 这些内部术语。",
    `当前主导倾向：${voice}。六维压力快照：${JSON.stringify(pressures)}。`,
  ].join("\n\n");
}

function buildUserPrompt(req: RespondRequest, dynamicTail: string) {
  const history = (req.recent_context ?? [])
    .slice(-12)
    .map((item) => `${item.role}: ${item.text}`)
    .join("\n");
  return [
    dynamicTail,
    history ? `近期上下文：\n${history}` : "",
    `当前消息来自 ${req.user_name || req.user_id || "unknown"}：${req.text}`,
  ]
    .filter(Boolean)
    .join("\n\n");
}

function extractAssistantText(message: any): string {
  const content = message?.content;
  if (typeof content === "string") return content.trim();
  if (!Array.isArray(content)) return "";
  return content
    .map((item) => (typeof item?.text === "string" ? item.text : ""))
    .filter(Boolean)
    .join("\n")
    .trim();
}

async function callLLM(
  llm: LLMConfig,
  body: Record<string, unknown>,
): Promise<any> {
  const response = await fetch(`${llm.base_url.replace(/\/$/, "")}/chat/completions`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${llm.api_key}`,
    },
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    throw new Error(`LLM 请求失败 (${response.status}): ${await response.text()}`);
  }
  return response.json();
}

function parseCommand(command: string) {
  const trimmed = command.trim();
  if (!trimmed) return { name: "", args: "" };
  const space = trimmed.search(/\s/);
  if (space === -1) return { name: trimmed, args: "" };
  return { name: trimmed.slice(0, space), args: trimmed.slice(space + 1).trim() };
}

function applyLocalCommand(command: string, req: RespondRequest) {
  const { name, args } = parseCommand(command);
  if (name !== "diary.write") return null;
  const text = normalizeText(args || req.text).slice(0, 4000);
  db.prepare(`INSERT INTO diary_entries (chat_id, user_id, content, summary, created_at_ms) VALUES (?, ?, ?, ?, ?)`)
    .run(req.chat_id, req.user_id ?? null, text, text.slice(0, 240), nowMs());
  return { output: "diary entry written" };
}

async function executeRunCommand(command: string, req: RespondRequest) {
  const local = applyLocalCommand(command, req);
  if (local) return local;
  const response = await fetch(`${req.engine_base_url.replace(/\/$/, "")}/engine/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      command,
      context: {
        chat_id: req.chat_id,
        sender_id: req.user_id,
        message_id: req.message_id,
        reply_to_message_id: req.reply_to_message_id,
        is_private: req.is_private,
        allow_sensitive: req.allow_sensitive,
      },
    }),
  });
  if (!response.ok) {
    throw new Error(await response.text());
  }
  return response.json();
}

async function runToolLoop(req: RespondRequest, system: string, user: string) {
  const messages: Array<Record<string, unknown>> = [
    { role: "system", content: system },
    { role: "user", content: user },
  ];
  const transcript: Array<{ command: string; output: string }> = [];

  for (let i = 0; i < 8; i++) {
    const response = await callLLM(req.llm, {
      model: req.llm.model,
      messages,
      max_tokens: req.llm.max_tokens,
      temperature: req.llm.temperature,
      tools: [
        {
          type: "function",
          function: {
            name: "run",
            description: "执行命令空间中的受控命令。",
            parameters: {
              type: "object",
              properties: {
                command: {
                  type: "string",
                  description: "要执行的命令字符串。",
                },
              },
              required: ["command"],
            },
          },
        },
      ],
      tool_choice: i === 0 ? "auto" : "auto",
    });

    const message = response?.choices?.[0]?.message;
    if (!message) throw new Error("LLM 未返回 message");
    messages.push(message);
    const toolCalls = Array.isArray(message.tool_calls) ? message.tool_calls : [];
    if (toolCalls.length === 0) {
      return {
        replyText: extractAssistantText(message),
        transcript,
      };
    }

    for (const call of toolCalls) {
      const parsed = parseJson<{ command?: string }>(call.function?.arguments ?? "{}", {});
      const command = String(parsed.command ?? "").trim();
      if (!command) {
        messages.push({
          role: "tool",
          tool_call_id: call.id,
          content: "missing command",
        });
        continue;
      }
      const result = await executeRunCommand(command, req);
      const output = typeof result?.output === "string" ? result.output : JSON.stringify(result);
      transcript.push({ command, output });
      messages.push({
        role: "tool",
        tool_call_id: call.id,
        content: output,
      });
    }
  }

  return {
    replyText: "（达到工具预算上限，先收一下）",
    transcript,
  };
}

async function completeText(llm: LLMConfig, system: string, userText: string) {
  const response = await callLLM(llm, {
    model: llm.model,
    messages: [
      { role: "system", content: system },
      { role: "user", content: userText },
    ],
    max_tokens: llm.max_tokens,
    temperature: llm.temperature,
  });
  return extractAssistantText(response?.choices?.[0]?.message);
}

async function handleRespond(req: RespondRequest) {
  const pressures = computePressures(req);
  const { voice, persona } = selectVoice(req.chat_id, pressures);
  const dynamicTail = buildDynamicTail(req);
  const system = buildStaticPrompt(req, voice, pressures);
  const user = buildUserPrompt(req, dynamicTail);
  const episodeId = randomUUID();
  const ts = nowMs();
  db.prepare(
    `INSERT INTO episodes (id, chat_id, user_id, origin, status, voice, pressures_json, tool_call_count, created_at_ms, updated_at_ms)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(episodeId, req.chat_id, req.user_id ?? null, "respond", "running", voice, JSON.stringify(pressures), 0, ts, ts);

  const result = await runToolLoop(req, system, user);

  db.prepare(`UPDATE episodes SET status = ?, tool_call_count = ?, updated_at_ms = ? WHERE id = ?`)
    .run("completed", result.transcript.length, nowMs(), episodeId);
  db.prepare(`INSERT INTO diary_entries (chat_id, user_id, content, summary, created_at_ms) VALUES (?, ?, ?, ?, ?)`)
    .run(
      req.chat_id,
      req.user_id ?? null,
      `用户: ${normalizeText(req.text)}\nAI: ${normalizeText(result.replyText)}`,
      normalizeText(result.replyText).slice(0, 240),
      nowMs(),
    );

  return {
    episode_id: episodeId,
    reply_text: result.replyText,
    tool_transcript: result.transcript,
    memory_hits: [],
    persona_snapshot: {
      voice,
      pressures,
      persona,
    },
  };
}

function maybeCreateRelationshipAction(quietHours: number[], cooldownMinutes: number, relationshipDecayDays: number) {
  const hour = new Date().getHours();
  if (quietHours.includes(hour)) return null;
  const thresholdMs = relationshipDecayDays * 24 * 60 * 60 * 1000;
  const cooldownMs = cooldownMinutes * 60 * 1000;
  const row = db
    .prepare(
      `SELECT * FROM relationships
       WHERE last_interaction_ms <= ? AND (last_proactive_ms IS NULL OR last_proactive_ms <= ?)
       ORDER BY last_interaction_ms ASC
       LIMIT 1`,
    )
    .get(nowMs() - thresholdMs, nowMs() - cooldownMs) as any;
  if (!row) return null;
  const actionId = randomUUID();
  const command = "chat.say 想起你了，最近怎么样？";
  db.prepare(
    `INSERT INTO scheduled_actions (id, chat_id, user_id, kind, command, status, due_ms, last_error, metadata_json, created_at_ms, updated_at_ms)
     VALUES (?, ?, ?, ?, ?, ?, ?, '', '{}', ?, ?)`,
  ).run(actionId, row.chat_id, row.user_id, "relationship_checkin", command, "pending", nowMs(), nowMs(), nowMs());
  return actionId;
}

function maybeCreateThreadAction(quietHours: number[]) {
  const hour = new Date().getHours();
  if (quietHours.includes(hour)) return null;
  const thread = db
    .prepare(`SELECT * FROM narrative_threads WHERE status = 'open' AND next_due_ms > 0 AND next_due_ms <= ? ORDER BY next_due_ms ASC LIMIT 1`)
    .get(nowMs()) as any;
  if (!thread) return null;
  const actionId = randomUUID();
  const command = `chat.say 提醒一下，${thread.title} 这件事我还记着：${thread.summary}`;
  db.prepare(
    `INSERT INTO scheduled_actions (id, chat_id, user_id, kind, command, status, due_ms, last_error, metadata_json, created_at_ms, updated_at_ms)
     VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
  ).run(actionId, thread.chat_id, thread.user_id ?? null, "thread_followup", command, "pending", nowMs(), thread.metadata_json ?? "{}", nowMs(), nowMs());
  db.prepare(`UPDATE narrative_threads SET next_due_ms = ?, last_event_ms = ? WHERE id = ?`).run(nowMs() + 12 * 60 * 60 * 1000, nowMs(), thread.id);
  return actionId;
}

function claimAction(payload: Record<string, unknown>) {
  const quietHours = Array.isArray(payload.quiet_hours) ? (payload.quiet_hours as number[]) : [0, 1, 2, 3, 4, 5, 6];
  const cooldown = Number(payload.proactive_cooldown_minutes ?? 720);
  const decayDays = Number(payload.relationship_decay_days ?? 14);
  maybeCreateThreadAction(quietHours);
  maybeCreateRelationshipAction(quietHours, cooldown, decayDays);

  const action = db
    .prepare(`SELECT * FROM scheduled_actions WHERE status = 'pending' AND due_ms <= ? ORDER BY due_ms ASC LIMIT 1`)
    .get(nowMs()) as any;
  if (!action) return null;
  db.prepare(`UPDATE scheduled_actions SET status = 'in_progress', updated_at_ms = ? WHERE id = ?`).run(nowMs(), action.id);
  return {
    id: action.id,
    kind: action.kind,
    command: action.command,
    context: {
      chat_id: action.chat_id,
      sender_id: action.user_id ?? undefined,
      is_private: true,
      allow_sensitive: false,
    },
    meta: parseJson<Record<string, unknown>>(action.metadata_json ?? "{}", {}),
  };
}

function completeAction(actionId: string, payload: Record<string, unknown>) {
  const success = Boolean(payload.success);
  db.prepare(`UPDATE scheduled_actions SET status = ?, last_error = ?, updated_at_ms = ? WHERE id = ?`).run(
    success ? "completed" : "failed",
    String(payload.error_text ?? ""),
    nowMs(),
    actionId,
  );
  const action = db.prepare(`SELECT * FROM scheduled_actions WHERE id = ?`).get(actionId) as any;
  if (action?.user_id) {
    db.prepare(`UPDATE relationships SET last_proactive_ms = ?, updated_at_ms = ? WHERE chat_id = ? AND user_id = ?`)
      .run(nowMs(), nowMs(), action.chat_id, action.user_id);
  }
}

function coerceIntent(raw: string) {
  const normalized = raw.trim().replace(/^```json/, "").replace(/^```/, "").replace(/```$/, "").trim();
  const parsed = parseJson<Record<string, unknown>>(normalized, {});
  return {
    name: String(parsed.name ?? ""),
    type: String(parsed.type ?? ""),
    year: String(parsed.year ?? ""),
    is_remaster: Boolean(parsed.is_remaster),
    season: Number(parsed.season ?? 0),
  };
}

const server = createServer(async (req, res) => {
  try {
    if (!req.url) {
      writeJson(res, 404, { error: "missing url" });
      return;
    }
    const url = new URL(req.url, `http://127.0.0.1:${port}`);
    if (req.method === "GET" && url.pathname === "/health") {
      writeJson(res, 200, { ok: true, db_path: dbPath });
      return;
    }
    if (req.method !== "POST") {
      writeJson(res, 405, { error: "method not allowed" });
      return;
    }

    const payload = (await readJson(req)) as Record<string, unknown>;

    if (url.pathname === "/cognition/events") {
      persistEvent(payload as unknown as EventPayload);
      writeJson(res, 200, { ok: true });
      return;
    }
    if (url.pathname === "/cognition/respond") {
      const respondPayload = payload as unknown as RespondRequest;
      const result = await handleRespond(respondPayload);
      writeJson(res, 200, result);
      return;
    }
    if (url.pathname === "/cognition/request-intent") {
      const llm = (payload.llm ?? {}) as LLMConfig;
      const text = String(payload.text ?? "");
      const prompt = [
        "你是一个影视信息提取助手。请从用户文本中提取 name、type(movie/tv)、year、is_remaster、season。",
        "只返回 JSON。",
      ].join("\n");
      const raw = await completeText(llm, prompt, text);
      writeJson(res, 200, coerceIntent(raw));
      return;
    }
    if (
      url.pathname === "/cognition/format-knowledge" ||
      url.pathname === "/cognition/merge-knowledge" ||
      url.pathname === "/cognition/summarize-digest"
    ) {
      const llm = (payload.llm ?? {}) as LLMConfig;
      const systemHint = String(payload.system_hint ?? "你是一个文本整理助手。");
      const userText = String(payload.user_text ?? "");
      const text = await completeText(llm, systemHint, userText);
      writeJson(res, 200, { text });
      return;
    }
    if (url.pathname === "/cognition/actions/claim") {
      const action = claimAction(payload);
      if (!action) {
        writeJson(res, 204);
        return;
      }
      writeJson(res, 200, action);
      return;
    }
    if (url.pathname.startsWith("/cognition/actions/") && url.pathname.endsWith("/result")) {
      const actionId = url.pathname.split("/")[3] ?? "";
      completeAction(actionId, payload);
      writeJson(res, 200, { ok: true });
      return;
    }

    writeJson(res, 404, { error: "not found" });
  } catch (error) {
    writeJson(res, 500, { error: error instanceof Error ? error.message : String(error) });
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`[cognition] listening on http://127.0.0.1:${port}`);
  console.log(`[cognition] sqlite at ${dbPath}`);
});
