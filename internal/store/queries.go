package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// GetSample 按 ID 读取样品及当前派生状态。
func (d *DB) GetSample(id int64) (*Sample, error) {
	return d.getSample(`WHERE s.id = ?`, id)
}

// GetSampleByLabNo 按实验室编号读取样品。
func (d *DB) GetSampleByLabNo(labNo string) (*Sample, error) {
	return d.getSample(`WHERE s.lab_no = ?`, labNo)
}

const sampleSelect = `
SELECT s.id, s.batch_id, s.shipment_id, s.lab_no, p.code, m.code,
       COALESCE(c.code,''), s.seal_no, s.sampled_at, s.crew_id, s.registered_by,
       s.created_at, s.temp_min, s.temp_max, s.max_gap_minutes,
       s.current_custodian_id, COALESCE(cu.login,''), s.custodian_seq,
       s.current_result_id, s.consumed_at
FROM samples s
JOIN sampling_points p ON p.id = s.point_id
JOIN methods m ON m.id = s.method_id
LEFT JOIN containers c ON c.id =
    (SELECT container_id FROM container_bindings WHERE sample_id = s.id
     ORDER BY released_at IS NOT NULL, id DESC LIMIT 1)
LEFT JOIN users cu ON cu.id = s.current_custodian_id
`

func (d *DB) getSample(where string, args ...any) (*Sample, error) {
	row := d.sql.QueryRow(sampleSelect+where, args...)
	var s Sample
	var containerCode, custodianLogin, consumed sql.NullString
	var curCust, curResult sql.NullInt64
	var tMin, tMax sql.NullFloat64
	if err := row.Scan(&s.ID, &s.BatchID, &s.ShipmentID, &s.LabNo, &s.PointCode,
		&s.MethodCode, &containerCode, &s.SealNo, &s.SampledAt, &s.CrewCode,
		&s.RegisteredBy, &s.CreatedAt, &tMin, &tMax, &s.MaxGapMinutes,
		&curCust, &custodianLogin, &s.CustodianSeq, &curResult, &consumed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.ContainerCode = containerCode.String
	s.CurrentCustodian = custodianLogin.String
	s.ConsumedAt = consumed.String
	if tMin.Valid {
		v := tMin.Float64
		s.TempMin = &v
	}
	if tMax.Valid {
		v := tMax.Float64
		s.TempMax = &v
	}
	if curCust.Valid {
		v := curCust.Int64
		s.CurrentCustodianID = &v
	}
	if curResult.Valid {
		v := curResult.Int64
		s.CurrentResultID = &v
	}
	s.OpenDeviationCount, _ = d.openDeviationCount(s.ID)
	s.Usable = s.OpenDeviationCount == 0 && !d.hasRejectDisposition(s.ID)
	return &s, nil
}

func (d *DB) openDeviationCount(sampleID int64) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM deviations v WHERE v.sample_id = ?
AND NOT EXISTS (SELECT 1 FROM deviation_dispositions x WHERE x.deviation_id = v.id)`,
		sampleID).Scan(&n)
	return n, err
}

func (d *DB) hasRejectDisposition(sampleID int64) bool {
	var n int
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM deviations v
JOIN deviation_dispositions x ON x.deviation_id = v.id
WHERE v.sample_id = ? AND x.decision = 'reject_sample'`, sampleID).Scan(&n)
	return n > 0
}

