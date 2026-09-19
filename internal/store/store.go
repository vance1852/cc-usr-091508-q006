// Package store 是样品链路服务的持久化层：全部业务不变量都在 SQLite 事务内完成检查。
//
// 设计要点：
//   - 事件类表（custody_events / deviation_events / result_adoptions /
//     container_registrations）只增不改，数据库触发器封禁 UPDATE/DELETE，
//     历史不能靠改写记录消失。
//   - samples 表只保存当前状态指针（当前保管人、版本号、hold 标志、采用版本），
//     保管人切换以 version 做乐观并发控制，保证"两人同时交接后只有一名当前保管人"。
//   - transfers 中 status='released' 的待接收记录对同一样品/容器至多一条
//     （部分唯一索引），杜绝重复离线扫码产生两个在途交接。
//   - 所有写操作携带 op_id 记入 ops 幂等台账：离线恢复重放同一操作不会产生
//     重复事件；同 op_id 改载荷则直接 409。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// Store 包装 SQLite 连接。所有写操作都在内部 IMMEDIATE 事务中执行，
// 配合单连接池，避免并发下的状态漂移。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(dsn string) (*Store, error) {
	if !strings.Contains(dsn, "?") {
		dsn += "?"
	} else {
		dsn += "&"
	}
	// _txlock=immediate 让 database/sql 的隐式 BEGIN 变为 BEGIN IMMEDIATE。
	dsn += "_txlock=immediate&_foreign_keys=on&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接序列化全部写入，CAS/SELECT-then-UPDATE 在进程内天然安全。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接给只读查询（如下钻聚合视图）。
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

