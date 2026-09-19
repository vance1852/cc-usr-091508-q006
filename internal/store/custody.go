package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type custodyInput struct {
	SampleID     int64
	Kind         string
	Station      string
	FromUser     *int64
	ToUser       *int64
	Actor        int64
	SealIntact   *bool
	ExpectedSeal string
	TransferID   *int64
	ClientTime   string
	IdemKey      string
	Note         string
}

// appendCustody 在事务内向哈希链追加一个交接事实，并返回新事件 ID。
// seq 严格递增；prev_hash 承接当前链尖，物理改动任何历史事件都会断链。
func appendCustody(tx *sql.Tx, in custodyInput) (int64, error) {
	var seq int
	var prev string
	err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0),
COALESCE((SELECT hash FROM custody_events WHERE sample_id = ? ORDER BY seq DESC LIMIT 1),'GENESIS')
FROM custody_events WHERE sample_id = ?`,
		in.SampleID, in.SampleID).Scan(&seq, &prev)
	if err != nil {
		return 0, err
	}
	seq++
	hash := chainHash(prev,
		fmt.Sprint(in.SampleID), fmt.Sprint(seq), in.Kind, in.Station,
		ptrSprint(in.FromUser), ptrSprint(in.ToUser), fmt.Sprint(in.Actor),
		boolPtrSprint(in.SealIntact), in.ExpectedSeal, ptrSprint(in.TransferID),
		in.ClientTime, in.IdemKey, in.Note)
	res, err := tx.Exec(`INSERT INTO custody_events
