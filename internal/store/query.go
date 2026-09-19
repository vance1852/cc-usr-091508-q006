package store

import "database/sql"

// ChainView 是评审人从试车批次下钻时看到的完整链路。
type ChainView struct {
	BatchID   string         `json:"batch_id"`
	BatchName string         `json:"batch_name"`
	Product   string         `json:"product"`
	Stations  []StationChain `json:"stations"`
}

type StationChain struct {
	PointID string        `json:"point_id"`
	Name    string        `json:"name"`
	Samples []SampleChain `json:"samples"`
}

type SampleChain struct {
	SampleID      string             `json:"sample_id"`
	PublicCode    string             `json:"public_code"`
	Status        string             `json:"status"`
	Hold          bool               `json:"hold"`
	HoldReason    string             `json:"hold_reason"`
	CurrentHolder PersonRef          `json:"current_holder"`
	Version       int                `json:"version"`
	AdoptedResult int                `json:"adopted_result_version"`
	Custody       []CustodyEventDTO  `json:"custody"`
	Deviations    []DeviationDTO     `json:"deviations"`
	Results       []ResultVersionDTO `json:"results"`
}

type PersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Team string `json:"team"`
	Role string `json:"role"`
}

type CustodyEventDTO struct {
	Seq        int       `json:"seq"`
	Kind       string    `json:"kind"`
	Actor      PersonRef `json:"actor"`
	Note       string    `json:"note"`
	SealOK     *bool     `json:"seal_ok,omitempty"`
	TempMin    *float64  `json:"temp_min,omitempty"`
	TempMax    *float64  `json:"temp_max,omitempty"`
	TransferID string    `json:"transfer_id,omitempty"`
	At         string    `json:"at"`
}

type DeviationDTO struct {
	ID       int64               `json:"id"`
	Kind     string              `json:"kind"`
	Title    string              `json:"title"`
	Detail   string              `json:"detail"`
	Status   string              `json:"status"`
	OpenedBy PersonRef           `json:"opened_by"`
	OpenedAt string              `json:"opened_at"`
	ClosedAt string              `json:"closed_at,omitempty"`
	Events   []DeviationEventDTO `json:"events"`
}

type DeviationEventDTO struct {
	Action string    `json:"action"`
	Actor  PersonRef `json:"actor"`
	Note   string    `json:"note"`
	At     string    `json:"at"`
}

type ResultVersionDTO struct {
	Version           int        `json:"version"`
	MethodName        string     `json:"method"`
	RawReading        string     `json:"raw_reading"`
	RawUnit           string     `json:"raw_unit"`
	Purity            float64    `json:"purity"`
	SupersedesVersion int        `json:"supersedes_version"`
	Status            string     `json:"status"`
	ReviewNote        string     `json:"review_note,omitempty"`
	IsRetest          bool       `json:"is_retest"`
	IsExternal        bool       `json:"is_external"`
	Analyst           PersonRef  `json:"analyst"`
	Reviewer          *PersonRef `json:"reviewer,omitempty"`
}