const schema = `
-- 人员：班组用于"采样班组不得复核自己的检测"的职责分离判定。
CREATE TABLE IF NOT EXISTS people (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  team       TEXT NOT NULL,
  role       TEXT NOT NULL CHECK (role IN
               ('sampler','courier','analyst','lab_admin','reviewer','approver','external_lab')),
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 登录令牌（无密码，令牌即身份；测试与种子数据直接写入）。
CREATE TABLE IF NOT EXISTS tokens (
  token      TEXT PRIMARY KEY,
  person_id  TEXT NOT NULL REFERENCES people(id),
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 试车批次
CREATE TABLE IF NOT EXISTS batches (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL,
  product         TEXT NOT NULL,              -- 例如 LOX（液氧）
  created_by      TEXT NOT NULL REFERENCES people(id),
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 采样点（同一批次下名称唯一）
CREATE TABLE IF NOT EXISTS sampling_points (
  id         TEXT PRIMARY KEY,
  batch_id   TEXT NOT NULL REFERENCES batches(id),
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE(batch_id, name)
);

-- 容器（采集瓶/转运箱等）。active_sample_id 指向当前盛装样品，清空即空闲，
-- 因此容器在任意时刻最多关联一个在用样品，支持"清空后再次使用"。
CREATE TABLE IF NOT EXISTS containers (
  id               TEXT PRIMARY KEY,
  kind             TEXT NOT NULL CHECK (kind IN ('bottle','transit_box')),
  active_sample_id TEXT,
  retired          INTEGER NOT NULL DEFAULT 0,
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 容器编号登记（采集瓶/转运箱/实验室三套编号）。只增：编号冲突不能靠改登记消失。
CREATE TABLE IF NOT EXISTS container_registrations (
  id            TEXT PRIMARY KEY,
  container_id  TEXT NOT NULL REFERENCES containers(id),
  scope         TEXT NOT NULL CHECK (scope IN ('collection','transit','lab')),
  code          TEXT NOT NULL,
  sample_id     TEXT,
  created_by    TEXT NOT NULL REFERENCES people(id),
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE(scope, code)
);
CREATE INDEX IF NOT EXISTS idx_reg_container ON container_registrations(container_id);

-- 温度允许区间（按批次设定，取最新一条）；接收读数与其无交集即温度越界。
CREATE TABLE IF NOT EXISTS temp_limits (
  id           TEXT PRIMARY KEY,
  batch_id     TEXT NOT NULL REFERENCES batches(id),
  min_celsius  REAL NOT NULL,
  max_celsius  REAL NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK (min_celsius <= max_celsius)
);

-- 检测方法
CREATE TABLE IF NOT EXISTS methods (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 样品主表：只存当前态。hold 与 status 正交：任一 open 偏差 => hold=1，
-- hold=1 时样品"暂停后续使用"（禁止转运/消耗/采用）。
CREATE TABLE IF NOT EXISTS samples (
  id             TEXT PRIMARY KEY,
  public_code    TEXT NOT NULL UNIQUE,        -- 对外脱敏批号（外部实验室唯一可见编号）
  batch_id       TEXT NOT NULL REFERENCES batches(id),
  point_id       TEXT NOT NULL REFERENCES sampling_points(id),
  collector_id   TEXT NOT NULL REFERENCES people(id),
  collector_team TEXT NOT NULL,
  container_id   TEXT NOT NULL REFERENCES containers(id),
  seal_no        TEXT NOT NULL DEFAULT '',    -- 封签号
  seal_intact    INTEGER NOT NULL,            -- 采集时封签状态
  collected_at   TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active','consumed','void')),
  hold           INTEGER NOT NULL DEFAULT 0,
  hold_reason    TEXT NOT NULL DEFAULT '',
  current_holder_id TEXT NOT NULL REFERENCES people(id),
  result_version INTEGER NOT NULL DEFAULT 0,  -- 当前采用的结果版本，0=尚无结论
  version        INTEGER NOT NULL DEFAULT 1,  -- 保管/状态乐观锁
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_samples_batch ON samples(batch_id);

-- 保管事件（append-only）：逐站去向都在这里。
CREATE TABLE IF NOT EXISTS custody_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id   TEXT NOT NULL REFERENCES samples(id),
  seq         INTEGER NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('collect','release','receive','cancel','consume')),
  actor_id    TEXT NOT NULL REFERENCES people(id),
  actor_team  TEXT NOT NULL,
  transfer_id TEXT,
  note        TEXT NOT NULL DEFAULT '',
  seal_ok     INTEGER,                        -- 仅 receive 有值
  temp_min    REAL,                           -- 仅 receive 有值；NULL 与空白记录同义
  temp_max    REAL,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_custody_seq ON custody_events(sample_id, seq);
CREATE TRIGGER IF NOT EXISTS trg_custody_no_upd BEFORE UPDATE ON custody_events
BEGIN SELECT RAISE(ABORT, 'custody_events is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_custody_no_del BEFORE DELETE ON custody_events
BEGIN SELECT RAISE(ABORT, 'custody_events is append-only'); END;

-- 交接单：released -> received/cancelled。接收人必须回传 release_code 才能确认，
-- 即"每次接收都承接上一位保管人的确认"。
CREATE TABLE IF NOT EXISTS transfers (
  id               TEXT PRIMARY KEY,
  op_id            TEXT NOT NULL UNIQUE,      -- 交出操作幂等键
  receive_op_id    TEXT UNIQUE,               -- 接收操作幂等键（离线重放防护）
  sample_id        TEXT NOT NULL REFERENCES samples(id),
  container_id     TEXT NOT NULL REFERENCES containers(id),
  from_person_id   TEXT NOT NULL REFERENCES people(id),
  to_person_id     TEXT NOT NULL REFERENCES people(id),
  release_code     TEXT NOT NULL,             -- 交出人确认码（扫码内容）
  release_note     TEXT NOT NULL DEFAULT '',
  seal_at_release  INTEGER NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('released','received','cancelled')),
  released_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  received_at      TEXT,
  receiver_team    TEXT,
  seal_at_receive  INTEGER,
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
-- 同一容器/样品至多一条在途交接：并发或重复离线扫码的第二条必然失败。
CREATE UNIQUE INDEX IF NOT EXISTS idx_transfer_open_container
  ON transfers(container_id) WHERE status = 'released';
CREATE UNIQUE INDEX IF NOT EXISTS idx_transfer_open_sample
  ON transfers(sample_id) WHERE status = 'released';
CREATE INDEX IF NOT EXISTS idx_transfers_sample ON transfers(sample_id);

-- 偏差（只增的实体；status 只能 open -> dispositioned/rejected）。
CREATE TABLE IF NOT EXISTS deviations (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  op_id       TEXT NOT NULL UNIQUE,
  sample_id   TEXT NOT NULL REFERENCES samples(id),
  transfer_id TEXT,
  kind        TEXT NOT NULL CHECK (kind IN
                ('seal_broken','temp_excursion','temp_gap','code_conflict','other')),
  title       TEXT NOT NULL,
  detail      TEXT NOT NULL DEFAULT '',
  opened_by   TEXT NOT NULL REFERENCES people(id),
  opened_team TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('open','dispositioned','rejected')),
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  closed_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_dev_sample ON deviations(sample_id);

-- 偏差处置事件（append-only）：调查/纠正/复测/拒收/关闭，处置链可逐站回放。
CREATE TABLE IF NOT EXISTS deviation_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  deviation_id INTEGER NOT NULL REFERENCES deviations(id),
  action       TEXT NOT NULL CHECK (action IN
                 ('open','investigate','corrective_action','resample','reject','close')),
  actor_id     TEXT NOT NULL REFERENCES people(id),
  actor_team   TEXT NOT NULL,
  note         TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_devevents_dev ON deviation_events(deviation_id);
CREATE TRIGGER IF NOT EXISTS trg_devevents_no_upd BEFORE UPDATE ON deviation_events
BEGIN SELECT RAISE(ABORT, 'deviation_events is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_devevents_no_del BEFORE DELETE ON deviation_events
BEGIN SELECT RAISE(ABORT, 'deviation_events is append-only'); END;

-- 检测结果版本（只增行；状态列可单向推进 submitted -> reviewed/rejected，
-- 原始读数列写入后永不修改，纠正只能新建版本引用旧版本）。
CREATE TABLE IF NOT EXISTS results (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id          TEXT NOT NULL REFERENCES samples(id),
  version            INTEGER NOT NULL,
  method_id          TEXT NOT NULL REFERENCES methods(id),
  raw_reading        TEXT NOT NULL,           -- 原始读数（原样留存）
  raw_unit           TEXT NOT NULL DEFAULT '',
  purity             REAL NOT NULL,
  supersedes_version INTEGER NOT NULL DEFAULT 0,
  analyst_id         TEXT NOT NULL REFERENCES people(id),
  analyst_team       TEXT NOT NULL,
  is_external        INTEGER NOT NULL DEFAULT 0,
  status             TEXT NOT NULL CHECK (status IN ('submitted','reviewed','rejected')),
  review_note        TEXT NOT NULL DEFAULT '',
  reviewed_by        TEXT REFERENCES people(id),
  reviewed_at        TEXT,
  is_retest          INTEGER NOT NULL DEFAULT 0,
  created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_results_version ON results(sample_id, version);
CREATE INDEX IF NOT EXISTS idx_results_sample ON results(sample_id);

-- 采用记录（append-only）：样品当前采用哪个版本看 samples.result_version，
-- 历次采用决定全部留痕，供下钻核对"采用的结果版本"。
CREATE TABLE IF NOT EXISTS result_adoptions (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id   TEXT NOT NULL REFERENCES samples(id),
  seq         INTEGER NOT NULL,
  version     INTEGER NOT NULL,
  actor_id    TEXT NOT NULL REFERENCES people(id),
  actor_team  TEXT NOT NULL,
  note        TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_adoptions_seq ON result_adoptions(sample_id, seq);
CREATE TRIGGER IF NOT EXISTS trg_adoptions_no_upd BEFORE UPDATE ON result_adoptions
BEGIN SELECT RAISE(ABORT, 'result_adoptions is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_adoptions_no_del BEFORE DELETE ON result_adoptions
BEGIN SELECT RAISE(ABORT, 'result_adoptions is append-only'); END;

-- 外部实验室委托：外部只能凭脱敏批号看到被委托的样品与方法，不见内部真码。
CREATE TABLE IF NOT EXISTS external_assignments (
  public_code TEXT PRIMARY KEY REFERENCES samples(public_code),
  method_id   TEXT NOT NULL REFERENCES methods(id),
  assigned_by TEXT NOT NULL REFERENCES people(id),
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 写操作幂等台账（离线扫码恢复）。同 op_id 必须同发起人/同类型/同载荷哈希。
CREATE TABLE IF NOT EXISTS ops (
  op_id      TEXT PRIMARY KEY,
  actor_id   TEXT NOT NULL,
  op_type    TEXT NOT NULL,
  req_hash   TEXT NOT NULL,
  ref_id     TEXT NOT NULL DEFAULT '',        -- 首执产生的资源标识（重放时原样回传）
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- 编号登记同样禁改禁删。
CREATE TRIGGER IF NOT EXISTS trg_reg_no_upd BEFORE UPDATE ON container_registrations
BEGIN SELECT RAISE(ABORT, 'container_registrations is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_reg_no_del BEFORE DELETE ON container_registrations
BEGIN SELECT RAISE(ABORT, 'container_registrations is append-only'); END;
`

// ErrCode 区分业务错误，API 层据此映射 HTTP 状态码。
type ErrCode string

const (
	ErrConflict   ErrCode = "conflict"        // 编号冲突 / 容器占用 / 版本冲突
	ErrValidation ErrCode = "validation"      // 入参不合法
	ErrCustody    ErrCode = "custody"         // 保管链断裂
	ErrDuty       ErrCode = "duty_separation" // 跨班组复核被拒
	ErrState      ErrCode = "invalid_state"   // 状态不允许该操作（含 hold 暂停）
	ErrReplay     ErrCode = "op_replay"       // 同 op_id 不同载荷
	ErrNotFound   ErrCode = "not_found"
	ErrPermission ErrCode = "forbidden"
)

// BizError 是带错误码的业务错误。
type BizError struct {
	Code    ErrCode
	Message string
}

func (e *BizError) Error() string { return string(e.Code) + ": " + e.Message }

func bizErr(code ErrCode, format string, args ...any) error {
	return &BizError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func asBiz(err error) (*BizError, bool) {
	var be *BizError
	return be, errors.As(err, &be)
}
