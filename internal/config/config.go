// Package config 负责加载夹具规则与脱敏交易。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"example.com/usr/090901/q012/internal/domain"
)

// Fixtures 是服务启动时加载的全部本地夹具。
type Fixtures struct {
	Rules        *domain.Rules
	Transactions map[string]domain.Transaction // case_id → 脱敏交易
}

// Load 从 dir 读取 rules.json 与 transactions.json。
func Load(dir string) (*Fixtures, error) {
	var rules domain.Rules
	if err := loadJSON(filepath.Join(dir, "rules.json"), &rules); err != nil {
		return nil, err
	}
	if err := rules.Finalize(); err != nil {
		return nil, fmt.Errorf("rules.json 无效: %w", err)
	}
	var txns []domain.Transaction
	if err := loadJSON(filepath.Join(dir, "transactions.json"), &txns); err != nil {
		return nil, err
	}
	idx := map[string]domain.Transaction{}
	for _, t := range txns {
		idx[t.CaseID] = t
	}
	return &Fixtures{Rules: &rules, Transactions: idx}, nil
}

func loadJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 %s: %w", path, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("解析 %s: %w", path, err)
	}
	return nil
}
