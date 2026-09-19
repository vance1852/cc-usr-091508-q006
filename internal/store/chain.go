package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ---------- 编号登记（编号冲突 -> 偏差） ----------

type RegisterCodeInput struct {
	ContainerID string
	Scope       string // collection | transit | lab
	Code        string
	SampleID    string // 可空：登记时样品尚未建立
	ActorID     string
	OpID        string
}

type RegisterCodeResult struct {
	RegistrationID string `json:"registration_id"`
	Conflict       bool   `json:"conflict"`
	DeviationID    int64  `json:"deviation_id,omitempty"`
}

// RegisterContainerCode 登记容器在某套编号体系（采集瓶/转运箱/实验室）下的编号。
// 同体系编号唯一：重复登记不覆盖旧记录，而在关联样品上形成 code_conflict 偏差并暂停使用。
func (s *Store) RegisterContainerCode(in RegisterCodeInput) (RegisterCodeResult, error) {
	if in.Scope != "collection" && in.Scope != "transit" && in.Scope != "lab" {
		return RegisterCodeResult{}, bizErr(ErrValidation, "编号体系只能是 collection/transit/lab")
	}
	if strings.TrimSpace(in.Code) == "" {
		return RegisterCodeResult{}, bizErr(ErrValidation, "编号不能为空")
	}
	res := RegisterCodeResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ActorID, "register_code", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return fillCodeReplay(tx, in, &res)
			}
			return err
		}
		if err := mustExist(tx, "containers", in.ContainerID); err != nil {
			return err
		}
		actor, err := getPersonTx(tx, in.ActorID)
		if err != nil {
			return err
		}
		if in.SampleID != "" {
			if err := mustExist(tx, "samples", in.SampleID); err != nil {
				return err
			}
		}
		regID := newID()
		_, err = tx.Exec(`INSERT INTO container_registrations(id,container_id,scope,code,sample_id,created_by)
		                 VALUES(?,?,?,?,?,?)`,
			regID, in.ContainerID, in.Scope, in.Code, nullable(in.SampleID), in.ActorID)
		if err == nil {
			res.RegistrationID = regID
			_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`, regID, in.OpID)
			return nil
		}
		if !isUniqueErr(err) {
			return err
		}
		// 编号冲突：找到锚定样品，开偏差（同编号的偏差幂等，只开一次）。
		res.Conflict = true
		sampleID := in.SampleID
		if sampleID == "" {
			var ex sql.NullString
			_ = tx.QueryRow(`SELECT sample_id FROM container_registrations
			                 WHERE scope=? AND code=? AND sample_id IS NOT NULL`,
				in.Scope, in.Code).Scan(&ex)
			sampleID = ex.String
		}
		if sampleID == "" {
			return bizErr(ErrConflict, "编号 %s/%s 已被占用且无关联样品，登记拒绝", scopeLabel(in.Scope), in.Code)
		}
		devID, err := ensureConflictDeviation(tx, in.Scope, in.Code, in.ContainerID, sampleID, actor)
		if err != nil {
			return err
		}
		res.Conflict = true
		res.DeviationID = devID
		_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`,
			fmt.Sprintf("conflict:%d", devID), in.OpID)
		// 事务正常提交：偏差与暂停状态必须落库，不能随登记失败一起回滚消失。
		return nil
	})
	return res, err
}

func fillCodeReplay(tx *sql.Tx, in RegisterCodeInput, res *RegisterCodeResult) error {
	ref, err := opRef(tx, in.OpID)
	if err != nil {
		return err
	}
	if strings.HasPrefix(ref, "conflict:") {
		var id int64
		fmt.Sscanf(ref, "conflict:%d", &id)
		res.Conflict = true
		res.DeviationID = id
		return nil
	}
	res.RegistrationID = ref
	return nil
}

