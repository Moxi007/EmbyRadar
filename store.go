package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// 纯 Go 实现的 SQLite 驱动，无需 CGO
	_ "modernc.org/sqlite"
)

// RequestRecord 对应 SQLite requests 表的一行记录
type RequestRecord struct {
	ID          int64      // 自增主键
	ChatID      int64      // 群聊 ID
	UserID      int64      // 请求者 Telegram ID
	UserName    string     // 请求者显示名
	TMDBID      int        // TMDB 条目 ID
	Title       string     // 影视标题
	MediaType   string     // "movie" 或 "tv"
	Year        string     // 年份
	IsRemaster  bool       // 是否洗版
	Status      string     // pending/approved/rejected/fulfilled/expired
	CoinCost    int        // 本次求片实际扣除的金币数，用于拒绝时退还
	CreatedAt   time.Time  // 创建时间
	ApprovedAt  *time.Time // 审批时间（可为空）
	FulfilledAt *time.Time // 入库确认时间（可为空）
}

// RequestStore 封装求片记录的 SQLite 持久化操作
type RequestStore struct {
	db *sql.DB
}

// 建表及索引的 DDL 语句
const createTableSQL = `
CREATE TABLE IF NOT EXISTS requests (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id      INTEGER NOT NULL,
    user_id      INTEGER NOT NULL,
    user_name    TEXT    NOT NULL DEFAULT '',
    tmdb_id      INTEGER NOT NULL,
    title        TEXT    NOT NULL DEFAULT '',
    media_type   TEXT    NOT NULL DEFAULT 'movie',
    year         TEXT    NOT NULL DEFAULT '',
    is_remaster  INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT 'pending',
    created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    approved_at  DATETIME,
    fulfilled_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_requests_status ON requests(status);
CREATE INDEX IF NOT EXISTS idx_requests_dedup ON requests(chat_id, user_id, tmdb_id, status);

CREATE TABLE IF NOT EXISTS admin_messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id   INTEGER NOT NULL,
    admin_id     INTEGER NOT NULL,
    message_id   INTEGER NOT NULL,
    message_text TEXT    NOT NULL DEFAULT '',
    FOREIGN KEY (request_id) REFERENCES requests(id)
);

CREATE INDEX IF NOT EXISTS idx_admin_messages_request ON admin_messages(request_id);
`

// NewRequestStore 创建并初始化数据库连接，自动创建目录和执行建表迁移
func NewRequestStore(dbPath string) (*RequestStore, error) {
	// 自动创建数据库文件所在目录
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 执行建表迁移（幂等操作，已存在则跳过）
	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("执行建表迁移失败: %w", err)
	}

	// 幂等迁移：为 requests 表添加 coin_cost 列（已存在则静默忽略）
	db.Exec(`ALTER TABLE requests ADD COLUMN coin_cost INTEGER NOT NULL DEFAULT 0`)

	return &RequestStore{db: db}, nil
}

// Close 关闭数据库连接
func (s *RequestStore) Close() error {
	return s.db.Close()
}

// InsertRequest 插入一条新的求片记录，状态为 pending
func (s *RequestStore) InsertRequest(r *RequestRecord) error {
	result, err := s.db.Exec(
		`INSERT INTO requests (chat_id, user_id, user_name, tmdb_id, title, media_type, year, is_remaster, status, coin_cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		r.ChatID, r.UserID, r.UserName, r.TMDBID, r.Title, r.MediaType, r.Year, boolToInt(r.IsRemaster), r.CoinCost,
	)
	if err != nil {
		return fmt.Errorf("插入求片记录失败: %w", err)
	}

	// 回填自增 ID
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("获取插入 ID 失败: %w", err)
	}
	r.ID = id
	return nil
}

// UpdateStatus 更新指定记录的状态及对应时间戳
// approved 状态设置 approved_at，fulfilled 状态设置 fulfilled_at
func (s *RequestStore) UpdateStatus(id int64, status string) error {
	var query string
	switch status {
	case "approved":
		query = `UPDATE requests SET status = ?, approved_at = datetime('now') WHERE id = ?`
	case "fulfilled":
		query = `UPDATE requests SET status = ?, fulfilled_at = datetime('now') WHERE id = ?`
	default:
		query = `UPDATE requests SET status = ? WHERE id = ?`
	}

	result, err := s.db.Exec(query, status, id)
	if err != nil {
		return fmt.Errorf("更新状态失败: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取影响行数失败: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("未找到 ID 为 %d 的记录", id)
	}
	return nil
}

// HasActiveRequest 检查指定用户和 TMDB ID 是否存在 pending 或 approved 状态的记录（去重检查）
func (s *RequestStore) HasActiveRequest(chatID, userID int64, tmdbID int) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM requests WHERE chat_id = ? AND user_id = ? AND tmdb_id = ? AND status IN ('pending', 'approved')`,
		chatID, userID, tmdbID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("去重检查失败: %w", err)
	}
	return count > 0, nil
}

