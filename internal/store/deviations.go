package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// insertDeviation 在事务内追加一条偏差（按 样品+类型+去重键 幂等）。
func insertDeviation(tx *sql.Tx, sampleID *int64, typ, dedupKey, detail string,
	refEvent *int64, attempt *int64, openedBy int64) error {
	_, err := tx.Exec(`INSERT INTO deviations
(sample_id, type, dedup_key, detail, ref_event_id, attempt_id, opened_by, opened_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(sample_id, type, dedup_key) DO NOTHING`,
		sampleID, typ, dedupKey, detail, refEvent, attempt, openedBy, ts())
	return mapExecError(err)
}

// requireUsable 校验样品是否可继续使用：
// 存在未结案偏差，或曾被判定 reject_sample，都将暂停/终止后续使用。
// 偏差不能靠改写记录消失，因此该判定无法被绕过。
func requireUsable(tx *sql.Tx, sampleID int64) error {
	var openCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM deviations d
WHERE d.sample_id = ? AND NOT EXISTS (
    SELECT 1 FROM deviation_dispositions x WHERE x.deviation_id = d.id)`,
		sampleID).Scan(&openCount); err != nil {
		return err
	}
	if openCount > 0 {
		return fmt.Errorf("%w: 样品存在 %d 条未处置偏差，已暂停后续使用", ErrConflict, openCount)
	}
	var rejected int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM deviations d
JOIN deviation_dispositions x ON x.deviation_id = d.id
WHERE d.sample_id = ? AND x.decision = 'reject_sample'`,
		sampleID).Scan(&rejected); err != nil {
		return err
	}
	if rejected > 0 {
		return fmt.Errorf("%w: 样品已被判定拒收，禁止继续使用", ErrConflict)
	}
	return nil
}

// DispositionInput 为偏差处置请求。
type DispositionInput struct {
	DeviationID   int64  `json:"deviation_id"`
	Decision      string `json:"decision"`
	Justification string `json:"justification"`
}

// DisposeDeviation 由复核人对偏差作出结案处置。处置只追加，不能改写既有处置。
func (d *DB) DisposeDeviation(actor User, in DispositionInput) (*Disposition, error) {
	if actor.Role != "reviewer" && actor.Role != "admin" {
		return nil, fmt.Errorf("%w: 只有复核人可以处置偏差", ErrForbidden)
	}
	switch in.Decision {
	case "reject_sample", "retest", "accept_justified":
	default:
		return nil, fmt.Errorf("%w: decision 必须是 reject_sample/retest/accept_justified", ErrValidation)
	}
	if in.Justification == "" {
		return nil, fmt.Errorf("%w: 处置必须填写 justification", ErrValidation)
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var devID int64
	var sampleID sql.NullInt64
	var already int
	err = tx.QueryRow(`SELECT v.id, v.sample_id,
(SELECT COUNT(*) FROM deviation_dispositions x WHERE x.deviation_id = v.id)
FROM deviations v WHERE v.id = ?`, in.DeviationID).Scan(&devID, &sampleID, &already)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 偏差 %d 不存在", ErrNotFound, in.DeviationID)
	}
	if err != nil {
		return nil, err
	}
	if already > 0 {
		return nil, fmt.Errorf("%w: 偏差已有结案处置，处置不可改写", ErrConflict)
	}
	// 采样班组不能处置自己样品的偏差（职责分离）。
	if sampleID.Valid && actor.CrewCode != "" {
		var crew string
		if err := tx.QueryRow(`SELECT crew_id FROM samples WHERE id = ?`,
			sampleID.Int64).Scan(&crew); err != nil {
			return nil, err
		}
		if crew == actor.CrewCode {
			return nil, fmt.Errorf("%w: 采样班组不能处置本班组样品的偏差", ErrForbidden)
		}
	}

	res, err := tx.Exec(`INSERT INTO deviation_dispositions
(deviation_id, decision, justification, actor_id, decided_at)
VALUES(?,?,?,?,?)`, in.DeviationID, in.Decision, in.Justification, actor.ID, ts())
	if err != nil {
		return nil, mapExecError(err)
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	return d.GetDisposition(id)
}

// GetDisposition 读取单条处置。
func (d *DB) GetDisposition(id int64) (*Disposition, error) {
	row := d.sql.QueryRow(`SELECT id, deviation_id, decision, justification, actor_id, decided_at
FROM deviation_dispositions WHERE id = ?`, id)
	var x Disposition
	if err := row.Scan(&x.ID, &x.DeviationID, &x.Decision, &x.Justification,
		&x.ActorID, &x.DecidedAt); err != nil {
		return nil, err
	}
	return &x, nil
}

// Deviations 列出偏差（可按样品过滤），附最新处置状态。
func (d *DB) Deviations(sampleID int64) ([]Deviation, error) {
	q := `SELECT v.id, v.sample_id, v.type, v.detail, v.opened_by, v.opened_at,
COALESCE(x.id,0), COALESCE(x.decision,''), COALESCE(x.justification,''),
COALESCE(x.actor_id,0), COALESCE(x.decided_at,'')
FROM deviations v
LEFT JOIN deviation_dispositions x ON x.deviation_id = v.id`
	args := []any{}
	if sampleID > 0 {
		q += ` WHERE v.sample_id = ?`
		args = append(args, sampleID)
	}
	q += ` ORDER BY v.id`
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deviation
	for rows.Next() {
		var v Deviation
		var sid sql.NullInt64
		var xid, xactor int64
		var decision, just, decided string
		if err := rows.Scan(&v.ID, &sid, &v.Type, &v.Detail, &v.OpenedBy, &v.OpenedAt,
			&xid, &decision, &just, &xactor, &decided); err != nil {
			return nil, err
		}
		v.SampleID = sid.Int64
		if xid != 0 {
			v.Open = false
			v.Disposition = &Disposition{
				ID: xid, DeviationID: v.ID, Decision: decision,
				Justification: just, ActorID: xactor, DecidedAt: decided,
			}
		} else {
			v.Open = true
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
