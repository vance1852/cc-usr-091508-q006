package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------- 结果提交（版本化、只增） ----------

type ResultInput struct {
	SampleID          string
	MethodID          string
	RawReading        string // 原始读数，原样留存
	RawUnit           string
	Purity            float64
	SupersedesVersion int // 复测时引用的旧版本；0 表示首版
	AnalystID         string
	OpID              string
}

type ResultOutput struct {
	ID                int64  `json:"id"`
	SampleID          string `json:"sample_id"`
	Version           int    `json:"version"`
	Status            string `json:"status"`
	SupersedesVersion int    `json:"supersedes_version"`
	IsRetest          bool   `json:"is_retest"`
	IsExternal        bool   `json:"is_external"`
}

// SubmitResult 提交一个检测结果版本。已发布/已复核的结论永不修改：
// 纠正只能以新版本引用旧版本（supersedes_version）。
func (s *Store) SubmitResult(in ResultInput) (ResultOutput, error) {
	if strings.TrimSpace(in.RawReading) == "" {
		return ResultOutput{}, bizErr(ErrValidation, "原始读数不能为空")
	}
	if in.OpID == "" {
		return ResultOutput{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := ResultOutput{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.AnalystID, "submit_result", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return tx.QueryRow(`SELECT id,sample_id,version,status,supersedes_version,is_retest,is_external
				                    FROM results WHERE id=(SELECT CAST(ref_id AS INTEGER) FROM ops WHERE op_id=?)`,
					in.OpID).Scan(&out.ID, &out.SampleID, &out.Version, &out.Status,
					&out.SupersedesVersion, &out.IsRetest, &out.IsExternal)
			}
			return err
		}
		analyst, err := getPersonTx(tx, in.AnalystID)
		if err != nil {
			return err
		}
		if analyst.Role != "analyst" && analyst.Role != "lab_admin" {
			return bizErr(ErrPermission, "只有化验人员能提交检测结果")
		}
		if err := mustExist(tx, "methods", in.MethodID); err != nil {
			return err
		}
		smp, err := loadSample(tx, in.SampleID)
		if err != nil {
			return err
		}
		if smp.Status == "void" {
			return bizErr(ErrState, "样品已作废，不能再提交结果")
		}
		var version int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM results WHERE sample_id=?`,
			smp.ID).Scan(&version); err != nil {
			return err
		}
		if in.SupersedesVersion != 0 {
			if in.SupersedesVersion >= version {
				return bizErr(ErrValidation, "复测只能引用已存在的旧版本")
			}
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM results
			                      WHERE sample_id=? AND version=?`, smp.ID, in.SupersedesVersion).
				Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return bizErr(ErrValidation, "被引用的旧版本不存在")
			}
		}
		res, err := tx.Exec(`INSERT INTO results
			(sample_id,version,method_id,raw_reading,raw_unit,purity,supersedes_version,
			 analyst_id,analyst_team,status,is_retest)
			VALUES(?,?,?,?,?,?,?,?,?, 'submitted',?)`,
			smp.ID, version, in.MethodID, in.RawReading, in.RawUnit, in.Purity,
			in.SupersedesVersion, analyst.ID, analyst.Team, b2i(in.SupersedesVersion != 0))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`, fmt.Sprint(id), in.OpID)
		out = ResultOutput{
			ID: id, SampleID: smp.ID, Version: version, Status: "submitted",
			SupersedesVersion: in.SupersedesVersion, IsRetest: in.SupersedesVersion != 0,
		}
		return nil
	})
	return out, err
}

// ---------- 复核（跨班组职责分离） ----------

type ReviewInput struct {
	ResultID   int64
	ReviewerID string
	Approve    bool
	Note       string
	OpID       string
}

type ReviewOutput struct {
	ResultID int64  `json:"result_id"`
	Version  int    `json:"version"`
	Status   string `json:"status"`
}

// ReviewResult 复核检测结果。采样班组不能批准自己的检测：
// 复核人所在班组必须不同于采样班组。
func (s *Store) ReviewResult(in ReviewInput) (ReviewOutput, error) {
	if in.OpID == "" {
		return ReviewOutput{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := ReviewOutput{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ReviewerID, "review_result", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return tx.QueryRow(`SELECT id,version,status FROM results WHERE id=?`, in.ResultID).
					Scan(&out.ResultID, &out.Version, &out.Status)
			}
			return err
		}
		reviewer, err := getPersonTx(tx, in.ReviewerID)
		if err != nil {
			return err
		}
		if reviewer.Role != "reviewer" {
			return bizErr(ErrPermission, "只有复核人能复核检测结果")
		}
		var sampleID, status, collectorTeam string
		var version int
		if err := tx.QueryRow(`SELECT r.sample_id,r.version,r.status,s.collector_team
		                      FROM results r JOIN samples s ON s.id=r.sample_id
		                      WHERE r.id=?`, in.ResultID).
			Scan(&sampleID, &version, &status, &collectorTeam); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "结果版本不存在")
			}
			return err
		}
		if status != "submitted" {
			return bizErr(ErrState, "结果版本状态为 %s，不能再复核", status)
		}
		if reviewer.Team == collectorTeam {
			return bizErr(ErrDuty, "复核人班组 %s 与采样班组相同，不能批准本班采样的检测", reviewer.Team)
		}
		newStatus := "reviewed"
		if !in.Approve {
			newStatus = "rejected"
		}
		res, err := tx.Exec(`UPDATE results
			SET status=?,review_note=?,reviewed_by=?,reviewed_at=?
			WHERE id=? AND status='submitted'`,
			newStatus, in.Note, reviewer.ID, nowUTC(), in.ResultID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return bizErr(ErrConflict, "结果状态已变化，复核失败")
		}
		out = ReviewOutput{ResultID: in.ResultID, Version: version, Status: newStatus}
		return nil
	})
	return out, err
}

// ---------- 采用结论版本 ----------

type AdoptInput struct {
	SampleID string
	Version  int
	ActorID  string
	Note     string
	OpID     string
}

type AdoptOutput struct {
	SampleID      string `json:"sample_id"`
	ResultVersion int    `json:"result_version"`
}

// AdoptResult 指定样品当前采用的结果版本。只能采用完成复核（reviewed）的结论；
// 样品存在未结案偏差（暂停）时禁止采用。历次采用均在 result_adoptions 留痕。
func (s *Store) AdoptResult(in AdoptInput) (AdoptOutput, error) {
	if in.OpID == "" {
		return AdoptOutput{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := AdoptOutput{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ActorID, "adopt_result", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return tx.QueryRow(`SELECT id,result_version FROM samples WHERE id=?`, in.SampleID).
					Scan(&out.SampleID, &out.ResultVersion)
			}
			return err
		}
		actor, err := getPersonTx(tx, in.ActorID)
		if err != nil {
			return err
		}
		if actor.Role != "lab_admin" {
			return bizErr(ErrPermission, "只有化验室管理员能采用结果版本")
		}
		smp, err := loadSample(tx, in.SampleID)
		if err != nil {
			return err
		}
		if smp.Hold != 0 {
			return bizErr(ErrState, "样品因偏差暂停使用，存在未结案偏差，不能采用结论")
		}
		if smp.Status == "void" {
			return bizErr(ErrState, "样品已作废，不能采用结论")
		}
		var rstatus string
		if err := tx.QueryRow(`SELECT status FROM results WHERE sample_id=? AND version=?`,
			smp.ID, in.Version).Scan(&rstatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "结果版本不存在")
			}
			return err
		}
		if rstatus != "reviewed" {
			return bizErr(ErrState, "只能采用完成复核的结论（该版本状态 %s）", rstatus)
		}
		if _, err := tx.Exec(`UPDATE samples SET result_version=? WHERE id=?`,
			in.Version, smp.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO result_adoptions(sample_id,seq,version,actor_id,actor_team,note)
		                      VALUES(?,(SELECT COALESCE(MAX(seq),0)+1 FROM result_adoptions WHERE sample_id=?),
		                             ?,?,?,?)`,
			smp.ID, smp.ID, in.Version, actor.ID, actor.Team, in.Note); err != nil {
			return err
		}
		out = AdoptOutput{SampleID: smp.ID, ResultVersion: in.Version}
		return nil
	})
	return out, err
}

