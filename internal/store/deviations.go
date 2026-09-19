package store

import (
	"database/sql"
	"errors"
	"strings"
)

// DispositionInput 是偏差处置请求。
type DispositionInput struct {
	DeviationID int64
	ActorID     string
	Action      string // investigate | corrective_action | resample | close | reject
	Note        string
	OpID        string
}

type DispositionResult struct {
	DeviationID int64  `json:"deviation_id"`
	Status      string `json:"status"`
	SampleHold  bool   `json:"sample_hold"`
	SampleState string `json:"sample_status"`
}

// Dispose 在偏差上追加处置事件（append-only）。
//   - investigate/corrective_action/resample：只留痕，偏差仍 open，样品保持暂停；
//   - close：偏差结案，样品无其他 open 偏差时解除暂停；
//   - reject：拒收，偏差结案、样品作废、容器释放。
func (s *Store) Dispose(in DispositionInput) (DispositionResult, error) {
	switch in.Action {
	case "investigate", "corrective_action", "resample", "close", "reject":
	default:
		return DispositionResult{}, bizErr(ErrValidation, "未知处置动作 %q", in.Action)
	}
	if in.OpID == "" {
		return DispositionResult{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := DispositionResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ActorID, "dispose:"+in.Action, in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return fillDisposeReplay(tx, in.DeviationID, &out)
			}
			return err
		}
		actor, err := getPersonTx(tx, in.ActorID)
		if err != nil {
			return err
		}
		if actor.Role != "lab_admin" {
			return bizErr(ErrPermission, "只有化验室管理员能处置偏差")
		}
		var sampleID, status, title string
		if err := tx.QueryRow(`SELECT sample_id,status,title FROM deviations WHERE id=?`,
			in.DeviationID).Scan(&sampleID, &status, &title); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "偏差不存在")
			}
			return err
		}
		if status != "open" {
			return bizErr(ErrState, "偏差已结案（%s），不能重复处置", status)
		}
		if _, err := tx.Exec(`INSERT INTO deviation_events(deviation_id,action,actor_id,actor_team,note)
		                      VALUES(?,?,?,?,?)`,
			in.DeviationID, in.Action, actor.ID, actor.Team, in.Note); err != nil {
			return err
		}
		switch in.Action {
		case "close", "reject":
			newStatus := "dispositioned"
			if in.Action == "reject" {
				newStatus = "rejected"
			}
			if _, err := tx.Exec(
				`UPDATE deviations SET status=?,closed_at=? WHERE id=? AND status='open'`,
				newStatus, nowUTC(), in.DeviationID); err != nil {
				return err
			}
		}
		if in.Action == "reject" {
			if _, err := tx.Exec(`UPDATE samples SET status='void' WHERE id=?`, sampleID); err != nil {
				return err
			}
			// 在途交接一律取消并留痕，避免容器的在途唯一索引阻碍其复用。
			openRows, err := tx.Query(`SELECT id FROM transfers
			                           WHERE sample_id=? AND status='released'`, sampleID)
			if err != nil {
				return err
			}
			var openIDs []string
			for openRows.Next() {
				var tid string
				if err := openRows.Scan(&tid); err != nil {
					openRows.Close()
					return err
				}
				openIDs = append(openIDs, tid)
			}
			openRows.Close()
			for _, tid := range openIDs {
				if _, err := tx.Exec(`UPDATE transfers SET status='cancelled' WHERE id=?`, tid); err != nil {
					return err
				}
				if err := appendCustody(tx, sampleID, "cancel", actor.ID, actor.Team, tid,
					"样品拒收，在途交接自动取消"); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE containers SET active_sample_id=NULL
			                      WHERE id=(SELECT container_id FROM samples WHERE id=?)`, sampleID); err != nil {
				return err
			}
		}
		// 重新计算 hold：仍有 open 偏差则保持暂停，否则解除。
		openN, err := countOpenDeviations(tx, sampleID)
		if err != nil {
			return err
		}
		if openN == 0 {
			if _, err := tx.Exec(`UPDATE samples SET hold=0, hold_reason='' WHERE id=?`, sampleID); err != nil {
				return err
			}
		}
		smp, err := loadSample(tx, sampleID)
		if err != nil {
			return err
		}
		newStatus := "open"
		if in.Action == "close" {
			newStatus = "dispositioned"
		}
		if in.Action == "reject" {
			newStatus = "rejected"
		}
		out = DispositionResult{
			DeviationID: in.DeviationID,
			Status:      newStatus,
			SampleHold:  openN != 0,
			SampleState: smp.Status,
		}
		return nil
	})
	return out, err
}

func fillDisposeReplay(tx *sql.Tx, devID int64, out *DispositionResult) error {
	var status, sampleID string
	if err := tx.QueryRow(`SELECT status,sample_id FROM deviations WHERE id=?`, devID).
		Scan(&status, &sampleID); err != nil {
		return err
	}
	var hold int
	var sampleStatus string
	if err := tx.QueryRow(`SELECT hold,status FROM samples WHERE id=?`, sampleID).
		Scan(&hold, &sampleStatus); err != nil {
		return err
	}
	*out = DispositionResult{
		DeviationID: devID, Status: status,
		SampleHold: hold != 0, SampleState: sampleStatus,
	}
	return nil
}

// OpenManualDeviation 允许复核人/化验室管理员登记其他异常偏差（如复核发现读数可疑）。
type ManualDeviationInput struct {
	SampleID string
	ActorID  string
	Title    string
	Detail   string
	Kind     string // other | seal_broken | temp_excursion | temp_gap
	OpID     string
}

func (s *Store) OpenManualDeviation(in ManualDeviationInput) (int64, error) {
	if strings.TrimSpace(in.Title) == "" {
		return 0, bizErr(ErrValidation, "偏差标题不能为空")
	}
	if in.Kind == "" {
		in.Kind = "other"
	}
	switch in.Kind {
	case "seal_broken", "temp_excursion", "temp_gap", "code_conflict", "other":
	default:
		return 0, bizErr(ErrValidation, "未知偏差类型")
	}
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ActorID, "open_deviation", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return tx.QueryRow(`SELECT id FROM deviations WHERE op_id=?`, in.OpID).Scan(&id)
			}
			return err
		}
		actor, err := getPersonTx(tx, in.ActorID)
		if err != nil {
			return err
		}
		if actor.Role != "lab_admin" && actor.Role != "reviewer" {
			return bizErr(ErrPermission, "只有复核人或化验室管理员能登记偏差")
		}
		if err := mustExist(tx, "samples", in.SampleID); err != nil {
			return err
		}
		did, err := openDeviationTx(tx, deviationInput{
			OpID:       in.OpID,
			SampleID:   in.SampleID,
			Kind:       in.Kind,
			Title:      in.Title,
			Detail:     in.Detail,
			OpenedBy:   actor.ID,
			OpenedTeam: actor.Team,
		})
		if err != nil {
			return err
		}
		id = did
		return nil
	})
	return id, err
}