(sample_id, seq, kind, station, from_user_id, to_user_id, actor_id, seal_intact,
 expected_seal_no, transfer_id, client_event_time, idem_key, note, recorded_at, prev_hash, hash)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.SampleID, seq, in.Kind, in.Station, in.FromUser, in.ToUser, in.Actor,
		in.SealIntact, in.ExpectedSeal, in.TransferID, in.ClientTime, nullIfEmpty(in.IdemKey),
		in.Note, ts(), prev, hash)
	if err != nil {
		return 0, mapExecError(err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func ptrSprint(p *int64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprint(*p)
}

func boolPtrSprint(p *bool) string {
	if p == nil {
		return ""
	}
	return fmt.Sprint(*p)
}

// ReleaseInput 为交出样品的请求。
type ReleaseInput struct {
	LabNo      string `json:"lab_no"`
	ToLogin    string `json:"to_login"`
	Station    string `json:"station"`
	SealNo     string `json:"seal_no"`
	Note       string `json:"note"`
	ClientTime string `json:"client_event_time"`
	IdemKey    string `json:"idem_key"`
}

// ReceiveInput 为接收确认的请求；每次接收必须显式确认封签状态。
type ReceiveInput struct {
	LabNo          string `json:"lab_no"`
	SealIntact     *bool  `json:"seal_intact"`
	SealNo         string `json:"seal_no"`
	Note           string `json:"note"`
	ClientTime     string `json:"client_event_time"`
	IdemKey        string `json:"idem_key"`
	RejectTransfer bool   `json:"reject_transfer"`
}

// Release 发起交接：当前保管人交出，指定唯一接收人。保管责任此刻不转移。
// 同一样品同时只能存在一个在途交接（ux_transfer_open），两名保管人无法
// 同时把同一样品交出；并发的第二笔发起会被唯一索引拒绝。
func (d *DB) Release(actor User, in ReleaseInput, offline bool) (*CustodyEvent, error) {
	if strings.TrimSpace(in.ToLogin) == "" {
		return nil, fmt.Errorf("%w: to_login 不能为空", ErrValidation)
	}
	kind := "release"
	if offline {
		kind = "recover_release"
		if strings.TrimSpace(in.IdemKey) == "" {
			return nil, fmt.Errorf("%w: 离线恢复必须提供 idem_key", ErrValidation)
		}
	}
	if in.ClientTime != "" {
		if _, err := parseTime(in.ClientTime); err != nil {
			return nil, fmt.Errorf("%w: client_event_time 必须是 RFC3339 时间", ErrValidation)
		}
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	s, toID, err := lockSampleAndUser(tx, in.LabNo, in.ToLogin)
	if err != nil {
		return nil, err
	}
	// 离线事件重放：相同客户端幂等键只生效一次，直接返回首次写入的事件，
	// 不再新建交接单（否则会与在途/已完成交接冲突）。
	if in.IdemKey != "" {
		if evID, err := existingIdemEvent(tx, s.ID, in.IdemKey); err == nil {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return d.GetCustodyEvent(evID)
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if err := requireUsable(tx, s.ID); err != nil {
		return nil, err
	}
	if s.CurrentCustodianID == nil {
		return nil, fmt.Errorf("%w: 样品无当前保管人（容器已归还），不能交接", ErrConflict)
	}
	if *s.CurrentCustodianID != actor.ID {
		return nil, fmt.Errorf("%w: 只有当前保管人可以交出样品", ErrForbidden)
	}
	if toID == actor.ID {
		return nil, fmt.Errorf("%w: 不能交接给自己", ErrValidation)
	}
	if in.SealNo != "" && in.SealNo != s.SealNo {
		return nil, fmt.Errorf("%w: 交出时封签号与登记不符", ErrConflict)
	}

	// 先创建 transfer（release_event_id 先置 0），唯一索引在此拦截并发的第二笔交接。
	tres, err := tx.Exec(`INSERT INTO transfers
(sample_id, release_event_id, from_user_id, to_user_id, station, status, reason, created_at)
VALUES(?, 0, ?, ?, ?, 'released', ?, ?)`,
		s.ID, actor.ID, toID, in.Station, in.Note, ts())
	if err != nil {
		return nil, mapExecError(err)
	}
	transferID, _ := tres.LastInsertId()

	evID, err := appendCustody(tx, custodyInput{
		SampleID: s.ID, Kind: kind, Station: in.Station, FromUser: &actor.ID, ToUser: &toID,
		Actor: actor.ID, ExpectedSeal: s.SealNo, TransferID: &transferID,
		ClientTime: in.ClientTime, IdemKey: in.IdemKey, Note: in.Note,
	})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE transfers SET release_event_id = ? WHERE id = ?`, evID, transferID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	return d.GetCustodyEvent(evID)
}

// Receive 完成交接：指定接收人确认承接上一保管人的责任，成为唯一当前保管人。
// 必须显式确认封签；封签破损立即形成偏差并暂停样品后续使用。
func (d *DB) Receive(actor User, in ReceiveInput, offline bool) (*CustodyEvent, error) {
	if in.SealIntact == nil {
		return nil, fmt.Errorf("%w: 接收时必须明确封签是否完好 (seal_intact)", ErrValidation)
	}
	kind := "receive"
	if offline {
		kind = "recover_receive"
		if strings.TrimSpace(in.IdemKey) == "" {
			return nil, fmt.Errorf("%w: 离线恢复必须提供 idem_key", ErrValidation)
		}
	}
	if in.ClientTime != "" {
		if _, err := parseTime(in.ClientTime); err != nil {
			return nil, fmt.Errorf("%w: client_event_time 必须是 RFC3339 时间", ErrValidation)
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
	// 离线接收事件重放：返回首次同步写入的事件（可能是 receive 或封签拒收）。
	if in.IdemKey != "" {
		if evID, err := existingIdemEvent(tx, s.ID, in.IdemKey); err == nil {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return d.GetCustodyEvent(evID)
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if err := requireUsable(tx, s.ID); err != nil {
		return nil, err
	}

	var tr transferRow
	err = tx.QueryRow(`SELECT id, from_user_id, to_user_id, station FROM transfers
WHERE sample_id = ? AND status = 'released' ORDER BY id DESC LIMIT 1`,
		s.ID).Scan(&tr.ID, &tr.From, &tr.To, &tr.Station)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 该样品没有等待接收的在途交接", ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	if tr.To != actor.ID {
		return nil, fmt.Errorf("%w: 只有交接单指定的接收人可以确认接收", ErrForbidden)
	}
	if in.SealNo != "" && in.SealNo != s.SealNo {
		return nil, fmt.Errorf("%w: 封签号与登记不符", ErrConflict)
	}

	if in.RejectTransfer {
		// 拒收：责任不转移，在途单关闭，交出人仍是唯一当前保管人。
		evID, err := appendCustody(tx, custodyInput{
			SampleID: s.ID, Kind: "refusal", Station: tr.Station,
			FromUser: &tr.From, ToUser: &tr.To, Actor: actor.ID,
			SealIntact: in.SealIntact, ExpectedSeal: s.SealNo, TransferID: &tr.ID,
			ClientTime: in.ClientTime, IdemKey: in.IdemKey,
			Note: firstNonEmpty(in.Note, "接收人拒收"),
		})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE transfers SET status='refused', receive_event_id=?, resolved_at=?
WHERE id=?`, evID, ts(), tr.ID); err != nil {
			return nil, err
		}
		// 拒收同时发现封签破损：照样形成偏差暂停样品，不能因拒收而放过异常。
		if !*in.SealIntact {
			if err := insertDeviation(tx, &s.ID, "seal_broken", fmt.Sprintf("event-%d", evID),
				"接收人拒收且封签破损，样品暂停后续使用", &evID, nil, actor.ID); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, mapExecError(err)
		}
		return d.GetCustodyEvent(evID)
	}

	// 每次接收都必须承接上一位保管人：在途交接的交出人必须与链上当前保管人一致。
	if s.CurrentCustodianID == nil {
		return nil, fmt.Errorf("%w: 样品无当前保管人", ErrConflict)
	}
	if *s.CurrentCustodianID != tr.From {
		return nil, fmt.Errorf("%w: 上一保管人与交接单不符，拒绝承接", ErrConflict)
	}

	evID, err := appendCustody(tx, custodyInput{
		SampleID: s.ID, Kind: kind, Station: tr.Station,
		FromUser: &tr.From, ToUser: &actor.ID, Actor: actor.ID,
		SealIntact: in.SealIntact, ExpectedSeal: s.SealNo, TransferID: &tr.ID,
		ClientTime: in.ClientTime, IdemKey: in.IdemKey, Note: in.Note,
	})
	if err != nil {
		return nil, err
	}

	if !*in.SealIntact {
		// 封签破损：留下接收证据事件，但保管责任不转移（按拒收关闭在途单），
		// 同时开偏差暂停样品后续使用。
		if _, err := tx.Exec(`UPDATE transfers SET status='refused', receive_event_id=?, resolved_at=?
WHERE id=?`, evID, ts(), tr.ID); err != nil {
			return nil, err
		}
		if err := insertDeviation(tx, &s.ID, "seal_broken", fmt.Sprintf("event-%d", evID),
			"接收确认时封签破损或与登记封签不符，样品暂停后续使用", &evID, nil, actor.ID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, mapExecError(err)
		}
		return d.GetCustodyEvent(evID)
	}

	if _, err := tx.Exec(`UPDATE samples SET current_custodian_id = ?, custodian_seq = custodian_seq + 1
WHERE id = ?`, actor.ID, s.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE transfers SET status='received', receive_event_id=?, resolved_at=?
WHERE id=?`, evID, ts(), tr.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	return d.GetCustodyEvent(evID)
}

type transferRow struct {
	ID      int64
	From    int64
	To      int64
	Station string
}

func lockSampleAndUser(tx *sql.Tx, labNo, toLogin string) (sampleRow, int64, error) {
	s, err := lockSampleByLabNo(tx, labNo)
	if err != nil {
		return sampleRow{}, 0, err
	}
	var toID int64
	var active int
	err = tx.QueryRow(`SELECT id, active FROM users WHERE login = ?`, toLogin).Scan(&toID, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return sampleRow{}, 0, fmt.Errorf("%w: 接收人 %s 不存在", ErrNotFound, toLogin)
	}
	if err != nil {
		return sampleRow{}, 0, err
	}
	if active != 1 {
		return sampleRow{}, 0, fmt.Errorf("%w: 接收人 %s 已停用", ErrValidation, toLogin)
	}
	return s, toID, nil
}

type sampleRow struct {
	ID                 int64
	SealNo             string
	CrewCode           string
	CurrentCustodianID *int64
}

func lockSampleByLabNo(tx *sql.Tx, labNo string) (sampleRow, error) {
	var s sampleRow
	err := tx.QueryRow(`SELECT id, seal_no, crew_id, current_custodian_id FROM samples
WHERE lab_no = ?`, strings.TrimSpace(labNo)).Scan(&s.ID, &s.SealNo, &s.CrewCode, &s.CurrentCustodianID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("%w: 样品 %s 不存在", ErrNotFound, labNo)
	}
	return s, err
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ReturnContainer 在样品检测终结后归还物理容器，解除占用，容器可被再次使用。
// 归还后样品无当前保管人；链路中该样品的保管责任随之结束。
func (d *DB) ReturnContainer(actor User, labNo, note string) (*CustodyEvent, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s, err := lockSampleByLabNo(tx, labNo)
	if err != nil {
		return nil, err
	}
	if s.CurrentCustodianID == nil {
		return nil, fmt.Errorf("%w: 容器已归还", ErrConflict)
	}
	if *s.CurrentCustodianID != actor.ID && actor.Role != "admin" {
		return nil, fmt.Errorf("%w: 只有当前保管人可以归还容器", ErrForbidden)
	}
	if err := requireUsable(tx, s.ID); err != nil {
		return nil, err
	}
	evID, err := appendCustody(tx, custodyInput{
		SampleID: s.ID, Kind: "container_return", FromUser: &actor.ID,
		Actor: actor.ID, ExpectedSeal: s.SealNo, Note: note,
	})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE samples SET current_custodian_id = NULL, consumed_at = ? WHERE id = ?`,
		ts(), s.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE container_bindings SET released_at = ?
WHERE sample_id = ? AND released_at IS NULL`, ts(), s.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	return d.GetCustodyEvent(evID)
}
