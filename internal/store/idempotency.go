package store

import "database/sql"

// existingIdemEvent 在事务内查找同一客户端幂等键此前写入的交接事件。
// 扫码设备离线恢复后重发事件时据此短路，保证链路中不会出现重复事件
// 或重复保管人。
func existingIdemEvent(tx *sql.Tx, sampleID int64, idemKey string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM custody_events
WHERE sample_id = ? AND idem_key = ?`, sampleID, idemKey).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return id, nil
}

// EventByIdemKey 返回离线事件首次同步时生成的交接事件（只读场景使用）。
func (d *DB) EventByIdemKey(sampleID int64, idemKey string) (*CustodyEvent, error) {
	var id int64
	err := d.sql.QueryRow(`SELECT id FROM custody_events
WHERE sample_id = ? AND idem_key = ?`, sampleID, idemKey).Scan(&id)
	if err != nil {
		return nil, ErrNotFound
	}
	return d.GetCustodyEvent(id)
}

// SampleIDByLabNo 用于幂等重放前定位样品。
func (d *DB) SampleIDByLabNo(labNo string) (int64, error) {
	var id int64
	if err := d.sql.QueryRow(`SELECT id FROM samples WHERE lab_no = ?`, labNo).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}
