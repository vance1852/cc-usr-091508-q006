package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// TemperatureInput 为一条温度记录的输入。
type TemperatureInput struct {
	TakenAt string  `json:"taken_at"`
	Celsius float64 `json:"celsius"`
	Source  string  `json:"source,omitempty"` // online / offline
}

// AddTemperatures 追加一批温度记录（可用于离线补传），并立即判定：
//   - 温度超出样品登记时固化的区间 -> temp_excursion 偏差；
//   - 相邻记录（含已存数据）间隔超过方法允许的最大分钟数 -> temp_gap 偏差。
//
// 读数本身只追加；一旦产生偏差，样品暂停后续使用。
func (d *DB) AddTemperatures(actor User, labNo string, in []TemperatureInput) ([]TemperatureReading, error) {
	if actor.Role != "sampler" && actor.Role != "analyst" && actor.Role != "admin" {
		return nil, fmt.Errorf("%w: 只有采样员/化验员可以记录温度", ErrForbidden)
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: 至少一条温度记录", ErrValidation)
	}
	type parsed struct {
		t   time.Time
		raw TemperatureInput
	}
	parsedIn := make([]parsed, 0, len(in))
	for _, r := range in {
		t, err := parseTime(r.TakenAt)
		if err != nil {
			return nil, fmt.Errorf("%w: taken_at 必须是 RFC3339 时间", ErrValidation)
		}
		src := r.Source
		if src == "" {
			src = "online"
		}
		if src != "online" && src != "offline" {
			return nil, fmt.Errorf("%w: source 必须是 online/offline", ErrValidation)
		}
		parsedIn = append(parsedIn, struct {
			t   time.Time
			raw TemperatureInput
		}{t, TemperatureInput{TakenAt: r.TakenAt, Celsius: r.Celsius, Source: src}})
	}
	sort.Slice(parsedIn, func(i, j int) bool { return parsedIn[i].t.Before(parsedIn[j].t) })

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s, err := lockSampleByLabNo(tx, labNo)
	if err != nil {
		return nil, err
	}
	// 温度必须由经手过该样品的人记录（当前保管人或历史交接参与人）。
	if actor.Role != "admin" {
		var handled int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM custody_events
WHERE sample_id = ? AND (actor_id = ? OR from_user_id = ? OR to_user_id = ?)`,
			s.ID, actor.ID, actor.ID, actor.ID).Scan(&handled); err != nil {
			return nil, err
		}
		if handled == 0 {
			return nil, fmt.Errorf("%w: 你未经手该样品，不能登记温度", ErrForbidden)
		}
	}
	var tMin, tMax sql.NullFloat64
	var maxGap int
	if err := tx.QueryRow(`SELECT temp_min, temp_max, max_gap_minutes FROM samples WHERE id = ?`,
		s.ID).Scan(&tMin, &tMax, &maxGap); err != nil {
		return nil, err
	}

	// 取出全部既有时间戳，用于跨“在线/离线两批数据”的区间与间隔判定。
	rows, err := tx.Query(`SELECT taken_at, celsius FROM temperature_readings
WHERE sample_id = ? ORDER BY taken_at`, s.ID)
	if err != nil {
		return nil, err
	}
	type point struct {
		t time.Time
		v float64
	}
	series := []point{}
	for rows.Next() {
		var ta string
		var v float64
		if err := rows.Scan(&ta, &v); err != nil {
			rows.Close()
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, ta)
		series = append(series, point{t, v})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range parsedIn {
		series = append(series, point{p.t, p.raw.Celsius})
	}
	sort.Slice(series, func(i, j int) bool { return series[i].t.Before(series[j].t) })

	var inserted []int64
	for _, p := range parsedIn {
		res, err := tx.Exec(`INSERT INTO temperature_readings
(sample_id, taken_at, celsius, source, recorded_by, recorded_at)
VALUES(?,?,?,?,?,?)`, s.ID, p.t.Format(time.RFC3339Nano), p.raw.Celsius, p.raw.Source, actor.ID, ts())
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		inserted = append(inserted, id)
	}

	// 越界判定（只对本次新读数开偏差，历史已判定过）。
	for _, p := range parsedIn {
		below := tMin.Valid && p.raw.Celsius < tMin.Float64
		above := tMax.Valid && p.raw.Celsius > tMax.Float64
		if below || above {
			detail := fmt.Sprintf("温度 %.3f°C 越出允许区间 [%v, %v]", p.raw.Celsius, tMin.Float64, tMax.Float64)
			if err := insertDeviation(tx, &s.ID, "temp_excursion",
				fmt.Sprintf("reading-%s", p.t.Format(time.RFC3339Nano)), detail, nil, nil, actor.ID); err != nil {
				return nil, err
			}
		}
	}

	// 空白判定：合并序列上相邻间隔超过 max_gap_minutes 即开一条偏差（按区间端点去重）。
	if maxGap > 0 {
		for i := 1; i < len(series); i++ {
			gap := series[i].t.Sub(series[i-1].t).Minutes()
			if gap > float64(maxGap) {
				if err := insertDeviation(tx, &s.ID, "temp_gap",
					series[i-1].t.Format(time.RFC3339)+".."+series[i].t.Format(time.RFC3339),
					fmt.Sprintf("温度记录空白 %.0f 分钟，超过允许的 %d 分钟", gap, maxGap),
					nil, nil, actor.ID); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, mapExecError(err)
	}
	out := make([]TemperatureReading, 0, len(inserted))
	for _, id := range inserted {
		r, err := d.getTemperature(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

func (d *DB) getTemperature(id int64) (*TemperatureReading, error) {
	row := d.sql.QueryRow(`SELECT id, sample_id, taken_at, celsius, source, recorded_by, recorded_at
FROM temperature_readings WHERE id = ?`, id)
	var r TemperatureReading
	if err := row.Scan(&r.ID, &r.SampleID, &r.TakenAt, &r.Celsius, &r.Source,
		&r.RecordedBy, &r.RecordedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// Temperatures 列出样品的全部温度记录（按时间）。
func (d *DB) Temperatures(sampleID int64) ([]TemperatureReading, error) {
	rows, err := d.sql.Query(`SELECT id, sample_id, taken_at, celsius, source, recorded_by, recorded_at
FROM temperature_readings WHERE sample_id = ? ORDER BY taken_at, id`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemperatureReading
	for rows.Next() {
		var r TemperatureReading
		if err := rows.Scan(&r.ID, &r.SampleID, &r.TakenAt, &r.Celsius, &r.Source,
			&r.RecordedBy, &r.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
