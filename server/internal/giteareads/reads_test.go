package giteareads

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalizationAndBoundedPeriods(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c := Collector{Now: func() time.Time { return now }}
	for i := 0; i < 25; i++ {
		c.Record("GET", "/repos/o/r/issues/123/comments/456?token=secret&page=9&state=open", 20, false)
		now = now.Add(time.Hour)
	}
	c.Record("GET", "/repos/o/r/issues/987/comments/654?state=open&token=other", 3, true)
	r := c.Report()
	hours := r["hours"].([]any)
	if len(hours) != 24 {
		t.Fatal(len(hours))
	}
	last := hours[0].(map[string]any)
	if last["count"] != int64(1) || last["errors"] != int64(1) {
		t.Fatal(last)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "123") {
		t.Fatal(string(raw))
	}
	if Normalize("https://secret.invalid/a") != "invalid-route" {
		t.Fatal("absolute URL retained")
	}
	if Normalize("/a/12/34?labels=x&state=open&type=issues&after=private") != "/a/{n}/{n}?labels=x&state=open&type=issues" {
		t.Fatal(Normalize("/a/12/34?labels=x&state=open&type=issues&after=private"))
	}
}
