package crq

import (
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultsToCodeRabbitRequiredBot(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing-env"))
	t.Setenv("CRQ_REQUIRED_BOTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RequiredBots) != 1 || cfg.RequiredBots[0] != "coderabbitai[bot]" {
		t.Fatalf("default required bots should only require CodeRabbit, got %#v", cfg.RequiredBots)
	}
}
