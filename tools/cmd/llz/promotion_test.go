package main

import (
	"reflect"
	"testing"
)

// promoInstance seeds a temp tfDir with cluster/<name>.tfvars carrying the given
// raw body, plus a template example file that must be ignored.
func promoInstance(t *testing.T, clusters map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"terraform.tfvars.example": "promotion_rank = 0\n"}
	for name, body := range clusters {
		files[name+".tfvars"] = body
	}
	writeCluster(t, dir, files)
	return dir
}

func TestHCLIntField(t *testing.T) {
	body := "promotion_rank = 2\nregion = \"us-x\"\nneg = -1\n"
	if n, ok := hclIntField(body, "promotion_rank"); !ok || n != 2 {
		t.Errorf("promotion_rank = %d,%v, want 2,true", n, ok)
	}
	if n, ok := hclIntField(body, "neg"); !ok || n != -1 {
		t.Errorf("neg = %d,%v, want -1,true", n, ok)
	}
	if _, ok := hclIntField(body, "missing"); ok {
		t.Errorf("missing field reported present")
	}
	// A quoted value is a string, not an int — must not match.
	if _, ok := hclIntField("region = \"us-x\"\n", "region"); ok {
		t.Errorf("quoted string matched as int")
	}
}

func TestReadPromotionOrderAndNext(t *testing.T) {
	dir := promoInstance(t, map[string]string{
		"prod":    "region = \"us-x\"\npromotion_rank = 3\n",
		"dev":     "region = \"us-x\"\npromotion_rank = 1\n",
		"staging": "region = \"us-x\"\npromotion_rank = 2\n",
		"scratch": "region = \"us-x\"\n",                     // unranked → excluded
		"sandbox": "region = \"us-x\"\npromotion_rank = 0\n", // rank 0 → excluded
	})
	stages, err := readPromotion(dir)
	if err != nil {
		t.Fatalf("readPromotion: %v", err)
	}
	if got := promotionOrder(stages); !reflect.DeepEqual(got, []string{"dev", "staging", "prod"}) {
		t.Errorf("promotionOrder = %v, want [dev staging prod]", got)
	}

	for _, tc := range []struct {
		from, want string
		ok         bool
	}{
		{"dev", "staging", true},
		{"staging", "prod", true},
		{"prod", "", false},    // last stage — nothing to promote to
		{"scratch", "", false}, // unranked — not in the pipeline
		{"nope", "", false},    // unknown
	} {
		next, ok := nextStage(stages, tc.from)
		if next != tc.want || ok != tc.ok {
			t.Errorf("nextStage(%q) = %q,%v, want %q,%v", tc.from, next, ok, tc.want, tc.ok)
		}
	}

	if _, ok := findStage(stages, "dev"); !ok {
		t.Errorf("findStage(dev) not found")
	}
	if _, ok := findStage(stages, "scratch"); ok {
		t.Errorf("findStage(scratch) found, want absent")
	}
}

func TestReadPromotionDuplicateRankErrors(t *testing.T) {
	dir := promoInstance(t, map[string]string{
		"east": "promotion_rank = 1\n",
		"west": "promotion_rank = 1\n",
	})
	if _, err := readPromotion(dir); err == nil {
		t.Fatal("readPromotion: want error on duplicate promotion_rank, got nil")
	}
}

func TestReadPromotionEmptyWhenNoneRanked(t *testing.T) {
	dir := promoInstance(t, map[string]string{
		"east": "region = \"us-x\"\n",
		"west": "region = \"us-x\"\npromotion_rank = 0\n",
	})
	stages, err := readPromotion(dir)
	if err != nil {
		t.Fatalf("readPromotion: %v", err)
	}
	if len(stages) != 0 {
		t.Errorf("stages = %v, want empty (no ranked deployments)", stages)
	}
}
