package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// ---------- 通用辅助 ----------

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func nowUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// withTx 在 IMMEDIATE 事务中执行 fn（DSN 已配置 _txlock=immediate）。
func (s *Store) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isUniqueErr(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) &&
		(e.ExtendedCode == sqlite3.ErrConstraintUnique ||
			e.ExtendedCode == sqlite3.ErrConstraintPrimaryKey)
}

func isTriggerErr(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) && e.ExtendedCode == sqlite3.ErrConstraintTrigger
}

func mustExist(tx *sql.Tx, table, id string) error {
	var x int
	if err := tx.QueryRow("SELECT 1 FROM "+table+" WHERE id=?", id).Scan(&x); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return bizErr(ErrNotFound, "%s %s 不存在", table, id)
		}
		return err
	}
	return nil
}

func getPersonTx(tx *sql.Tx, id string) (Person, error) {
	var p Person
	err := tx.QueryRow(`SELECT id,name,team,role FROM people WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.Team, &p.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, bizErr(ErrNotFound, "人员 %s 不存在", id)
	}
	return p, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- 人员 / 令牌 ----------

type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Team string `json:"team"`
	Role string `json:"role"`
}

var validRoles = map[string]bool{
	"sampler": true, "courier": true, "analyst": true, "lab_admin": true,
	"reviewer": true, "approver": true, "external_lab": true,
}

func (s *Store) CreatePerson(p Person) (Person, error) {
	if !validRoles[p.Role] {
		return Person{}, bizErr(ErrValidation, "未知角色 %q", p.Role)
	}
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Team) == "" {
		return Person{}, bizErr(ErrValidation, "姓名与班组不能为空")
	}
	if p.ID == "" {
		p.ID = newID()
	}
	err := s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO people(id,name,team,role) VALUES(?,?,?,?)`,
			p.ID, p.Name, p.Team, p.Role)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "人员 ID 已存在")
		}
		return err
	})
	return p, err
}

func (s *Store) GetPerson(id string) (Person, error) {
	row := s.db.QueryRow(`SELECT id,name,team,role FROM people WHERE id=?`, id)
	var p Person
	if err := row.Scan(&p.ID, &p.Name, &p.Team, &p.Role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Person{}, bizErr(ErrNotFound, "人员不存在")
		}
		return Person{}, err
	}
	return p, nil
}

// IssueToken 为人员签发登录令牌。
func (s *Store) IssueToken(personID, token string) (string, error) {
	if token == "" {
		token = newID() + newID()
	}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := mustExist(tx, "people", personID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO tokens(token,person_id) VALUES(?,?)`, token, personID)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "令牌已存在")
		}
		return err
	})
	return token, err
}

// PersonByToken 用令牌解析人员（鉴权中间件使用）。
func (s *Store) PersonByToken(token string) (Person, error) {
	var p Person
	err := s.db.QueryRow(`SELECT pe.id,pe.name,pe.team,pe.role
	                      FROM tokens t JOIN people pe ON pe.id=t.person_id
	                    WHERE t.token=?`, token).
		Scan(&p.ID, &p.Name, &p.Team, &p.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, bizErr(ErrPermission, "令牌无效")
	}
	return p, err
}

// ---------- 批次 / 采样点 / 温度区间 / 方法 ----------

type Batch struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Product string `json:"product"`
}

func (s *Store) CreateBatch(b Batch, createdBy string) (Batch, error) {
	if strings.TrimSpace(b.Name) == "" || strings.TrimSpace(b.Product) == "" {
		return Batch{}, bizErr(ErrValidation, "批次名称与产品类型不能为空")
	}
	if b.ID == "" {
		b.ID = newID()
	}
	err := s.withTx(func(tx *sql.Tx) error {
		if _, err := getPersonTx(tx, createdBy); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO batches(id,name,product,created_by) VALUES(?,?,?,?)`,
			b.ID, b.Name, b.Product, createdBy)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "批次 ID 已存在")
		}
		return err
	})
	return b, err
}

func (s *Store) CreateSamplingPoint(batchID, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", bizErr(ErrValidation, "采样点名称不能为空")
	}
	id := newID()
	err := s.withTx(func(tx *sql.Tx) error {
		if err := mustExist(tx, "batches", batchID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sampling_points(id,batch_id,name) VALUES(?,?,?)`, id, batchID, name)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "批次下采样点名称重复")
		}
		return err
	})
	return id, err
}

// SetTempLimits 设定（或调整）批次温度允许区间。历史区间行保留。
func (s *Store) SetTempLimits(batchID string, minC, maxC float64) (string, error) {
	if minC > maxC {
		return "", bizErr(ErrValidation, "温度下限不能高于上限")
	}
	id := newID()
	err := s.withTx(func(tx *sql.Tx) error {
		if err := mustExist(tx, "batches", batchID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO temp_limits(id,batch_id,min_celsius,max_celsius) VALUES(?,?,?,?)`,
			id, batchID, minC, maxC)
		return err
	})
	return id, err
}

