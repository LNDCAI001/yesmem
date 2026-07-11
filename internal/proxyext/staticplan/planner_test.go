package staticplan_test

import (
	"bytes"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/proxyext/staticplan"
)

func TestPlannerDisabledByDefault(t *testing.T) {
	p := staticplan.NewPlanner(staticplan.DefaultConfig())
	body := bytes.Repeat([]byte("x"), 20000)
	result := p.Plan(body, false)
	if result.Eligible {
		t.Fatal("default config should produce ineligible plan")
	}
}

func TestPlannerBelowMinBytes(t *testing.T) {
	cfg := staticplan.DefaultConfig()
	cfg.Enabled = true
	cfg.Mode = staticplan.ModeNormalizeText
	p := staticplan.NewPlanner(cfg)
	body := []byte("small body")
	result := p.Plan(body, false)
	if result.Eligible {
		t.Fatal("small body should be ineligible")
	}
}

func TestPlannerSubagentBypass(t *testing.T) {
	cfg := staticplan.DefaultConfig()
	cfg.Enabled = true
	cfg.Mode = staticplan.ModeNormalizeText
	cfg.ApplyToSubagents = false
	p := staticplan.NewPlanner(cfg)
	body := bytes.Repeat([]byte("x"), 20000)
	result := p.Plan(body, true /* isSubagent */)
	if result.Eligible {
		t.Fatal("subagent should be bypassed")
	}
}

func TestNormalizeTextDeterministic(t *testing.T) {
	cfg := staticplan.DefaultConfig()
	cfg.Enabled = true
	cfg.Mode = staticplan.ModeNormalizeText
	cfg.MinBytes = 5
	p := staticplan.NewPlanner(cfg)
	body := []byte("hello   \nworld  \n")
	_, applied1 := p.Apply(body, p.Plan(body, false))
	_, applied2 := p.Apply(body, p.Plan(body, false))
	if applied1 != applied2 {
		t.Fatal("Apply is not deterministic")
	}
}

func TestHashContent(t *testing.T) {
	a := staticplan.HashContent([]byte("abc"))
	b := staticplan.HashContent([]byte("abc"))
	if a != b {
		t.Fatal("HashContent not deterministic")
	}
	if a == staticplan.HashContent([]byte("def")) {
		t.Fatal("different inputs must not produce same hash")
	}
}