// ensureConflictDeviation 返回该编号冲突的偏差（幂等：同 scope/code 只存在一条）。
func ensureConflictDeviation(tx *sql.Tx, scope, code, containerID, sampleID string, actor Person) (int64, error) {
	opID := "dev:code:" + scope + ":" + code
	var id int64
	err := tx.QueryRow(`SELECT id FROM deviations WHERE op_id=?`, opID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return openDeviationTx(tx, deviationInput{
		OpID:       opID,
		SampleID:   sampleID,
		Kind:       "code_conflict",
		Title:      fmt.Sprintf("编号冲突：%s编号 %s 被多个班组重复登记", scopeLabel(scope), code),
		Detail:     fmt.Sprintf("重复登记容器 %s，登记人 %s（%s）", containerID, actor.Name, actor.Team),
		OpenedBy:   actor.ID,
		OpenedTeam: actor.Team,
	})
}

func scopeLabel(scope string) string {
	switch scope {
	case "collection":
		return "采集瓶"
	case "transit":
		return "转运箱"
	case "lab":
		return "实验室"
	}
	return scope
}

// ---------- 交出（release） ----------

type ReleaseInput struct {
	SampleID        string
	OpID            string
	FromPersonID    string
	ToPersonID      string
	SealIntact      bool
	Note            string
	ExpectedVersion int // 离线客户端最后见到的保管版本
}

type ReleaseResult struct {
	TransferID  string `json:"transfer_id"`
	ReleaseCode string `json:"release_code"`
	Status      string `json:"status"`
}

// Release 当前保管人把样品交出，生成在途交接单。在途期间保管人仍是交出人，
// 链路始终恰有一名保管人。样品被暂停（hold=1）时禁止交出。
func (s *Store) Release(in ReleaseInput) (ReleaseResult, error) {
	if in.OpID == "" {
		return ReleaseResult{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := ReleaseResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.FromPersonID, "release", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return fillReleaseReplay(tx, in.OpID, &out)
			}
			return err
		}
		smp, err := loadSample(tx, in.SampleID)
		if err != nil {
			return err
		}
		if smp.Status != "active" {
			return bizErr(ErrState, "样品状态为 %s，不能交接", smp.Status)
		}
		if smp.Hold != 0 {
			return bizErr(ErrState, "样品因偏差暂停使用（%s），禁止交接", smp.HoldReason)
		}
		if smp.CurrentHolder != in.FromPersonID {
			return bizErr(ErrCustody, "只有当前保管人能交出样品")
		}
		if in.ExpectedVersion != 0 && smp.Version != in.ExpectedVersion {
			return bizErr(ErrConflict, "保管版本已变化（期望 %d，实际 %d），请重新扫码",
				in.ExpectedVersion, smp.Version)
		}
		to, err := getPersonTx(tx, in.ToPersonID)
		if err != nil {
			return err
		}
		from, err := getPersonTx(tx, in.FromPersonID)
		if err != nil {
			return err
		}
		transferID := newID()
		code := genReleaseCode()
		_, err = tx.Exec(`INSERT INTO transfers
			(id,op_id,sample_id,container_id,from_person_id,to_person_id,release_code,
			 release_note,seal_at_release,status)
			VALUES(?,?,?,?,?,?,?,?,?,'released')`,
			transferID, in.OpID, smp.ID, smp.ContainerID, in.FromPersonID, to.ID, code,
			in.Note, b2i(in.SealIntact))
		if isUniqueErr(err) {
			// 样品或容器已存在在途交接（两人同时扫码的第二条必然落在这）。
			return bizErr(ErrConflict, "样品/容器已有在途交接，不能重复交出")
		}
		if err != nil {
			return err
		}
		if err := appendCustody(tx, smp.ID, "release", in.FromPersonID, from.Team,
			transferID, "交还给 "+to.Name); err != nil {
			return err
		}
		_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`, transferID, in.OpID)
		out = ReleaseResult{TransferID: transferID, ReleaseCode: code, Status: "released"}
		return nil
	})
	return out, err
}

func fillReleaseReplay(tx *sql.Tx, opID string, out *ReleaseResult) error {
	var id, code, status string
	err := tx.QueryRow(`SELECT id,release_code,status FROM transfers WHERE op_id=?`, opID).
		Scan(&id, &code, &status)
	if err != nil {
		return err
	}
	out.TransferID, out.ReleaseCode, out.Status = id, code, status
	return nil
}

// ---------- 接收（receive） ----------

type ReceiveInput struct {
	TransferID      string
	OpID            string
	ReceiverID      string
	ReleaseCode     string // 扫码得到的交出确认码，必须与交接单一致
	SealIntact      bool
	SealNo          string // 接收到的封签号，与采集封签号比对
	HasTemp         bool
	TempMin         float64
	TempMax         float64
	ExpectedVersion int
}

type ReceiveResult struct {
	TransferID   string  `json:"transfer_id"`
	SampleID     string  `json:"sample_id"`
	Status       string  `json:"status"`
	HolderID     string  `json:"holder_id"`
	Version      int     `json:"version"`
	Hold         bool    `json:"hold"`
	DeviationIDs []int64 `json:"deviation_ids"`
}

// Receive 接收人凭交出确认码完成接收；封签破损/温度越界/温度空白在本事务内形成偏差，
// 样品随即暂停，但保管人仍正常切换（去向与异常都如实留痕）。
func (s *Store) Receive(in ReceiveInput) (ReceiveResult, error) {
	if in.OpID == "" {
		return ReceiveResult{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := ReceiveResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.ReceiverID, "receive", in); err != nil {
			if errors.Is(err, ErrOpReplay) {
				return fillReceiveReplay(tx, in.OpID, &out)
			}
			return err
		}
		var tr transferRow
		err := tx.QueryRow(`SELECT id,sample_id,container_id,from_person_id,to_person_id,
		                    release_code,seal_at_release,status
		                    FROM transfers WHERE id=?`, in.TransferID).Scan(
			&tr.ID, &tr.SampleID, &tr.ContainerID, &tr.From, &tr.To, &tr.Code, &tr.SealAtRelease, &tr.Status)
		if errors.Is(err, sql.ErrNoRows) {
			return bizErr(ErrNotFound, "交接单不存在")
		}
		if err != nil {
			return err
		}
		if tr.To != in.ReceiverID {
			return bizErr(ErrCustody, "交接单指定的接收人不是当前操作人")
		}
		if tr.Status != "released" {
			return bizErr(ErrState, "交接单状态为 %s，不能接收", tr.Status)
		}
		if tr.Code != in.ReleaseCode {
			return bizErr(ErrCustody, "交出确认码不符，不能承接上一保管人的交接")
		}
		smp, err := loadSample(tx, tr.SampleID)
		if err != nil {
			return err
		}
		if smp.Status != "active" {
			return bizErr(ErrState, "样品状态为 %s，不能接收", smp.Status)
		}
		if smp.CurrentHolder != tr.From {
			return bizErr(ErrCustody, "当前保管人与交出人不一致，保管链断裂")
		}
		if in.ExpectedVersion != 0 && smp.Version != in.ExpectedVersion {
			return bizErr(ErrConflict, "保管版本已变化（期望 %d，实际 %d），请重新扫码",
				in.ExpectedVersion, smp.Version)
		}
		receiver, err := getPersonTx(tx, in.ReceiverID)
		if err != nil {
			return err
		}

		// 异常判定（以本次上送事实为准，可一次叠加多条偏差）。
		var devIDs []int64
		if !in.SealIntact || (in.SealNo != "" && in.SealNo != smp.SealNo) {
			id, err := openDeviationTx(tx, deviationInput{
				OpID:       "dev:recv:" + tr.ID + ":seal",
				SampleID:   smp.ID,
				TransferID: tr.ID,
				Kind:       "seal_broken",
				Title:      "接收时封签破损/封签号不符",
				Detail:     fmt.Sprintf("上报封签号 %q，采集封签号 %q", in.SealNo, smp.SealNo),
				OpenedBy:   receiver.ID,
				OpenedTeam: receiver.Team,
			})
			if err != nil {
				return err
			}
			devIDs = append(devIDs, id)
		}
		lo, hi, limitsOK, lerr := currentTempLimits(tx, smp.BatchID)
		if lerr != nil {
			return lerr
		}
		switch {
		case !in.HasTemp:
			id, err := openDeviationTx(tx, deviationInput{
				OpID:       "dev:recv:" + tr.ID + ":tempgap",
				SampleID:   smp.ID,
				TransferID: tr.ID,
				Kind:       "temp_gap",
				Title:      "接收时温度记录空白",
				OpenedBy:   receiver.ID,
				OpenedTeam: receiver.Team,
			})
			if err != nil {
				return err
			}
			devIDs = append(devIDs, id)
		case !limitsOK:
			id, err := openDeviationTx(tx, deviationInput{
				OpID:       "dev:recv:" + tr.ID + ":nolimit",
				SampleID:   smp.ID,
				TransferID: tr.ID,
				Kind:       "temp_gap",
				Title:      "批次未配置温度允许区间，无法判定温度符合性",
				OpenedBy:   receiver.ID,
				OpenedTeam: receiver.Team,
			})
			if err != nil {
				return err
			}
			devIDs = append(devIDs, id)
		case in.TempMax < lo || in.TempMin > hi:
			id, err := openDeviationTx(tx, deviationInput{
				OpID:       "dev:recv:" + tr.ID + ":tempex",
				SampleID:   smp.ID,
				TransferID: tr.ID,
				Kind:       "temp_excursion",
				Title:      "接收温度越界",
				Detail: fmt.Sprintf("上报区间 [%.2f, %.2f]℃，允许区间 [%.2f, %.2f]℃",
					in.TempMin, in.TempMax, lo, hi),
				OpenedBy:   receiver.ID,
				OpenedTeam: receiver.Team,
			})
			if err != nil {
				return err
			}
			devIDs = append(devIDs, id)
		}

		// CAS 换保管人：并发接收只有一条能命中。
		res, err := tx.Exec(`UPDATE samples
			SET current_holder_id=?, version=version+1
			WHERE id=? AND current_holder_id=? AND version=?`,
			in.ReceiverID, smp.ID, tr.From, smp.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return bizErr(ErrConflict, "保管人版本冲突，接收失败")
		}
		if _, err := tx.Exec(`UPDATE transfers
			SET status='received',received_at=?,receiver_team=?,seal_at_receive=?,receive_op_id=?
			WHERE id=? AND status='released'`,
			nowUTC(), receiver.Team, b2i(in.SealIntact), in.OpID, tr.ID); err != nil {
			return err
		}
		note := "接收入库"
		if len(devIDs) > 0 {
			note = fmt.Sprintf("接收入库（%d 条偏差，已暂停）", len(devIDs))
		}
		if err := appendCustodyEx(tx, smp.ID, "receive", receiver.ID, receiver.Team,
			tr.ID, note, b2i(in.SealIntact), in.HasTemp, in.TempMin, in.TempMax); err != nil {
			return err
		}
		_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`, tr.ID, in.OpID)
		out = ReceiveResult{
			TransferID: tr.ID, SampleID: smp.ID, Status: "received",
			HolderID: in.ReceiverID, Version: smp.Version + 1,
			Hold: len(devIDs) > 0 || smp.Hold != 0, DeviationIDs: devIDs,
		}
		return nil
	})
	return out, err
}

