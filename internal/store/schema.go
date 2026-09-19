package store

// schemaDDL 定义样品链路服务的全部表结构。
//
// 设计要点：
//   - 事实表（交接事件、温度读数、偏差、处置、结果版本、原始读数、状态事件、
//     登记留痕）通过触发器禁止 UPDATE/DELETE，任何纠正只能追加新记录；
//   - “当前保管人唯一”“容器一次只绑定一个在途样品”“同一样品只有一个
//     在途交接”等不变量由部分唯一索引 + BEGIN IMMEDIATE 事务共同保证；
//   - 交接事件通过 prev_hash/hash 形成哈希链，供离线扫码恢复后比对链尖；
//   - 偏差是否关闭由“是否存在处置记录”派生，处置本身也只追加。
const schemaDDL = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS crews (
    code TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id           INTEGER PRIMARY KEY,
    token        TEXT    NOT NULL UNIQUE,
    login        TEXT    NOT NULL UNIQUE,
    display_name TEXT    NOT NULL,
    role         TEXT    NOT NULL CHECK (role IN
                   ('sampler','analyst','reviewer','approver','external','admin')),
    crew_id      TEXT    REFERENCES crews(code),
    active       INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS batches (
    id         INTEGER PRIMARY KEY,
    code       TEXT    NOT NULL UNIQUE,
    product    TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    created_by INTEGER NOT NULL REFERENCES users(id)
);

CREATE TABLE IF NOT EXISTS sampling_points (
    id   INTEGER PRIMARY KEY,
    code TEXT    NOT NULL UNIQUE,
    name TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS methods (
    id              INTEGER PRIMARY KEY,
    code            TEXT    NOT NULL UNIQUE,
    name            TEXT    NOT NULL,
    standard        TEXT,
    unit            TEXT,
    temp_min        REAL,
    temp_max        REAL,
    max_gap_minutes INTEGER
);

-- 一次登记对应一个转运箱发运，箱内可有多个采集瓶；箱号在发运未关闭前唯一。
CREATE TABLE IF NOT EXISTS shipments (
    id         INTEGER PRIMARY KEY,
    box_code   TEXT    NOT NULL,
    batch_id   INTEGER NOT NULL REFERENCES batches(id),
    crew_id    TEXT    NOT NULL REFERENCES crews(code),
    created_at TEXT    NOT NULL,
    closed_at  TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_shipment_box_active
    ON shipments(box_code) WHERE closed_at IS NULL;

CREATE TABLE IF NOT EXISTS containers (
    id         INTEGER PRIMARY KEY,
    code       TEXT    NOT NULL UNIQUE,
    kind       TEXT    NOT NULL DEFAULT 'bottle',
    created_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS samples (
    id                   INTEGER PRIMARY KEY,
    batch_id             INTEGER    NOT NULL REFERENCES batches(id),
    shipment_id          INTEGER    NOT NULL REFERENCES shipments(id),
    lab_no              TEXT       NOT NULL UNIQUE,
    point_id            INTEGER    NOT NULL REFERENCES sampling_points(id),
    method_id           INTEGER    NOT NULL REFERENCES methods(id),
    seal_no             TEXT       NOT NULL,
    sampled_at          TEXT       NOT NULL,
    registered_by       INTEGER    NOT NULL REFERENCES users(id),
    crew_id             TEXT       NOT NULL REFERENCES crews(code),
    created_at          TEXT       NOT NULL,
    -- 温度剖面在登记时从检测方法固化，避免方法日后修改改变历史判定
    temp_min            REAL,
    temp_max            REAL,
    max_gap_minutes     INTEGER,
    -- 当前保管人：任何时刻至多一人；容器归还、样品终结后为 NULL
    current_custodian_id INTEGER   REFERENCES users(id),
    custodian_seq       INTEGER    NOT NULL DEFAULT 0,
    current_result_id   INTEGER    REFERENCES result_versions(id),
    consumed_at         TEXT
);
CREATE INDEX IF NOT EXISTS ix_samples_batch ON samples(batch_id);

-- 容器与样品的绑定关系：一个物理容器同时只能被一个未完结样品占用。
CREATE TABLE IF NOT EXISTS container_bindings (
    id           INTEGER PRIMARY KEY,
    container_id INTEGER NOT NULL REFERENCES containers(id),
    sample_id    INTEGER NOT NULL REFERENCES samples(id),
    bound_at     TEXT    NOT NULL,
    released_at  TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_binding_container_active
    ON container_bindings(container_id) WHERE released_at IS NULL;

-- 封签一次性使用。
CREATE TABLE IF NOT EXISTS seals (
    seal_no   TEXT PRIMARY KEY,
    sample_id INTEGER NOT NULL REFERENCES samples(id),
    used_at   TEXT    NOT NULL
);

-- 失败的重复 / 冲突登记留痕（瓶号、箱号、实验室编号被不同班组分别登记等）。
CREATE TABLE IF NOT EXISTS registration_attempts (
    id             INTEGER PRIMARY KEY,
    attempted_at   TEXT    NOT NULL,
    actor_id       INTEGER NOT NULL REFERENCES users(id),
    actor_crew_id  TEXT,
    batch_code     TEXT,
    lab_no         TEXT,
    container_code TEXT,
    box_code       TEXT,
    detail         TEXT    NOT NULL
);

-- 交接事实（哈希链），只追加。
CREATE TABLE IF NOT EXISTS custody_events (
    id                INTEGER PRIMARY KEY,
    sample_id         INTEGER NOT NULL REFERENCES samples(id),
    seq               INTEGER NOT NULL,
    kind              TEXT    NOT NULL CHECK (kind IN (
                        'collect','release','receive','refusal',
                        'recover_release','recover_receive','container_return')),
    station           TEXT,
    from_user_id      INTEGER REFERENCES users(id),
    to_user_id        INTEGER REFERENCES users(id),
    actor_id          INTEGER NOT NULL REFERENCES users(id),
    seal_intact       INTEGER,
    expected_seal_no  TEXT,
    transfer_id       INTEGER,
    client_event_time TEXT,
    idem_key          TEXT,
    note              TEXT,
    recorded_at       TEXT    NOT NULL,
    prev_hash        TEXT    NOT NULL,
    hash             TEXT    NOT NULL,
    UNIQUE(sample_id, seq)
);
CREATE INDEX IF NOT EXISTS ix_custody_sample ON custody_events(sample_id);

-- 离线事件的客户端幂等键，重复同步只生效一次。
CREATE UNIQUE INDEX IF NOT EXISTS ux_custody_idem
    ON custody_events(sample_id, idem_key) WHERE idem_key IS NOT NULL;

-- 两阶段交接：release 后等待指定接收人 receive；保管责任在 receive 前不转移。
CREATE TABLE IF NOT EXISTS transfers (
    id               INTEGER PRIMARY KEY,
    sample_id        INTEGER NOT NULL REFERENCES samples(id),
    -- 交接先建单、事件后写入，release_event_id 先以 0 占位、事务内回填，
    -- 故该外键延迟到事务提交时检查，届时必须已指向真实事件。
    release_event_id INTEGER NOT NULL REFERENCES custody_events(id)
                       DEFERRABLE INITIALLY DEFERRED,
    receive_event_id INTEGER REFERENCES custody_events(id),
    from_user_id     INTEGER NOT NULL REFERENCES users(id),
    to_user_id       INTEGER NOT NULL REFERENCES users(id),
    station          TEXT,
    status           TEXT    NOT NULL CHECK (status IN ('released','received','refused')),
    reason           TEXT,
    created_at       TEXT NOT NULL,
    resolved_at      TEXT
);
-- 同一样品同时只能有一个未完成交接（一物不能两交）。
CREATE UNIQUE INDEX IF NOT EXISTS ux_transfer_open
    ON transfers(sample_id) WHERE status = 'released';

CREATE TABLE IF NOT EXISTS temperature_readings (
    id          INTEGER PRIMARY KEY,
    sample_id   INTEGER NOT NULL REFERENCES samples(id),
    taken_at    TEXT    NOT NULL,
    celsius     REAL    NOT NULL,
    source      TEXT    NOT NULL CHECK (source IN ('online','offline')),
    recorded_by INTEGER NOT NULL REFERENCES users(id),
    recorded_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_temp_sample_time ON temperature_readings(sample_id, taken_at);

-- 偏差只追加；是否关闭由“是否存在结案处置”派生。
CREATE TABLE IF NOT EXISTS deviations (
    id             INTEGER PRIMARY KEY,
    -- 编号冲突可能在样品创建成功之前就被拦下，故允许为空。
    sample_id      INTEGER REFERENCES samples(id),
    type           TEXT    NOT NULL CHECK (type IN (
                     'seal_broken','temp_excursion','temp_gap','number_conflict')),
    dedup_key      TEXT    NOT NULL,
    detail         TEXT    NOT NULL,
    ref_event_id   INTEGER REFERENCES custody_events(id),
    attempt_id     INTEGER REFERENCES registration_attempts(id),
    opened_by      INTEGER NOT NULL REFERENCES users(id),
    opened_at      TEXT    NOT NULL,
    UNIQUE(sample_id, type, dedup_key)
);
CREATE INDEX IF NOT EXISTS ix_dev_sample ON deviations(sample_id);
-- 样品创建前的冲突按登记留痕去重：一次失败登记最多产生一条偏差。
CREATE UNIQUE INDEX IF NOT EXISTS ux_dev_attempt ON deviations(attempt_id) WHERE attempt_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS deviation_dispositions (
    id            INTEGER PRIMARY KEY,
    deviation_id  INTEGER NOT NULL REFERENCES deviations(id),
    decision      TEXT    NOT NULL CHECK (decision IN (
                     'reject_sample','retest','accept_justified')),
    justification TEXT    NOT NULL,
    actor_id      INTEGER NOT NULL REFERENCES users(id),
    decided_at    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_disp_dev ON deviation_dispositions(deviation_id);

CREATE TABLE IF NOT EXISTS result_versions (
    id            INTEGER PRIMARY KEY,
    sample_id     INTEGER NOT NULL REFERENCES samples(id),
    version_no    INTEGER NOT NULL,
    method_id     INTEGER NOT NULL REFERENCES methods(id),
    instrument    TEXT,
    analyst_id    INTEGER NOT NULL REFERENCES users(id),
    -- 只能在创建时指向更早版本（复测引用旧版本）；记录不可改，引用不可事后补。
    supersedes_id INTEGER REFERENCES result_versions(id),
    submitted_at  TEXT    NOT NULL,
    note          TEXT,
    UNIQUE(sample_id, version_no)
);

CREATE TABLE IF NOT EXISTS raw_readings (
    id          INTEGER PRIMARY KEY,
    result_id   INTEGER NOT NULL REFERENCES result_versions(id),
    name        TEXT    NOT NULL,
    value       REAL    NOT NULL,
    unit        TEXT,
    recorded_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_raw_result ON raw_readings(result_id);

-- 结果状态以追加事件表达，最新一条为当前状态。
CREATE TABLE IF NOT EXISTS result_status_events (
    id         INTEGER PRIMARY KEY,
    result_id  INTEGER NOT NULL REFERENCES result_versions(id),
    status     TEXT    NOT NULL CHECK (status IN ('submitted','reviewed','rejected')),
    conclusion TEXT,
    note       TEXT,
    actor_id   INTEGER NOT NULL REFERENCES users(id),
    created_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_rse_result ON result_status_events(result_id, id);
`

// immutableTriggers 为只追加的事实表创建禁止 UPDATE/DELETE 的触发器。
// 偏差不能靠改写记录消失：任何 UPDATE/DELETE 都被 SQLite 直接中止。
func immutableTriggers() string {
	tables := []string{
		"custody_events",
		"temperature_readings",
		"deviations",
		"deviation_dispositions",
		"result_versions",
		"raw_readings",
		"result_status_events",
		"registration_attempts",
	}
	var ddl string
	for _, t := range tables {
		ddl += `
CREATE TRIGGER IF NOT EXISTS trg_` + t + `_no_update BEFORE UPDATE ON ` + t + `
BEGIN
    SELECT RAISE(ABORT, '` + t + ` is append-only');
END;
CREATE TRIGGER IF NOT EXISTS trg_` + t + `_no_delete BEFORE DELETE ON ` + t + `
BEGIN
    SELECT RAISE(ABORT, '` + t + ` is append-only');
END;
`
	}
	return ddl
}
