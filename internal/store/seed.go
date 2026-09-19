package store

// Seed 在空库时写入演示人员、令牌、检测方法，便于直接联调。
// 已有任何人员数据时跳过，保证幂等、不覆盖正式数据。
func Seed(s *Store) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM people`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	type seedPerson struct {
		id, name, team, role, token string
	}
	people := []seedPerson{
		{"p-admin", "化验室管理员", "化验室管理组", "lab_admin", "token-admin"},
		{"p-sampler-a", "采样员甲", "采样一组", "sampler", "token-sampler-a"},
		{"p-sampler-b", "采样员乙", "采样二组", "sampler", "token-sampler-b"},
		{"p-courier", "押运员", "转运组", "courier", "token-courier"},
		{"p-analyst-a", "化验员甲", "化验一组", "analyst", "token-analyst-a"},
		{"p-analyst-b", "化验员乙", "化验二组", "analyst", "token-analyst-b"},
		{"p-reviewer", "复核员", "质量组", "reviewer", "token-reviewer"},
		{"p-approver", "试车审批人", "试车指挥部", "approver", "token-approver"},
		{"p-external", "外部化验员", "外部实验室", "external_lab", "token-external"},
	}
	for _, p := range people {
		if _, err := s.CreatePerson(Person{ID: p.id, Name: p.name, Team: p.team, Role: p.role}); err != nil {
			return err
		}
		if _, err := s.IssueToken(p.id, p.token); err != nil {
			return err
		}
	}
	if _, err := s.CreateMethod("m-gc", "气相色谱法", "液氧纯度气相色谱检测"); err != nil {
		return err
	}
	return nil
}