// currentTempLimits 取批次最新温度区间；未设定时 ok=false（接收时按 temp_gap 处理）。
func currentTempLimits(tx *sql.Tx, batchID string) (lo, hi float64, ok bool, err error) {
	err = tx.QueryRow(`SELECT min_celsius,max_celsius FROM temp_limits
	                   WHERE batch_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, batchID).
		Scan(&lo, &hi)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	return lo, hi, err == nil, err
}

func (s *Store) CreateMethod(id, name, desc string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", bizErr(ErrValidation, "方法名称不能为空")
	}
	if id == "" {
		id = newID()
	}
	err := s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO methods(id,name,description) VALUES(?,?,?)`, id, name, desc)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "检测方法已存在")
		}
		return err
	})
	return id, err
}

// ---------- 容器 ----------

func (s *Store) RegisterContainer(id, kind string) (string, error) {
	if kind != "bottle" && kind != "transit_box" {
		return "", bizErr(ErrValidation, "容器类型只能是 bottle 或 transit_box")
	}
	if id == "" {
		id = newID()
	}
	err := s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO containers(id,kind) VALUES(?,?)`, id, kind)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "容器 ID 已存在")
		}
		return err
	})
	return id, err
}

// ---------- 样品采集 ----------

type CollectInput struct {
	PublicCode  string // 对外脱敏批号
	BatchID     string
	PointID     string
	CollectorID string
	ContainerID string
	SealNo      string
	SealIntact  bool
	CollectedAt string // RFC3339；留空取当前时间
	OpID        string // 离线幂等键
}

type CollectResult struct {
	SampleID    string `json:"sample_id"`
	PublicCode  string `json:"public_code"`
	Status      string `json:"status"`
	Hold        bool   `json:"hold"`
	DeviationID int64  `json:"deviation_id,omitempty"`
}

// Collect 建立链路第一站：采样人将封签样品装入容器并成为第一任保管人。
func (s *Store) Collect(in CollectInput) (CollectResult, error) {
	if strings.TrimSpace(in.PublicCode) == "" {
		return CollectResult{}, bizErr(ErrValidation, "脱敏批号不能为空")
	}
	if strings.TrimSpace(in.SealNo) == "" {
		return CollectResult{}, bizErr(ErrValidation, "封签号不能为空")
	}
	out := CollectResult{}
	err := s.withTx(func(tx *sql.Tx) error {
		if err := claimOp(tx, in.OpID, in.CollectorID, "collect", hashOf(in)); err != nil {
			return err
		}
		collector, err := getPersonTx(tx, in.CollectorID)
		if err != nil {
			return err
		}
		if collector.Role != "sampler" {
			return bizErr(ErrPermission, "只有采样人员能登记采集")
		}
		if err := mustExist(tx, "batches", in.BatchID); err != nil {
			return err
		}
		var pointBatch string
		if err := tx.QueryRow(`SELECT batch_id FROM sampling_points WHERE id=?`, in.PointID).
			Scan(&pointBatch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "采样点不存在")
			}
			return err
		}
		if pointBatch != in.BatchID {
			return bizErr(ErrValidation, "采样点不属于该试车批次")
		}
		var active sql.NullString
		var retired int
		if err := tx.QueryRow(`SELECT active_sample_id, retired FROM containers WHERE id=?`, in.ContainerID).
			Scan(&active, &retired); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return bizErr(ErrNotFound, "容器不存在")
			}
			return err
		}
		if retired != 0 {
			return bizErr(ErrState, "容器已报废，不能再次使用")
		}
		if active.Valid {
			return bizErr(ErrConflict, "容器仍盛装样品 %s，未清空不能再次使用", active.String)
		}
		collectedAt := in.CollectedAt
		if collectedAt == "" {
			collectedAt = nowUTC()
		}
		sampleID := newID()
		status := "active"
		hold := !in.SealIntact
		holdReason := ""
		if hold {
			holdReason = "采集时封签破损"
		}
		_, err = tx.Exec(`INSERT INTO samples
			(id,public_code,batch_id,point_id,collector_id,collector_team,container_id,
			 seal_no,seal_intact,collected_at,status,hold,hold_reason,current_holder_id)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sampleID, in.PublicCode, in.BatchID, in.PointID, in.CollectorID, collector.Team,
			in.ContainerID, in.SealNo, b2i(in.SealIntact), collectedAt,
			status, b2i(hold), holdReason, in.CollectorID)
		if isUniqueErr(err) {
			return bizErr(ErrConflict, "脱敏批号已存在：%s", in.PublicCode)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE containers SET active_sample_id=? WHERE id=?`,
			sampleID, in.ContainerID); err != nil {
			return err
		}
		if err := appendCustody(tx, sampleID, "collect", in.CollectorID, collector.Team, "", "样品采集封存"); err != nil {
			return err
		}
		if hold {
			devID, err := openDeviationTx(tx, deviationInput{
				OpID:       "dev:collect:" + sampleID + ":seal",
				SampleID:   sampleID,
				Kind:       "seal_broken",
				Title:      "采集时封签破损",
				Detail:     fmt.Sprintf("封签号 %s", in.SealNo),
				OpenedBy:   in.CollectorID,
				OpenedTeam: collector.Team,
			})
			if err != nil {
				return err
			}
			out.DeviationID = devID
		}
		_, _ = tx.Exec(`UPDATE ops SET ref_id=? WHERE op_id=?`, sampleID, in.OpID)
		out.SampleID = sampleID
		out.PublicCode = in.PublicCode
		out.Status = status
		out.Hold = hold
		return nil
	})
	return out, err
}