// ListApproved 查询所有状态为 approved 的记录
func (s *RequestStore) ListApproved() ([]*RequestRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, chat_id, user_id, user_name, tmdb_id, title, media_type, year, is_remaster, status, coin_cost, created_at, approved_at, fulfilled_at
		 FROM requests WHERE status = 'approved'`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询 approved 记录失败: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// FindPendingRequest 根据群聊 ID、用户 ID 和 TMDB ID 查找 pending 状态的记录
// 用于管理员审批时从回调数据定位对应的求片记录
func (s *RequestStore) FindPendingRequest(chatID, userID int64, tmdbID int) (*RequestRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, chat_id, user_id, user_name, tmdb_id, title, media_type, year, is_remaster, status, coin_cost, created_at, approved_at, fulfilled_at
		 FROM requests WHERE chat_id = ? AND user_id = ? AND tmdb_id = ? AND status = 'pending' ORDER BY id DESC LIMIT 1`,
		chatID, userID, tmdbID,
	)

	r := &RequestRecord{}
	var isRemaster int
	var createdAt string
	var approvedAt, fulfilledAt sql.NullString

	err := row.Scan(
		&r.ID, &r.ChatID, &r.UserID, &r.UserName, &r.TMDBID,
		&r.Title, &r.MediaType, &r.Year, &isRemaster, &r.Status,
		&r.CoinCost, &createdAt, &approvedAt, &fulfilledAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查找 pending 记录失败: %w", err)
	}

	r.IsRemaster = isRemaster != 0
	if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
		r.CreatedAt = t
	}
	if approvedAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", approvedAt.String); err == nil {
			r.ApprovedAt = &t
		}
	}
	if fulfilledAt.Valid {
		if t, err := time.Parse("2006-01-02 15:04:05", fulfilledAt.String); err == nil {
			r.FulfilledAt = &t
		}
	}

	return r, nil
}

// ListExpiredApproved 查询超过指定天数仍为 approved 的记录
func (s *RequestStore) ListExpiredApproved(days int) ([]*RequestRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, chat_id, user_id, user_name, tmdb_id, title, media_type, year, is_remaster, status, coin_cost, created_at, approved_at, fulfilled_at
		 FROM requests WHERE status = 'approved' AND approved_at <= datetime('now', ?)`,
		fmt.Sprintf("-%d days", days),
	)
	if err != nil {
		return nil, fmt.Errorf("查询过期 approved 记录失败: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// scanRecords 从查询结果中扫描并构建 RequestRecord 切片
func scanRecords(rows *sql.Rows) ([]*RequestRecord, error) {
	var records []*RequestRecord
	for rows.Next() {
		r := &RequestRecord{}
		var isRemaster int
		var createdAt string
		var approvedAt, fulfilledAt sql.NullString

		err := rows.Scan(
			&r.ID, &r.ChatID, &r.UserID, &r.UserName, &r.TMDBID,
			&r.Title, &r.MediaType, &r.Year, &isRemaster, &r.Status,
			&r.CoinCost, &createdAt, &approvedAt, &fulfilledAt,
		)
		if err != nil {
			return nil, fmt.Errorf("扫描记录失败: %w", err)
		}

		r.IsRemaster = isRemaster != 0

		// 解析时间字段
		if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
			r.CreatedAt = t
		}
		if approvedAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", approvedAt.String); err == nil {
				r.ApprovedAt = &t
			}
		}
		if fulfilledAt.Valid {
			if t, err := time.Parse("2006-01-02 15:04:05", fulfilledAt.String); err == nil {
				r.FulfilledAt = &t
			}
		}

		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历记录失败: %w", err)
	}
	return records, nil
}

// boolToInt 将布尔值转换为 SQLite 整数（0/1）
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AdminMessage 管理员消息记录，用于审批后同步更新所有管理员的消息
type AdminMessage struct {
	AdminID     int64  // 管理员 Telegram ID
	MessageID   int    // 消息 ID
	MessageText string // 原始 Markdown 消息文本
}

// SaveAdminMessage 保存管理员收到的求片消息 ID 和原始文本
func (s *RequestStore) SaveAdminMessage(requestID int64, adminID int64, messageID int, messageText string) error {
	_, err := s.db.Exec(
		`INSERT INTO admin_messages (request_id, admin_id, message_id, message_text) VALUES (?, ?, ?, ?)`,
		requestID, adminID, messageID, messageText,
	)
	if err != nil {
		return fmt.Errorf("保存管理员消息记录失败: %w", err)
	}
	return nil
}

// GetAdminMessages 查询指定求片记录对应的所有管理员消息
func (s *RequestStore) GetAdminMessages(requestID int64) ([]AdminMessage, error) {
	rows, err := s.db.Query(
		`SELECT admin_id, message_id, message_text FROM admin_messages WHERE request_id = ?`,
		requestID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询管理员消息失败: %w", err)
	}
	defer rows.Close()

	var msgs []AdminMessage
	for rows.Next() {
		var m AdminMessage
		if err := rows.Scan(&m.AdminID, &m.MessageID, &m.MessageText); err != nil {
			return nil, fmt.Errorf("扫描管理员消息失败: %w", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
