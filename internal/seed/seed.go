package seed

import (
	"samplechain/internal/store"
)

// Data 预置三个采样班组、化验室角色与一条试车批次，
// 用于演示和集成测试。令牌即登录名前加 tok-。
type Data struct {
	Crews   map[string]string
	Users   map[string]store.User
	Batch   string
	Points  []string
	Methods []store.Method
	Boxes   []string
	Bottles []string
}

// Load 在空库上写入基础数据；重复执行保持幂等。
func Load(db *store.DB) (*Data, error) {
	crews := map[string]string{
		"A": "甲采样班组",
		"B": "乙采样班组",
		"C": "丙采样班组",
	}
	for code, name := range crews {
		if err := db.EnsureCrew(code, name); err != nil {
			return nil, err
		}
	}

	type spec struct {
		login, name, role, crew string
	}
	specs := []spec{
		{"zhang", "张工", "sampler", "A"},
		{"li", "李工", "sampler", "B"},
		{"wang", "王工", "sampler", "C"},
		{"analyst1", "化验员刘", "analyst", ""},
		{"reviewer1", "复核人陈", "reviewer", ""},
		{"approver1", "试车审批人赵", "approver", ""},
		{"extlab", "外部实验室账号", "external", ""},
		{"admin", "系统管理员", "admin", ""},
	}
	users := map[string]store.User{}
	for _, s := range specs {
		u := store.User{
			Token: "tok-" + s.login, Login: s.login, DisplayName: s.name,
			Role: s.role, CrewCode: s.crew, Active: true,
		}
		if err := db.EnsureUser(u); err != nil {
			return nil, err
		}
		saved, err := db.UserByToken(u.Token)
		if err != nil {
			return nil, err
		}
		users[s.login] = saved
	}

	loxMin, loxMax := -190.0, -181.0
	methods := []store.Method{
		{Code: "LOX-PURITY", Name: "液氧纯度测定", Standard: "QJ 20071", Unit: "%",
			TempMin: &loxMin, TempMax: &loxMax, MaxGapMinutes: 30},
		{Code: "LOX-HC", Name: "液氧总烃含量", Standard: "QJ 20072", Unit: "ppm",
			TempMin: &loxMin, TempMax: &loxMax, MaxGapMinutes: 30},
	}
	for _, m := range methods {
		if err := db.EnsureMethod(m); err != nil {
			return nil, err
		}
	}
	points := []string{"TANK-01", "LINE-02", "DOCK-03"}
	pointNames := map[string]string{"TANK-01": "液氧储罐", "LINE-02": "加注管路", "DOCK-03": "加注口"}
	for _, p := range points {
		if err := db.EnsurePoint(p, pointNames[p]); err != nil {
			return nil, err
		}
	}
	batch := "FIRE-2026-009"
	if _, err := db.EnsureBatch(batch, "液氧 LOX", users["admin"].ID); err != nil {
		return nil, err
	}
	bottles := []string{"BTL-001", "BTL-002", "BTL-003", "BTL-004"}
	for _, b := range bottles {
		if err := db.EnsureContainer(b, "bottle"); err != nil {
			return nil, err
		}
	}

	return &Data{
		Crews: crews, Users: users, Batch: batch, Points: points,
		Methods: methods, Bottles: bottles,
	}, nil
}
