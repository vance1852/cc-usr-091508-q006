package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SubmitResultInput 为提交一版检测结果的请求。
type SubmitResultInput struct {
	LabNo        string            `json:"lab_no"`
	MethodCode   string            `json:"method_code"`
	Instrument   string            `json:"instrument"`
	Note         string            `json:"note"`
	SupersedesID int64             `json:"supersedes_id"` // 复测时引用的旧版本；旧版本不被改动
	Readings     []RawReadingInput `json:"readings"`
}

// RawReadingInput 为原始读数输入。
type RawReadingInput struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// SubmitResult 由化验员提交一版检测结果（含原始读数）。
// 结果一经提交即不可变；纠正只能再提交一个 supersedes 旧版本的新版本。
// 样品存在未处置偏差时不允许提交（偏差暂停后续使用）。
func (d *DB) SubmitResult(actor User, in SubmitResultInput) (*ResultVersion, error) {
	if actor.Role != "analyst" && actor.Role != "admin" {
		return nil, fmt.Errorf("%w: 只有化验员可以提交检测结果", ErrForbidden)
	}
	if len(in.Readings) == 0 {
		return nil, fmt.Errorf("%w: 至少一条原始读数", ErrValidation)
	}
	for _, r := range in.Readings {
		if strings.TrimSpace(r.Name) == "" {
			return nil, fmt.Errorf("%w: 原始读数名称不能为空", ErrValidation)
		}
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	s, err := lockSampleByLabNo(tx, in.LabNo)
	if err != nil {
		return nil, err
	}
	if err := requireUsable(tx, s.ID); err != nil {
		return nil, err
	}
	// 检测只能由当前持有样品的化验员执行（管理员可代运维操作）。
	if actor.Role != "admin" {
		if s.CurrentCustodianID == nil || *s.CurrentCustodianID != actor.ID {
			return nil, fmt.Errorf("%w: 样品当前不由你保管，不能提交检测结果", ErrForbidden)
		}
	}
	var methodID int64
	if err := tx.QueryRow(`SELECT id FROM methods WHERE code = ?`, in.MethodCode).Scan(&methodID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 检测方法 %s 不存在", ErrNotFound, in.MethodCode)
		}
		return nil, err
	}
	if in.SupersedesID != 0 {
		var oldSample int64
		var oldVerNo int
		err := tx.QueryRow(`SELECT sample_id, version_no FROM result_versions WHERE id = ?`,
			in.SupersedesID).Scan(&oldSample, &oldVerNo)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 被引用的旧版本 %d 不存在", ErrNotFound, in.SupersedesID)
		}
		if err != nil {
			return nil, err
		}
		if oldSample != s.ID {
			return nil, fmt.Errorf("%w: 复测版本只能引用同一样品的旧版本", ErrValidation)
		}
	}

	var verNo int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version_no),0)+1 FROM result_versions