// ---------- 外部实验室委托 ----------

// AssignExternal 委托外部实验室检测：外部之后只能凭脱敏批号看到该委托。
func (s *Store) AssignExternal(publicCode, methodID, actorID, opID string) error {
	if opID == "" {
		return bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	return s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, opID, actorID, "assign_external", publicCode+":"+methodID); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return nil
			}
			return err
		}
		actor, err := getPersonTx(tx, actorID)
		if err != nil {
			return err
		}
		if actor.Role != "lab_admin" {
			return bizErr(ErrPermission, "只有化验室管理员能建立外部委托")
		}
		if _, err := loadSampleByCode(tx, publicCode); err != nil {
			return err
		}
		if err := mustExist(tx, "methods", methodID); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO external_assignments(public_code,method_id,assigned_by)
		                  VALUES(?,?,?)`, publicCode, methodID, actorID)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "该脱敏批号已有外部委托")
		}
		return err
	})
}

type ExternalOrder struct {
	PublicCode string `json:"public_code"` // 脱敏批号；无任何内部真码
	MethodName string `json:"method"`
}

// MethodIDByName 按方法名解析方法 ID（外部实验室只能看到方法名）。
func (s *Store) MethodIDByName(name string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM methods WHERE name=?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", bizErr(ErrValidation, "未知检测方法 %q", name)
	}
	return id, err
}

// ListExternalOrders 外部实验室视角：只见脱敏批号与委托方法。
func (s *Store) ListExternalOrders() ([]ExternalOrder, error) {
	rows, err := s.db.Query(`SELECT a.public_code,m.name
	                         FROM external_assignments a JOIN methods m ON m.id=a.method_id
	                         ORDER BY a.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExternalOrder
	for rows.Next() {
		var o ExternalOrder
		if err := rows.Scan(&o.PublicCode, &o.MethodName); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ExternalResultInput 外部实验室回传结果。
type ExternalResultInput struct {
	PublicCode string
	MethodID   string
	RawReading string
	RawUnit    string
	Purity     float64
	ActorID    string
	OpID       string
}

// SubmitExternalResult 外部实验室凭脱敏批号回传结果，生成 is_external 的 submitted 版本，
// 仍须内部跨班组复核后才能被采用。
func (s *Store) SubmitExternalResult(in ExternalResultInput) (ResultOutput, error) {
	if strings.TrimSpace(in.RawReading) == "" {
		return ResultOutput{}, bizErr(ErrValidation, "原始读数不能为空")
	}
	if in.OpID == "" {
		return ResultOutput{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := ResultOutput{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ActorID, "external_result", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return tx.QueryRow(`SELECT id,sample_id,version,status,supersedes_version,is_retest,is_external
				                    FROM results WHERE id=(SELECT CAST(ref_id AS INTEGER) FROM ops WHERE op_id=?)`,
					in.OpID).Scan(&out.ID, &out.SampleID, &out.Version, &out.Status,
					&out.SupersedesVersion, &out.IsRetest, &out.IsExternal)
			}
			return err
		}
		actor, err := getPersonTx(tx, in.ActorID)
		if err != nil {
			return err
		}
		if actor.Role != "external_lab" {
			return bizErr(ErrPermission, "只有外部实验室身份能回传外部结果")
		}
		var assignee string
		if err := tx.QueryRow(`SELECT method_id FROM external_assignments WHERE public_code=?`,
			in.PublicCode).Scan(&assignee); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "未找到该脱敏批号的外部委托")
			}
			return err
		}
		if assignee != in.MethodID {
			return bizErr(ErrValidation, "检测方法与委托方法不一致")
		}
		smp, err := loadSampleByCode(tx, in.PublicCode)
		if err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM results WHERE sample_id=?`,
			smp.ID).Scan(&version); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO results
			(sample_id,version,method_id,raw_reading,raw_unit,purity,supersedes_version,
			 analyst_id,analyst_team,status,is_external)
			VALUES(?,?,?,?,?,?,?,?,?, 'submitted',1)`,
			smp.ID, version, in.MethodID, in.RawReading, in.RawUnit, in.Purity, 0,
			actor.ID, actor.Team)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`, fmt.Sprint(id), in.OpID)
		out = ResultOutput{
			ID: id, SampleID: smp.ID, Version: version, Status: "submitted",
			IsExternal: true,
		}
		return nil
	})
	return out, err
}
