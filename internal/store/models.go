package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// sampleRow 是样品当前态的事务内视图。
type sampleRow struct {
	ID            string
	BatchID       string
	ContainerID   string
	Status        string
	Hold          int
	HoldReason    string
	CurrentHolder string
	SealNo        string
	ResultVersion int
	Version       int
	CollectorID   string
	CollectorTeam string
}

func loadSample(tx *sql.Tx, id string) (sampleRow, error) {
	var s sampleRow
	err := tx.QueryRow(`SELECT id,batch_id,container_id,status,hold,hold_reason,
	                    current_holder_id,seal_no,result_version,version,collector_id,collector_team
	                    FROM samples WHERE id=?`, id).
		Scan(&s.ID, &s.BatchID, &s.ContainerID, &s.Status, &s.Hold, &s.HoldReason,
			&s.CurrentHolder, &s.SealNo, &s.ResultVersion, &s.Version,
			&s.CollectorID, &s.CollectorTeam)
	if errors.Is(err, sql.ErrNoRows) {
		return sampleRow{}, bizErr(ErrNotFound, "样品不存在")
	}
	return s, err
}

// loadSampleByCode 用脱敏批号加载（外部接口与下钻查询用）。
func loadSampleByCode(tx *sql.Tx, code string) (sampleRow, error) {
	var s sampleRow
	err := tx.QueryRow(`SELECT id,batch_id,container_id,status,hold,hold_reason,
	                    current_holder_id,seal_no,result_version,version,collector_id,collector_team
	                    FROM samples WHERE public_code=?`, code).
		Scan(&s.ID, &s.BatchID, &s.ContainerID, &s.Status, &s.Hold, &s.HoldReason,
			&s.CurrentHolder, &s.SealNo, &s.ResultVersion, &s.Version,
			&s.CollectorID, &s.CollectorTeam)
	if errors.Is(err, sql.ErrNoRows) {
		return sampleRow{}, bizErr(ErrNotFound, "样品不存在")
	}
	return s, err
}

type transferRow struct {
	ID            string
	SampleID      string
	ContainerID   string
	From          string
	To            string
	Code          string
	SealAtRelease int
	Status        string
}

// genReleaseCode 生成交出确认码（扫码内容，12 位 hex）。
func genReleaseCode() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