WHERE sample_id = ?`, s.ID).Scan(&verNo); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO result_versions
(sample_id, version_no, method_id, instrument, analyst_id, supersedes_id, submitted_at, note)
VALUES(?,?,?,?,?,?,?,?)`,
		s.ID, verNo, methodID, in.Instrument, actor.ID, nullZero(in.SupersedesID), ts(), in.Note)
	if err != nil {
		return nil, mapExecError(err)
	}
	resultID, _ := res.LastInsertId()
	for _, r := range in.Readings {
		if _, err := tx.Exec(`INSERT INTO raw_readings(result_id, name, value, unit, recorded_at)
VALUES(?,?,?,?,?)`, resultID, r.Name, r.Value, r.Unit, ts()); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO result_status_events(result_id, status, actor_id, created_at)
VALUES(?, 'submitted', ?, ?)`, resultID, actor.ID, ts()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	return d.GetResult(resultID)
}

func nullZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ReviewResult 由复核人批准或驳回一版结果。
// 职责分离：检测人不能复核自己的结果；采样班组成员不能批准本班组样品的检测。
// 只有“已提交待复核”的版本可复核；已发布版本不可改状态（纠正只能另出复测版本）。
func (d *DB) ReviewResult(actor User, resultID int64, approve bool, conclusion, note string) (*ResultVersion, error) {
	if actor.Role != "reviewer" && actor.Role != "admin" {
		return nil, fmt.Errorf("%w: 只有复核人可以复核检测结果", ErrForbidden)
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sampleID, analystID int64
	if err := tx.QueryRow(`SELECT sample_id, analyst_id FROM result_versions WHERE id = ?`,
		resultID).Scan(&sampleID, &analystID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 结果版本 %d 不存在", ErrNotFound, resultID)
		}
		return nil, err
	}
	if analystID == actor.ID {
		return nil, fmt.Errorf("%w: 检测人不能复核自己提交的结果", ErrForbidden)
	}
	var crew string
	if err := tx.QueryRow(`SELECT crew_id FROM samples WHERE id = ?`, sampleID).Scan(&crew); err != nil {
		return nil, err
	}
	if actor.CrewCode != "" && crew == actor.CrewCode {
		return nil, fmt.Errorf("%w: 采样班组不能批准自己采集样品的检测", ErrForbidden)
	}

	status, err := currentStatus(tx, resultID)
	if err != nil {
		return nil, err
	}
	if status != "submitted" {
		return nil, fmt.Errorf("%w: 结果版本当前状态为 %s，不能重复复核（纠正只能另出复测版本）",
			ErrConflict, status)
	}

	newStatus := "reviewed"
	if !approve {
		newStatus = "rejected"
	}
	if _, err := tx.Exec(`INSERT INTO result_status_events
(result_id, status, conclusion, note, actor_id, created_at)
VALUES(?,?,?,?,?,?)`, resultID, newStatus, conclusion, note, actor.ID, ts()); err != nil {
		return nil, err
	}
	if approve {
		// 完成复核的结论才成为样品当前采用版本，供试车审批读取。
		if _, err := tx.Exec(`UPDATE samples SET current_result_id = ? WHERE id = ?`,
			resultID, sampleID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	return d.GetResult(resultID)
}

func currentStatus(tx *sql.Tx, resultID int64) (string, error) {
	var status string
	err := tx.QueryRow(`SELECT status FROM result_status_events WHERE result_id = ?
ORDER BY id DESC LIMIT 1`, resultID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: 结果没有状态事件", ErrConflict)
	}
	return status, err
}

// GetResult 读取一个结果版本（含原始读数与当前状态）。
func (d *DB) GetResult(id int64) (*ResultVersion, error) {
	var rv ResultVersion
	var methodCode string
	var analystID int64
	var analystLogin, analystName string
	var supersedes sql.NullInt64
	var instrument, note sql.NullString
	var conclusion sql.NullString
	err := d.sql.QueryRow(`SELECT rv.id, rv.sample_id, rv.version_no, m.code,
rv.instrument, rv.analyst_id, u.login, u.display_name, rv.supersedes_id, rv.submitted_at, rv.note,
(SELECT status FROM result_status_events WHERE result_id = rv.id ORDER BY id DESC LIMIT 1),
(SELECT conclusion FROM result_status_events WHERE result_id = rv.id ORDER BY id DESC LIMIT 1)
FROM result_versions rv
JOIN methods m ON m.id = rv.method_id
JOIN users u ON u.id = rv.analyst_id
WHERE rv.id = ?`, id).Scan(
		&rv.ID, &rv.SampleID, &rv.VersionNo, &methodCode, &instrument,
		&analystID, &analystLogin, &analystName, &supersedes, &rv.SubmittedAt, &note,
		&rv.Status, &conclusion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 结果版本 %d 不存在", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	rv.MethodCode = methodCode
	rv.Instrument = instrument.String
	rv.AnalystID = analystID
	rv.Analyst = analystName + " (" + analystLogin + ")"
	rv.SupersedesID = nil
	if supersedes.Valid {
		v := supersedes.Int64
		rv.SupersedesID = &v
	}
	rv.Note = note.String
	rv.Conclusion = conclusion.String
	readings, err := d.readingsOf(id)
	if err != nil {
		return nil, err
	}
	rv.Readings = readings
	return &rv, nil
}

func (d *DB) readingsOf(resultID int64) ([]RawReading, error) {
	rows, err := d.sql.Query(`SELECT id, name, value, COALESCE(unit,''), recorded_at
FROM raw_readings WHERE result_id = ? ORDER BY id`, resultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawReading
	for rows.Next() {
		var r RawReading
		if err := rows.Scan(&r.ID, &r.Name, &r.Value, &r.Unit, &r.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResultsOfSample 列出样品的全部结果版本（按版本号）。
func (d *DB) ResultsOfSample(sampleID int64) ([]ResultVersion, error) {
	rows, err := d.sql.Query(`SELECT id FROM result_versions
WHERE sample_id = ? ORDER BY version_no`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ResultVersion, 0, len(ids))
	for _, id := range ids {
		rv, err := d.GetResult(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rv)
	}
	return out, nil
}
