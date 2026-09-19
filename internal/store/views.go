package store

// ExternalSampleView 是外部实验室可见的样品视图：只给脱敏批号，
// 不暴露内部实验室编号、采样班组、交接人、转运箱等身份信息。
type ExternalSampleView struct {
	MaskedBatch string              `json:"masked_batch"`
	Product     string              `json:"product"`
	PointCode   string              `json:"point_code"`
	Method      string              `json:"method"`
	Status      string              `json:"status"` // released / blocked / rejected / retest_pending
	Result      *ExternalResultView `json:"result,omitempty"`
}

// ExternalResultView 为外部可见的结果版本摘要（人员信息脱敏）。
type ExternalResultView struct {
	Version    int          `json:"version"`
	Supersedes int          `json:"supersedes_version,omitempty"`
	Status     string       `json:"status"`
	Conclusion string       `json:"conclusion,omitempty"`
	Readings   []RawReading `json:"readings"`
}

// ExternalViewByMaskedBatch 返回脱敏批号下、样品已具备完成复核结论的视图。
// 未复核完成、存在未结案偏差的样品以 blocked 状态出现，不给出检测数据。
func (d *DB) ExternalViewByMaskedBatch(masked string) ([]ExternalSampleView, error) {
	rows, err := d.sql.Query(`
SELECT b.code, b.product, p.code, m.code, s.id
FROM batches b
JOIN samples s ON s.batch_id = b.id
JOIN sampling_points p ON p.id = s.point_id
JOIN methods m ON m.id = s.method_id
ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	type row struct {
		code, product, point, method string
		sampleID                     int64
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.code, &r.product, &r.point, &r.method, &r.sampleID); err != nil {
			rows.Close()
			return nil, err
		}
		if maskedCode(r.code) == masked {
			rs = append(rs, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ExternalSampleView, 0, len(rs))
	for _, r := range rs {
		v := ExternalSampleView{
			MaskedBatch: masked, Product: r.product, PointCode: r.point,
			Method: r.method, Status: d.externalStatus(r.sampleID),
		}
		if v.Status == "released" {
			rv, err := d.currentResultView(r.sampleID)
			if err == nil {
				v.Result = rv
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// MaskedBatchOf 返回某内部批号对应的脱敏批号（供内部人员生成对外链接时使用）。
func (d *DB) MaskedBatchOf(batchCode string) (string, bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM batches WHERE code = ?`, batchCode).Scan(&n)
	if err != nil {
		return "", false, err
	}
	if n == 0 {
		return "", false, nil
	}
	return maskedCode(batchCode), true, nil
}

func (d *DB) externalStatus(sampleID int64) string {
	var openDev, rejected int
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM deviations v WHERE v.sample_id = ?
AND NOT EXISTS (SELECT 1 FROM deviation_dispositions x WHERE x.deviation_id = v.id)`,
		sampleID).Scan(&openDev)
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM deviations v
JOIN deviation_dispositions x ON x.deviation_id = v.id
WHERE v.sample_id = ? AND x.decision = 'reject_sample'`, sampleID).Scan(&rejected)
	switch {
	case rejected > 0:
		return "rejected"
	case openDev > 0:
		return "blocked"
	}
	rv, err := d.currentResultView(sampleID)
	if err != nil || rv == nil {
		return "retest_pending"
	}
	if rv.Status == "reviewed" {
		return "released"
	}
	return "retest_pending"
}

func (d *DB) currentResultView(sampleID int64) (*ExternalResultView, error) {
	var resultID int64
	err := d.sql.QueryRow(`SELECT current_result_id FROM samples WHERE id = ? AND current_result_id IS NOT NULL`,
		sampleID).Scan(&resultID)
	if err != nil {
		return nil, err
	}
	full, err := d.GetResult(resultID)
	if err != nil {
		return nil, err
	}
	v := &ExternalResultView{
		Version: full.VersionNo, Status: full.Status,
		Conclusion: full.Conclusion, Readings: full.Readings,
	}
	if full.SupersedesID != nil {
		var oldNo int
		if err := d.sql.QueryRow(`SELECT version_no FROM result_versions WHERE id = ?`,
			*full.SupersedesID).Scan(&oldNo); err == nil {
			v.Supersedes = oldNo
		}
	}
	return v, nil
}

// ApprovalItem 是试车审批人看到的一条放行依据。
type ApprovalItem struct {
	LabNo          string `json:"lab_no"`
	PointCode      string `json:"point_code"`
	MethodCode     string `json:"method_code"`
	OpenDeviations int    `json:"open_deviations"`
	ResultVersion  int    `json:"result_version"`
	ResultStatus   string `json:"result_status"`
	Conclusion     string `json:"conclusion,omitempty"`
	Releasable     bool   `json:"releasable"`
	Reason         string `json:"reason,omitempty"`
}

// ApprovalView 返回试车批次的放行视图。
// 审批人只能读取“完成复核(reviewed)”且当前采用的结论；未复核完成或存在
// 未处置偏差的样品不会成为放行依据。
func (d *DB) ApprovalView(batchCode string) ([]ApprovalItem, error) {
	samples, err := d.SamplesByBatch(batchCode)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalItem, 0, len(samples))
	for _, s := range samples {
		item := ApprovalItem{
			LabNo: s.LabNo, PointCode: s.PointCode, MethodCode: s.MethodCode,
			OpenDeviations: s.OpenDeviationCount,
		}
		switch {
		case s.OpenDeviationCount > 0:
			item.Reason = "存在未处置偏差，样品已暂停使用"
		case d.hasRejectDisposition(s.ID):
			item.Reason = "样品已被判定拒收"
		case s.CurrentResultID == nil:
			item.Reason = "尚无完成复核并采用的结果版本"
		default:
			rv, err := d.GetResult(*s.CurrentResultID)
			if err != nil {
				return nil, err
			}
			item.ResultVersion = rv.VersionNo
			item.ResultStatus = rv.Status
			item.Conclusion = rv.Conclusion
			if rv.Status != "reviewed" {
				item.Reason = "当前采用版本未完成复核"
			} else {
				item.Releasable = true
			}
		}
		out = append(out, item)
	}
	return out, nil
}