func personRef(tx *sql.Tx, id string) (PersonRef, error) {
	var p PersonRef
	err := tx.QueryRow(`SELECT id,name,team,role FROM people WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.Team, &p.Role)
	return p, err
}

// BatchChain 组装批次下钻视图：逐站 -> 逐样品 -> 保管去向 / 偏差处置 / 结果版本。
// 只读查询，走单连接，不需要显式事务。
func (s *Store) BatchChain(batchID string) (ChainView, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ChainView{}, err
	}
	defer tx.Rollback()

	view := ChainView{BatchID: batchID}
	if err := tx.QueryRow(`SELECT id,name,product FROM batches WHERE id=?`, batchID).
		Scan(&view.BatchID, &view.BatchName, &view.Product); err != nil {
		if err == sql.ErrNoRows {
			return ChainView{}, bizErr(ErrNotFound, "批次不存在")
		}
		return ChainView{}, err
	}

	pRows, err := tx.Query(`SELECT id,name FROM sampling_points WHERE batch_id=? ORDER BY name`, batchID)
	if err != nil {
		return ChainView{}, err
	}
	type pt struct{ id, name string }
	var points []pt
	for pRows.Next() {
		var p pt
		if err := pRows.Scan(&p.id, &p.name); err != nil {
			pRows.Close()
			return ChainView{}, err
		}
		points = append(points, p)
	}
	pRows.Close()

	for _, p := range points {
		sc := StationChain{PointID: p.id, Name: p.name}
		sRows, err := tx.Query(`SELECT id,public_code,status,hold,hold_reason,current_holder_id,
		                        version,result_version
		                        FROM samples WHERE point_id=? ORDER BY collected_at, id`, p.id)
		if err != nil {
			return ChainView{}, err
		}
		for sRows.Next() {
			var sm SampleChain
			var holderID string
			var hold int
			if err := sRows.Scan(&sm.SampleID, &sm.PublicCode, &sm.Status, &hold, &sm.HoldReason,
				&holderID, &sm.Version, &sm.AdoptedResult); err != nil {
				sRows.Close()
				return ChainView{}, err
			}
			sm.Hold = hold != 0
			sm.CurrentHolder, err = personRef(tx, holderID)
			if err != nil {
				sRows.Close()
				return ChainView{}, err
			}
			if sm.Custody, err = custodyOf(tx, sm.SampleID); err != nil {
				sRows.Close()
				return ChainView{}, err
			}
			if sm.Deviations, err = deviationsOf(tx, sm.SampleID); err != nil {
				sRows.Close()
				return ChainView{}, err
			}
			if sm.Results, err = resultsOf(tx, sm.SampleID); err != nil {
				sRows.Close()
				return ChainView{}, err
			}
			sc.Samples = append(sc.Samples, sm)
		}
		sRows.Close()
		view.Stations = append(view.Stations, sc)
	}
	return view, nil
}

func custodyOf(tx *sql.Tx, sampleID string) ([]CustodyEventDTO, error) {
	rows, err := tx.Query(`SELECT seq,kind,actor_id,note,seal_ok,temp_min,temp_max,
	                        COALESCE(transfer_id,''),created_at
	                        FROM custody_events WHERE sample_id=? ORDER BY seq`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustodyEventDTO
	for rows.Next() {
		var e CustodyEventDTO
		var actorID string
		var seal sql.NullInt64
		var tmin, tmax sql.NullFloat64
		if err := rows.Scan(&e.Seq, &e.Kind, &actorID, &e.Note, &seal, &tmin, &tmax,
			&e.TransferID, &e.At); err != nil {
			return nil, err
		}
		if e.Actor, err = personRef(tx, actorID); err != nil {
			return nil, err
		}
		if seal.Valid {
			b := seal.Int64 != 0
			e.SealOK = &b
		}
		if tmin.Valid {
			e.TempMin = &tmin.Float64
		}
		if tmax.Valid {
			e.TempMax = &tmax.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func deviationsOf(tx *sql.Tx, sampleID string) ([]DeviationDTO, error) {
	rows, err := tx.Query(`SELECT id,kind,title,detail,status,opened_by,created_at,COALESCE(closed_at,'')
	                        FROM deviations WHERE sample_id=? ORDER BY id`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviationDTO
	for rows.Next() {
		var d DeviationDTO
		var openerID string
		if err := rows.Scan(&d.ID, &d.Kind, &d.Title, &d.Detail, &d.Status,
			&openerID, &d.OpenedAt, &d.ClosedAt); err != nil {
			return nil, err
		}
		if d.OpenedBy, err = personRef(tx, openerID); err != nil {
			return nil, err
		}
		evRows, err := tx.Query(`SELECT action,actor_id,note,created_at
		                         FROM deviation_events WHERE deviation_id=? ORDER BY id`, d.ID)
		if err != nil {
			return nil, err
		}
		for evRows.Next() {
			var ev DeviationEventDTO
			var actorID string
			if err := evRows.Scan(&ev.Action, &actorID, &ev.Note, &ev.At); err != nil {
				evRows.Close()
				return nil, err
			}
			if ev.Actor, err = personRef(tx, actorID); err != nil {
				evRows.Close()
				return nil, err
			}
			d.Events = append(d.Events, ev)
		}
		evRows.Close()
		out = append(out, d)
	}
	return out, rows.Err()
}

func resultsOf(tx *sql.Tx, sampleID string) ([]ResultVersionDTO, error) {
	rows, err := tx.Query(`SELECT r.version,m.name,r.raw_reading,r.raw_unit,r.purity,
	                        r.supersedes_version,r.status,r.review_note,r.is_retest,r.is_external,
	                        r.analyst_id, r.reviewed_by
	                        FROM results r JOIN methods m ON m.id=r.method_id
	                        WHERE r.sample_id=? ORDER BY r.version`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResultVersionDTO
	for rows.Next() {
		var r ResultVersionDTO
		var analystID string
		var reviewerID sql.NullString
		if err := rows.Scan(&r.Version, &r.MethodName, &r.RawReading, &r.RawUnit, &r.Purity,
			&r.SupersedesVersion, &r.Status, &r.ReviewNote, &r.IsRetest, &r.IsExternal,
			&analystID, &reviewerID); err != nil {
			return nil, err
		}
		if r.Analyst, err = personRef(tx, analystID); err != nil {
			return nil, err
		}
		if reviewerID.Valid {
			p, perr := personRef(tx, reviewerID.String)
			if perr != nil {
				return nil, perr
			}
			r.Reviewer = &p
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Conclusion 是试车审批人能看到的结论投影：只含完成复核的版本。
type Conclusion struct {
	PublicCode     string  `json:"public_code"`
	PointName      string  `json:"sampling_point"`
	Status         string  `json:"status"`
	Hold           bool    `json:"hold"`
	AdoptedVersion int     `json:"adopted_version"`
	Purity         float64 `json:"purity"`
	Method         string  `json:"method"`
	Reviewer       string  `json:"reviewer"`
	ReviewedAt     string  `json:"reviewed_at"`
}

// BatchConclusions 返回批次下"可放行依据"：样品当前采用版本必须已完成复核；
// 未复核/被拒/暂停/作废的样品显式暴露，而不是被隐藏。
func (s *Store) BatchConclusions(batchID string) ([]Conclusion, error) {
	rows, err := s.db.Query(`SELECT s.public_code,sp.name,s.status,s.hold,s.result_version,
	                          COALESCE(r.purity,0),COALESCE(m.name,''),
	                          COALESCE(pr.name,''),COALESCE(r.reviewed_at,'')
	                          FROM samples s
	                          JOIN sampling_points sp ON sp.id=s.point_id
	                          LEFT JOIN results r ON r.sample_id=s.id AND r.version=s.result_version
	                          LEFT JOIN methods m ON m.id=r.method_id
	                          LEFT JOIN people pr ON pr.id=r.reviewed_by
	                          WHERE s.batch_id=?
	                          ORDER BY sp.name,s.collected_at`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conclusion
	for rows.Next() {
		var c Conclusion
		var hold int
		if err := rows.Scan(&c.PublicCode, &c.PointName, &c.Status, &hold, &c.AdoptedVersion,
			&c.Purity, &c.Method, &c.Reviewer, &c.ReviewedAt); err != nil {
			return nil, err
		}
		c.Hold = hold != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// SampleStatus 是单样品当前态（客户端扫码后读取保管版本用）。
func (s *Store) SampleStatus(sampleID string) (SampleChain, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return SampleChain{}, err
	}
	defer tx.Rollback()
	smp, err := loadSample(tx, sampleID)
	if err != nil {
		return SampleChain{}, err
	}
	out := SampleChain{
		SampleID: smp.ID, Status: smp.Status, Hold: smp.Hold != 0,
		HoldReason: smp.HoldReason, Version: smp.Version,
		AdoptedResult: smp.ResultVersion,
	}
	if err := tx.QueryRow(`SELECT public_code FROM samples WHERE id=?`, sampleID).Scan(&out.PublicCode); err != nil {
		return SampleChain{}, err
	}
	if out.CurrentHolder, err = personRef(tx, smp.CurrentHolder); err != nil {
		return SampleChain{}, err
	}
	if out.Custody, err = custodyOf(tx, sampleID); err != nil {
		return SampleChain{}, err
	}
	if out.Deviations, err = deviationsOf(tx, sampleID); err != nil {
		return SampleChain{}, err
	}
	if out.Results, err = resultsOf(tx, sampleID); err != nil {
		return SampleChain{}, err
	}
	return out, nil
}