// SamplesByBatch 列出一个试车批次下的全部样品。
func (d *DB) SamplesByBatch(batchCode string) ([]Sample, error) {
	var batchID int64
	err := d.sql.QueryRow(`SELECT id FROM batches WHERE code = ?`, batchCode).Scan(&batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 批次 %s 不存在", ErrNotFound, batchCode)
	}
	if err != nil {
		return nil, err
	}
	ids, err := d.scalarIDs(`SELECT id FROM samples WHERE batch_id = ? ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(ids))
	for _, id := range ids {
		s, err := d.GetSample(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

// 占位：使用 database/sql 原生接口。
type rowx struct{}

func (d *DB) scalarIDs(query string, args ...any) ([]int64, error) {
	rows, err := d.sql.Query(query, args...)
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
	return ids, rows.Err()
}

// BatchTrace 是评审下钻视图：批次下逐站核对去向、异常处置与采用的结果版本。
type BatchTrace struct {
	BatchCode string        `json:"batch_code"`
	Product   string        `json:"product"`
	Samples   []SampleTrace `json:"samples"`
}

// SampleTrace 为单个样品的完整链路。
type SampleTrace struct {
	Sample       Sample               `json:"sample"`
	Custody      []CustodyEvent       `json:"custody_chain"`
	Temperatures []TemperatureReading `json:"temperatures"`
	Deviations   []Deviation          `json:"deviations"`
	Results      []ResultVersion      `json:"result_versions"`
	ChainOK      bool                 `json:"chain_intact"`
}

// TraceBatch 返回一个试车批次的完整样品链路，供评审人员逐站核对。
func (d *DB) TraceBatch(batchCode string) (*BatchTrace, error) {
	var product string
	err := d.sql.QueryRow(`SELECT product FROM batches WHERE code = ?`, batchCode).Scan(&product)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 批次 %s 不存在", ErrNotFound, batchCode)
	}
	if err != nil {
		return nil, err
	}
	samples, err := d.SamplesByBatch(batchCode)
	if err != nil {
		return nil, err
	}
	t := &BatchTrace{BatchCode: batchCode, Product: product}
	for i := range samples {
		st, err := d.TraceSample(samples[i].ID)
		if err != nil {
			return nil, err
		}
		t.Samples = append(t.Samples, *st)
	}
	return t, nil
}

// TraceSample 返回单个样品的完整链路。
func (d *DB) TraceSample(sampleID int64) (*SampleTrace, error) {
	s, err := d.GetSample(sampleID)
	if err != nil {
		return nil, err
	}
	chain, err := d.CustodyChain(sampleID)
	if err != nil {
		return nil, err
	}
	temps, err := d.Temperatures(sampleID)
	if err != nil {
		return nil, err
	}
	devs, err := d.Deviations(sampleID)
	if err != nil {
		return nil, err
	}
	results, err := d.ResultsOfSample(sampleID)
	if err != nil {
		return nil, err
	}
	return &SampleTrace{
		Sample:       *s,
		Custody:      chain,
		Temperatures: temps,
		Deviations:   devs,
		Results:      results,
		ChainOK:      VerifyChain(chain),
	}, nil
}

// CustodyChain 按顺序返回样品的全部交接事件。
func (d *DB) CustodyChain(sampleID int64) ([]CustodyEvent, error) {
	rows, err := d.sql.Query(`
SELECT e.id, e.sample_id, e.seq, e.kind, COALESCE(e.station,''),
       e.from_user_id, e.to_user_id,
       COALESCE(fu.login,''), COALESCE(tu.login,''),
       e.actor_id, e.seal_intact, COALESCE(e.expected_seal_no,''),
       e.transfer_id, COALESCE(e.client_event_time,''), COALESCE(e.idem_key,''),
       COALESCE(e.note,''), e.recorded_at, e.prev_hash, e.hash
FROM custody_events e
LEFT JOIN users fu ON fu.id = e.from_user_id
LEFT JOIN users tu ON tu.id = e.to_user_id
WHERE e.sample_id = ? ORDER BY e.seq`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustodyEvent
	for rows.Next() {
		var e CustodyEvent
		var station, fromLogin, toLogin, expectedSeal, clientTime, idemKey, note sql.NullString
		var fromID, toID, transferID sql.NullInt64
		var sealIntact sql.NullInt64
		if err := rows.Scan(&e.ID, &e.SampleID, &e.Seq, &e.Kind, &station,
			&fromID, &toID, &fromLogin, &toLogin, &e.ActorID, &sealIntact,
			&expectedSeal, &transferID, &clientTime, &idemKey, &note,
			&e.RecordedAt, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		e.Station = station.String
		e.FromUser = fromLogin.String
		e.ToUser = toLogin.String
		e.ExpectedSealNo = expectedSeal.String
		e.ClientEventTime = clientTime.String
		e.IdemKey = idemKey.String
		e.Note = note.String
		if fromID.Valid {
			v := fromID.Int64
			e.FromUserID = &v
		}
		if toID.Valid {
			v := toID.Int64
			e.ToUserID = &v
		}
		if transferID.Valid {
			v := transferID.Int64
			e.TransferID = &v
		}
		if sealIntact.Valid {
			b := sealIntact.Int64 == 1
			e.SealIntact = &b
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetCustodyEvent 按 ID 读取单个交接事件。
func (d *DB) GetCustodyEvent(id int64) (*CustodyEvent, error) {
	var sampleID int64
	err := d.sql.QueryRow(`SELECT sample_id FROM custody_events WHERE id = ?`, id).Scan(&sampleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	chain, err := d.CustodyChain(sampleID)
	if err != nil {
		return nil, err
	}
	for i := range chain {
		if chain[i].ID == id {
			return &chain[i], nil
		}
	}
	return nil, ErrNotFound
}

// GetCustodyEventBySample 返回样品链上最新事件。
func (d *DB) GetCustodyEventBySample(sampleID int64) (*CustodyEvent, error) {
	chain, err := d.CustodyChain(sampleID)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, ErrNotFound
	}
	return &chain[len(chain)-1], nil
}

// VerifyChain 重算哈希链：每个事件的 prev_hash 必须承接上一事件 hash，
// 且本事件 hash 必须与重算值一致。任一不符即断链，说明历史被物理改动。
func VerifyChain(chain []CustodyEvent) bool {
	prev := "GENESIS"
	for _, e := range chain {
		if e.PrevHash != prev {
			return false
		}
		want := chainHash(prev,
			fmt.Sprint(e.SampleID), fmt.Sprint(e.Seq), e.Kind, e.Station,
			ptrSprint(e.FromUserID), ptrSprint(e.ToUserID), fmt.Sprint(e.ActorID),
			boolPtrSprint(e.SealIntact), e.ExpectedSealNo, ptrSprint(e.TransferID),
			e.ClientEventTime, e.IdemKey, e.Note)
		if want != e.Hash {
			return false
		}
		prev = e.Hash
	}
	return true
}
