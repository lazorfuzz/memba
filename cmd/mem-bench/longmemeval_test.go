package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestParseLMEDate(t *testing.T) {
	tm := parseLMEDate("2023/05/20 (Sat) 02:21")
	if tm == nil || tm.Year() != 2023 || tm.Month() != 5 || tm.Day() != 20 || tm.Hour() != 2 || tm.Minute() != 21 {
		t.Fatalf("bad parse: %v", tm)
	}
	if tm := parseLMEDate("2023/05/20"); tm == nil || tm.Day() != 20 {
		t.Fatalf("date-only parse failed: %v", tm)
	}
	if tm := parseLMEDate("not a date"); tm != nil {
		t.Fatalf("garbage parsed: %v", tm)
	}
}

func TestLMEFixtureParses(t *testing.T) {
	data, err := os.ReadFile("../../testdata/bench/lme_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var instances []lmeInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		t.Fatal(err)
	}
	if len(instances) != 3 {
		t.Fatalf("expected 3 fixture instances, got %d", len(instances))
	}
	if !instances[2].abstention() {
		t.Fatal("_abs suffix must mark abstention instances")
	}
	if instances[0].answerString() != "corgi" {
		t.Fatalf("answer extraction: %q", instances[0].answerString())
	}
	if len(instances[1].HaystackSessions) != 2 || instances[1].HaystackSessions[1][0].Content == "" {
		t.Fatal("haystack sessions malformed")
	}
}
