package store

import (
	"database/sql"
	"errors"
)

// EnsureUser 创建或更新用户（仅演示/初始化用；token 是不记名令牌）。
func (d *DB) EnsureUser(u User) error {
	if u.Role == "" || u.Login == "" || u.Token == "" {
		return errors.New("login/token/role required")
	}
	_, err := d.sql.Exec(`
INSERT INTO users (token, login, display_name, role, crew_id, active)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(login) DO UPDATE SET
    display_name = excluded.display_name,
    role         = excluded.role,
    crew_id      = excluded.crew_id,
    active       = excluded.active`,
		u.Token, u.Login, u.DisplayName, u.Role, nullIfEmpty(u.CrewCode), boolInt(u.Active))
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UserByToken 按不记名令牌解析当前操作者。
func (d *DB) UserByToken(token string) (User, error) {
	row := d.sql.QueryRow(`
SELECT id, token, login, display_name, role, COALESCE(crew_id,''), active
FROM users WHERE token = ? AND active = 1`, token)
	var u User
	var active int
	if err := row.Scan(&u.ID, &u.Token, &u.Login, &u.DisplayName, &u.Role, &u.CrewCode, &active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Active = active == 1
	return u, nil
}

// EnsureCrew / EnsureBatch / EnsurePoint / EnsureMethod / EnsureContainer 为初始化数据接口。

func (d *DB) EnsureCrew(code, name string) error {
	_, err := d.sql.Exec(`INSERT INTO crews(code,name) VALUES(?,?)
ON CONFLICT(code) DO UPDATE SET name = excluded.name`, code, name)
	return err
}

func (d *DB) EnsureBatch(code, product string, createdBy int64) (int64, error) {
	_, err := d.sql.Exec(`INSERT INTO batches(code,product,created_at,created_by)
VALUES(?,?,?,?) ON CONFLICT(code) DO NOTHING`, code, product, ts(), createdBy)
	if err != nil {
		return 0, err
	}
	return d.sqlID(`SELECT id FROM batches WHERE code = ?`, code)
}

func (d *DB) EnsurePoint(code, name string) error {
	_, err := d.sql.Exec(`INSERT INTO sampling_points(code,name) VALUES(?,?)
ON CONFLICT(code) DO UPDATE SET name = excluded.name`, code, name)
	return err
}

func (d *DB) EnsureMethod(m Method) error {
	_, err := d.sql.Exec(`INSERT INTO methods(code,name,standard,unit,temp_min,temp_max,max_gap_minutes)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(code) DO UPDATE SET
    name=excluded.name, standard=excluded.standard, unit=excluded.unit,
    temp_min=excluded.temp_min, temp_max=excluded.temp_max,
    max_gap_minutes=excluded.max_gap_minutes`,
		m.Code, m.Name, m.Standard, m.Unit, m.TempMin, m.TempMax, m.MaxGapMinutes)
	return err
}

func (d *DB) EnsureContainer(code, kind string) error {
	if kind == "" {
		kind = "bottle"
	}
	_, err := d.sql.Exec(`INSERT INTO containers(code,kind,created_at)
VALUES(?,?,?) ON CONFLICT(code) DO NOTHING`, code, kind, ts())
	return err
}

func (d *DB) sqlID(query string, args ...any) (int64, error) {
	row := d.sql.QueryRow(query, args...)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// MaskedCode 返回对外脱敏批号：批代码 SHA-256 前 12 位，外部实验室据此去身份化。
// 脱敏为单向：外部接口不暴露原始批号、班组与人员信息。
func maskedCode(code string) string {
	return shortHash("mask:" + code)
}

func shortHash(s string) string {
	return chainHash("", s)[:12]
}
