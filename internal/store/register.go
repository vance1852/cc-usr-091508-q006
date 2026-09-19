package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ConflictItem 描述一次被拦下的编号/资源冲突。
type ConflictItem struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// RegisterSample 登记一个采集瓶样品。
//
// 采集瓶（容器）、转运箱、实验室编号任一与未完结登记冲突，都会：
//  1. 在 registration_attempts 留痕；
//  2. 生成 number_conflict 偏差（追加、不可删除）；
//  3. 中止本次登记（不创建样品），调用方收到 ErrConflict 与冲突明细。
//
// 成功时样品当前保管人为登记采样员本人，并产生链首 collect 事件。
func (d *DB) RegisterSample(actor User, in RegisterSampleInput) (*Sample, []ConflictItem, error) {
	if actor.Role != "sampler" && actor.Role != "admin" {
		return nil, nil, fmt.Errorf("%w: 只有采样班组可以登记样品", ErrForbidden)
	}
	if actor.CrewCode == "" {
		return nil, nil, fmt.Errorf("%w: 采样员必须归属班组", ErrValidation)
	}
	in.BoxCode = strings.TrimSpace(in.BoxCode)
	for _, f := range [][2]string{
		{"batch_code", in.BatchCode}, {"box_code", in.BoxCode}, {"lab_no", in.LabNo},
		{"container_code", in.ContainerCode}, {"seal_no", in.SealNo},
		{"point_code", in.PointCode}, {"method_code", in.MethodCode}, {"sampled_at", in.SampledAt},
	} {
		if strings.TrimSpace(f[1]) == "" {
			return nil, nil, fmt.Errorf("%w: %s 不能为空", ErrValidation, f[0])
		}
	}
	if _, err := parseTime(in.SampledAt); err != nil {
		return nil, nil, fmt.Errorf("%w: sampled_at 必须是 RFC3339 时间", ErrValidation)
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	var batchID int64
	if err := tx.QueryRow(`SELECT id FROM batches WHERE code = ?`, in.BatchCode).Scan(&batchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: 批次 %s 不存在", ErrNotFound, in.BatchCode)
		}
		return nil, nil, err
	}
	var pointID int64
	if err := tx.QueryRow(`SELECT id FROM sampling_points WHERE code = ?`, in.PointCode).Scan(&pointID); err != nil {
		return nil, nil, fmt.Errorf("%w: 采样点 %s 不存在", ErrNotFound, in.PointCode)
	}
	var methodID int64
	var tMin, tMax sql.NullFloat64
	var maxGap int
	if err := tx.QueryRow(`SELECT id, temp_min, temp_max, max_gap_minutes FROM methods WHERE code = ?`,
		in.MethodCode).Scan(&methodID, &tMin, &tMax, &maxGap); err != nil {
		return nil, nil, fmt.Errorf("%w: 检测方法 %s 不存在", ErrNotFound, in.MethodCode)
	}

	var conflicts []ConflictItem

	// 实验室编号全局唯一。
	var existCrew string
	err = tx.QueryRow(`SELECT c.code FROM samples s JOIN crews c ON c.code = s.crew_id
WHERE s.lab_no = ? LIMIT 1`, in.LabNo).Scan(&existCrew)
	switch {
	case err == nil:
		reason := "实验室编号已被本班组登记"
		if existCrew != actor.CrewCode {
			reason = "实验室编号已被另一班组（" + existCrew + "）登记"
		}
		conflicts = append(conflicts, ConflictItem{"lab_no", in.LabNo, reason})
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, nil, err
	}

	// 容器不得同时绑定两个未完结样品。
	var boundSample int64
	var boundCrew string
	err = tx.QueryRow(`SELECT cb.sample_id, s.crew_id FROM container_bindings cb
JOIN samples s ON s.id = cb.sample_id
JOIN containers c ON c.id = cb.container_id
WHERE c.code = ? AND cb.released_at IS NULL`, in.ContainerCode).Scan(&boundSample, &boundCrew)
	switch {
	case err == nil:
		reason := "采集瓶仍在本班组另一样品上使用"
		if boundCrew != actor.CrewCode {
			reason = "采集瓶正被另一班组（" + boundCrew + "）的未完结样品占用"
		}
		conflicts = append(conflicts, ConflictItem{"container_code", in.ContainerCode, reason})
	case errors.Is(err, sql.ErrNoRows):
		var containerIDChk int64
		if err := tx.QueryRow(`SELECT id FROM containers WHERE code = ?`, in.ContainerCode).Scan(&containerIDChk); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, fmt.Errorf("%w: 容器 %s 未建档", ErrNotFound, in.ContainerCode)
			}
			return nil, nil, err
		}
	default:
		return nil, nil, err
	}

	// 封签一次性使用。
	var sealSample int64
	err = tx.QueryRow(`SELECT sample_id FROM seals WHERE seal_no = ?`, in.SealNo).Scan(&sealSample)
	switch {
	case err == nil:
		conflicts = append(conflicts, ConflictItem{"seal_no", in.SealNo, "封签号已使用，禁止重复封签"})
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, nil, err
	}

	// 转运箱：未关闭的箱号唯一；同班组同批次可续装，跨班组登记即冲突。
	var shipmentID int64
	var shipCrew string
	var shipBatch int64
	err = tx.QueryRow(`SELECT id, crew_id, batch_id FROM shipments WHERE box_code = ? AND closed_at IS NULL`,
		in.BoxCode).Scan(&shipmentID, &shipCrew, &shipBatch)
	switch {
	case err == nil:
		switch {
		case shipCrew != actor.CrewCode:
			conflicts = append(conflicts, ConflictItem{"box_code", in.BoxCode,
				"转运箱已由另一班组（" + shipCrew + "）发运登记"})
		case shipBatch != batchID:
			conflicts = append(conflicts, ConflictItem{"box_code", in.BoxCode, "转运箱绑定的是另一试车批次"})
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, nil, err
	}

	// 任何冲突都先留痕、开偏差，再中止。偏差不可事后删除或改写。
	if len(conflicts) > 0 {
		var parts []string
		for _, c := range conflicts {
			parts = append(parts, c.Field+"="+c.Value+": "+c.Reason)
		}
		detail := strings.Join(parts, "; ")
		res, err := tx.Exec(`INSERT INTO registration_attempts
(attempted_at, actor_id, actor_crew_id, batch_code, lab_no, container_code, box_code, detail)
VALUES(?,?,?,?,?,?,?,?)`, ts(), actor.ID, actor.CrewCode, in.BatchCode, in.LabNo,
			in.ContainerCode, in.BoxCode, detail)
		if err != nil {
			return nil, nil, err
		}
		attemptID, _ := res.LastInsertId()
		if err := insertDeviation(tx, nil, "number_conflict", fmt.Sprintf("attempt-%d", attemptID),
			"登记被拦截："+detail, nil, &attemptID, actor.ID); err != nil {
			return nil, nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, mapExecError(err)
		}
		return nil, conflicts, fmt.Errorf("%w: 编号/资源冲突，已暂停使用并登记偏差", ErrConflict)
	}

	if shipmentID == 0 {
		res, err := tx.Exec(`INSERT INTO shipments(box_code, batch_id, crew_id, created_at)
VALUES(?,?,?,?)`, in.BoxCode, batchID, actor.CrewCode, ts())
		if err != nil {
			return nil, nil, mapExecError(err)
		}
		shipmentID, _ = res.LastInsertId()
	}

	var containerID int64
	if err := tx.QueryRow(`SELECT id FROM containers WHERE code = ?`, in.ContainerCode).Scan(&containerID); err != nil {
		return nil, nil, err
	}

	res, err := tx.Exec(`INSERT INTO samples
(batch_id, shipment_id, lab_no, point_id, method_id, seal_no, sampled_at, registered_by,
 crew_id, created_at, temp_min, temp_max, max_gap_minutes, current_custodian_id)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		batchID, shipmentID, in.LabNo, pointID, methodID, in.SealNo, in.SampledAt, actor.ID,
		actor.CrewCode, ts(), tMin, tMax, maxGap, actor.ID)
	if err != nil {
		return nil, nil, mapExecError(err)
	}
	sampleID, _ := res.LastInsertId()

	if _, err := tx.Exec(`INSERT INTO container_bindings(container_id, sample_id, bound_at)
VALUES(?,?,?)`, containerID, sampleID, ts()); err != nil {
		return nil, nil, mapExecError(err)
	}
	if _, err := tx.Exec(`INSERT INTO seals(seal_no, sample_id, used_at) VALUES(?,?,?)`,
		in.SealNo, sampleID, ts()); err != nil {
		return nil, nil, mapExecError(err)
	}
	if _, err := tx.Exec(`INSERT INTO registration_attempts
(attempted_at, actor_id, actor_crew_id, batch_code, lab_no, container_code, box_code, detail)
VALUES(?,?,?,?,?,?,?,?)`, ts(), actor.ID, actor.CrewCode, in.BatchCode, in.LabNo,
		in.ContainerCode, in.BoxCode, "登记成功"); err != nil {
		return nil, nil, err
	}
	if _, err := appendCustody(tx, custodyInput{
		SampleID: sampleID, Kind: "collect", Station: in.PointCode,
		ToUser: &actor.ID, Actor: actor.ID, ExpectedSeal: in.SealNo,
	}); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, mapExecError(err)
	}
	sample, err := d.GetSample(sampleID)
	if err != nil {
		return nil, nil, err
	}
	return sample, nil, nil
}

// CloseShipment 关闭一个转运箱发运：箱内样品必须全部完成检测并归还容器，
// 以避免箱子在还有在途样品时被另一班组重新登记。
func (d *DB) CloseShipment(actor User, boxCode string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var crew string
	err = tx.QueryRow(`SELECT id, crew_id FROM shipments WHERE box_code = ? AND closed_at IS NULL`,
		boxCode).Scan(&id, &crew)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 没有未关闭的转运箱 %s", ErrNotFound, boxCode)
	}
	if err != nil {
		return err
	}
	if actor.Role != "admin" && actor.CrewCode != crew {
		return fmt.Errorf("%w: 只能关闭本班组的转运箱", ErrForbidden)
	}
	var outstanding int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM samples WHERE shipment_id = ?
AND consumed_at IS NULL`, id).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding > 0 {
		return fmt.Errorf("%w: 箱内仍有 %d 个样品未完成检测/归还容器，不能关闭",
			ErrConflict, outstanding)
	}
	if _, err := tx.Exec(`UPDATE shipments SET closed_at = ? WHERE id = ?`, ts(), id); err != nil {
		return err
	}
	return tx.Commit()
}
