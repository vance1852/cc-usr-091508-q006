package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// 本文件包含：写操作幂等台账、保管事件、偏差开启/计数辅助。

// ErrOpReplay 表示幂等键命中且载荷一致：调用方应返回首次执行结果而非再次执行。
var ErrOpReplay = errors.New("idempotent replay")

// claimOp 在事务开始处抢占幂等键。
//   - 未命中：插入 ops 行后继续执行业务（若业务失败回滚，op 行一并回滚）；
//   - 命中且四元组一致：返回 ErrOpReplay —— 业务方法据此回读已存在资源并返回
//     首次执行的结果，保证离线扫码恢复重放不产生重复事件；
//   - 命中但发起人/类型/载荷不同：返回冲突，防止同键换载荷。
func claimOp(tx *sql.Tx, opID, actorID, opType string, payload any) error {
	if opID == "" {
		return bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	h := hashOf(payload)
	_, err := tx.Exec(`INSERT INTO ops(op_id,actor_id,op_type,req_hash) VALUES(?,?,?,?)`,
		opID, actorID, opType, h)
	if err == nil {
		return nil
	}
	if !isUniqueErr(err) {
		return err
	}
	var gotActor, gotType, gotHash string
	if err := tx.QueryRow(`SELECT actor_id,op_type,req_hash FROM ops WHERE op_id=?`, opID).
		Scan(&gotActor, &gotType, &gotHash); err != nil {
		return err
	}
	if gotActor != actorID || gotType != opType || gotHash != h {
		return bizErr(ErrReplay, "幂等键 %s 已用于不同操作", opID)
	}
	return ErrOpReplay
}

// opRef 读取首次执行记录的资源标识（重放路径用）。
func opRef(tx *sql.Tx, opID string) (string, error) {
	var ref sql.NullString
	if err := tx.QueryRow(`SELECT ref_id FROM ops WHERE op_id=?`, opID).Scan(&ref); err != nil {
		return "", err
	}
	return ref.String, nil
}

// hashOf 计算载荷的稳定哈希：结构体按 JSON 编码后 SHA-256。
func hashOf(v any) string {
	b := mustMarshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------- 保管事件 ----------

// appendCustody 追加保管事件，seq 在样品内自动递增。
func appendCustody(tx *sql.Tx, sampleID, kind, actorID, team, transferID, note string) error {
	_, err := tx.Exec(`INSERT INTO custody_events(sample_id,seq,kind,actor_id,actor_team,transfer_id,note)
	                    VALUES(?,(SELECT COALESCE(MAX(seq),0)+1 FROM custody_events WHERE sample_id=?),
	                           ?,?,?,?,?)`,
		sampleID, sampleID, kind, actorID, team, nullable(transferID), note)
	return err
}

// appendCustodyEx 追加带温度/封签事实的接收事件。
func appendCustodyEx(tx *sql.Tx, sampleID, kind, actorID, team, transferID, note string,
	sealOK int, hasTemp bool, tmin, tmax float64) error {
	var smin, smax interface{}
	if !hasTemp {
		smin, smax = nil, nil
	} else {
		smin, smax = tmin, tmax
	}
	_, err := tx.Exec(`INSERT INTO custody_events
		(sample_id,seq,kind,actor_id,actor_team,transfer_id,note,seal_ok,temp_min,temp_max)
		VALUES(?,(SELECT COALESCE(MAX(seq),0)+1 FROM custody_events WHERE sample_id=?),
		        ?,?,?,?,?,?,?,?)`,
		sampleID, sampleID, kind, actorID, team, nullable(transferID), note, sealOK, smin, smax)
	return err
}

// ---------- 偏差 ----------

type deviationInput struct {
	OpID       string
	SampleID   string
	TransferID string
	Kind       string
	Title      string
	Detail     string
	OpenedBy   string
	OpenedTeam string
}

// openDeviationTx 开启偏差（append-only），追加 open 处置事件，并把样品置为暂停。
func openDeviationTx(tx *sql.Tx, in deviationInput) (int64, error) {
	res, err := tx.Exec(`INSERT INTO deviations
		(op_id,sample_id,transfer_id,kind,title,detail,opened_by,opened_team,status)
		VALUES(?,?,?,?,?,?,?,?,'open')`,
		in.OpID, in.SampleID, nullable(in.TransferID), in.Kind, in.Title, in.Detail,
		in.OpenedBy, in.OpenedTeam)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO deviation_events(deviation_id,action,actor_id,actor_team,note)
	                      VALUES(?,'open',?,?,?)`,
		id, in.OpenedBy, in.OpenedTeam, in.Title); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE samples SET hold=1, hold_reason=? WHERE id=?`,
		in.Title, in.SampleID); err != nil {
		return 0, err
	}
	return id, nil
}

// countOpenDeviations 返回样品未结案偏差数；为 0 时才能解除暂停。
func countOpenDeviations(tx *sql.Tx, sampleID string) (int, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM deviations WHERE sample_id=? AND status='open'`,
		sampleID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