func fillReceiveReplay(tx *sql.Tx, opID string, out *ReceiveResult) error {
	var transferID, sampleID, holder string
	var status sql.NullString
	var version int
	err := tx.QueryRow(`SELECT t.id,t.sample_id,t.status,s.current_holder_id,s.version
	                    FROM transfers t JOIN samples s ON s.id=t.sample_id
	                    WHERE t.receive_op_id=?`, opID).
		Scan(&transferID, &sampleID, &status, &holder, &version)
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id FROM deviations WHERE transfer_id=?`, transferID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	var hold int
	_ = tx.QueryRow(`SELECT hold FROM samples WHERE id=?`, sampleID).Scan(&hold)
	*out = ReceiveResult{
		TransferID: transferID, SampleID: sampleID, Status: status.String,
		HolderID: holder, Version: version, Hold: hold != 0, DeviationIDs: ids,
	}
	return nil
}

// ---------- 取消在途交接 ----------

type CancelResult struct {
	Status string `json:"status"`
}

// CancelTransfer 交出人取消尚未接收的交接；保管人不变、版本不变。
func (s *Store) CancelTransfer(transferID, actorID, opID string) (CancelResult, error) {
	if opID == "" {
		return CancelResult{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := CancelResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, opID, actorID, "cancel_transfer", transferID); err != nil {
			if errors.Is(err, ErrOpReplay) {
				out.Status = "cancelled"
				return nil
			}
			return err
		}
		var from, sampleID, status string
		if err := tx.QueryRow(`SELECT from_person_id,sample_id,status FROM transfers WHERE id=?`,
			transferID).Scan(&from, &sampleID, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "交接单不存在")
			}
			return err
		}
		if from != actorID {
			return bizErr(ErrPermission, "只有交出人能取消交接")
		}
		if status != "released" {
			return bizErr(ErrState, "交接单状态为 %s，不能取消", status)
		}
		if _, err := tx.Exec(`UPDATE transfers SET status='cancelled' WHERE id=? AND status='released'`,
			transferID); err != nil {
			return err
		}
		actor, err := getPersonTx(tx, actorID)
		if err != nil {
			return err
		}
		if err := appendCustody(tx, sampleID, "cancel", actorID, actor.Team, transferID, "交接取消"); err != nil {
			return err
		}
		out.Status = "cancelled"
		return nil
	})
	return out, err
}

// ---------- 化验消耗（容器释放，可再次使用） ----------

type ConsumeResult struct {
	Status      string `json:"status"`
	ContainerID string `json:"container_id"`
}

// Consume 化验消耗样品并释放容器；暂停中的样品禁止消耗。
func (s *Store) Consume(sampleID, actorID, opID, note string) (ConsumeResult, error) {
	if opID == "" {
		return ConsumeResult{}, bizErr(ErrValidation, "缺少幂等键 op_id")
	}
	out := ConsumeResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, opID, actorID, "consume", sampleID); err != nil {
			if errors.Is(err, ErrOpReplay) {
				out.Status = "consumed"
				return nil
			}
			return err
		}
		smp, err := loadSample(tx, sampleID)
		if err != nil {
			return err
		}
		actor, err := getPersonTx(tx, actorID)
		if err != nil {
			return err
		}
		if actor.Role != "analyst" && actor.Role != "lab_admin" {
			return bizErr(ErrPermission, "只有化验人员能消耗样品")
		}
		if smp.Status != "active" {
			return bizErr(ErrState, "样品状态为 %s，不能消耗", smp.Status)
		}
		if smp.Hold != 0 {
			return bizErr(ErrState, "样品因偏差暂停使用，禁止消耗")
		}
		if smp.CurrentHolder != actorID {
			return bizErr(ErrCustody, "只有当前保管人能消耗样品")
		}
		if _, err := tx.Exec(`UPDATE samples SET status='consumed' WHERE id=?`, smp.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE containers SET active_sample_id=NULL WHERE id=?`, smp.ContainerID); err != nil {
			return err
		}
		if err := appendCustody(tx, smp.ID, "consume", actorID, actor.Team, "", note); err != nil {
			return err
		}
		out = ConsumeResult{Status: "consumed", ContainerID: smp.ContainerID}
		return nil
	})
	return out, err
}
