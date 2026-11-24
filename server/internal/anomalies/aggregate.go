package anomalies

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"time"
)

func latest(rows []map[string]any) map[string]any {
	var last map[string]any
	for _, r := range rows {
		if !at(r).IsZero() && (last == nil || at(r).After(at(last))) {
			last = r
		}
	}
	return last
}
func newAggregateHit(kind string, last map[string]any, end time.Time, measured, baseline, threshold any, rule, period, unit, cause string) map[string]any {
	h := hit(kind, last, measured, baseline, threshold, rule, period, unit)
	h["repo"], h["issue"], h["at"], h["causeKey"] = nil, nil, iso(end), cause
	return h
}

type inputRatio struct {
	Count, Unknown     int
	Input, Read, Write float64
	Ratio              *float64
}

func uncachedRatio(rows []map[string]any, step string) inputRatio {
	out := inputRatio{}
	for _, r := range rows {
		if usage.CanonicalStep(fleet.Text(r["step"])) != step {
			continue
		}
		out.Count++
		input, io := number(r["inputTokens"])
		read, ro := number(r["cacheReadTokens"])
		write, wo := number(r["cacheWriteTokens"])
		if r["provider"] == "codex" {
			input -= read
			write, wo = 0, true
			if input < 0 {
				io = false
			}
		} else if r["provider"] != "claude" {
			io = false
		}
		if !io || !ro || !wo {
			out.Unknown++
			continue
		}
		out.Input += input
		out.Read += read
		out.Write += write
	}
	denom := out.Input + out.Read + out.Write
	if denom > 0 {
		ratio := out.Input / denom
		out.Ratio = &ratio
	}
	return out
}
func Cache(rows, history []map[string]any) []map[string]any {
	last := latest(rows)
	if last == nil {
		return nil
	}
	end := at(last)
	current, previous := []map[string]any{}, []map[string]any{}
	for _, r := range history {
		if inWindow(r, end.Add(-24*time.Hour), end) {
			current = append(current, r)
		} else if inWindow(r, end.Add(-8*24*time.Hour), end.Add(-24*time.Hour)) {
			previous = append(previous, r)
		}
	}
	c, b := uncachedRatio(current, usage.ConsultationStep), uncachedRatio(previous, usage.ConsultationStep)
	if c.Count-c.Unknown < 10 || b.Count-b.Unknown < 10 || c.Ratio == nil || b.Ratio == nil {
		return nil
	}
	threshold := math.Max(.1, *b.Ratio*1.5)
	if *c.Ratio+2.220446049250313e-16*threshold < threshold {
		return nil
	}
	var consultation map[string]any
	for _, r := range current {
		u := uncachedRatio([]map[string]any{r}, usage.ConsultationStep)
		if usage.CanonicalStep(fleet.Text(r["step"])) == usage.ConsultationStep && u.Unknown == 0 && (consultation == nil || at(r).After(at(consultation))) {
			consultation = r
		}
	}
	h := newAggregateHit("cache", consultation, end, *c.Ratio, *b.Ratio, threshold, "現在と直前7日に既知10行以上、比率が1.5倍以上かつ0.1以上", "直近24時間", "比率", "cache:受付の相談")
	h["step"] = usage.ConsultationStep
	h["ratio"] = nil
	if *b.Ratio > 0 {
		h["ratio"] = *c.Ratio / *b.Ratio
	}
	return []map[string]any{h}
}
func CallTriggers(rows, history []map[string]any) []map[string]any {
	last := latest(rows)
	if last == nil {
		return nil
	}
	end := at(last)
	split := end.Add(-24 * time.Hour)
	groups := map[string][]map[string]any{}
	keys := []string{}
	for _, r := range history {
		if r["source"] != "intake" || !inWindow(r, split.Add(-7*24*time.Hour), end) {
			continue
		}
		key := usage.CanonicalStep(text(r["step"], "")) + "\x00" + text(r["provider"], "")
		if groups[key] == nil {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], r)
	}
	out := []map[string]any{}
	for _, key := range keys {
		group := groups[key]
		recent, previous, currentUnknown, previousUnknown, values, last := callTriggerSides(group, split)
		if len(recent) < 20 || len(previous) < 20 {
			continue
		}
		triggers := []string{}
		for trigger := range values {
			triggers = append(triggers, trigger)
		}
		sort.Strings(triggers)
		for _, trigger := range triggers {
			if trigger == "initial" || trigger == "user_reply" {
				continue
			}
			n, b := 0, 0
			for _, r := range recent {
				if r["callTrigger"] == trigger {
					n++
				}
			}
			for _, r := range previous {
				if r["callTrigger"] == trigger {
					b++
				}
			}
			measured, base := float64(n)/float64(len(recent)), float64(b)/float64(len(previous))
			threshold := math.Max(base*1.5, base+.1)
			if measured+2.220446049250313e-16*threshold < threshold {
				continue
			}
			h := newAggregateHit("call-trigger", last, end, measured, base, threshold, "既知の行が各20件以上で、契機比率が1.5倍以上かつ0.1以上増加", "直近24時間", "比率", "call-trigger:"+usage.CanonicalStep(text(last["step"], "null"))+":"+text(last["provider"], "null")+":"+trigger)
			h["provider"], h["callTrigger"], h["ratio"] = last["provider"], trigger, nil
			h["step"] = usage.CanonicalStep(text(last["step"], ""))
			if base > 0 {
				h["ratio"] = measured / base
			}
			h["current"], h["previous"] = map[string]any{"count": len(recent), "unknown": currentUnknown}, map[string]any{"count": len(previous), "unknown": previousUnknown}
			copy := copyRow(last)
			copy["source"], copy["callTrigger"] = last["source"], last["callTrigger"]
			h["row"] = copy
			out = append(out, h)
		}
	}
	return out
}

// callTriggerSides separates the group's rows of the last day from the week
// before, keeping the known triggers and counting the rows without one.
func callTriggerSides(group []map[string]any, split time.Time) (recent, previous []map[string]any, currentUnknown, previousUnknown int, values map[string]bool, last map[string]any) {
	recent, previous = []map[string]any{}, []map[string]any{}
	values = map[string]bool{}
	for _, r := range group {
		trigger, known := r["callTrigger"].(string)
		if at(r).After(split) {
			if last == nil || !at(r).Before(at(last)) {
				last = r
			}
			if known {
				recent = append(recent, r)
				values[trigger] = true
			} else {
				currentUnknown++
			}
		} else if known {
			previous = append(previous, r)
			values[trigger] = true
		} else {
			previousUnknown++
		}
	}
	return recent, previous, currentUnknown, previousUnknown, values, last
}

type unitTotal struct {
	Key, Type      string
	Count, Unknown int
	Amount         float64
	Last           map[string]any
}

func unitID(r map[string]any) (string, string) {
	if draft := fleet.Text(r["draft"]); draft != "" {
		return "draft:" + draft, "draft"
	}
	if key := issueKey(r); key != "" {
		return key, "issue"
	}
	return "", ""
}
func UnitTotals(rows, history []map[string]any, weights map[string]any) []map[string]any {
	last := latest(rows)
	if last == nil {
		return nil
	}
	end := at(last)
	start := end.Add(-7 * 24 * time.Hour)
	targets := map[string]bool{}
	for _, r := range rows {
		key, _ := unitID(r)
		if key != "" {
			targets[key] = true
		}
	}
	current, previous, order := unitTotalWindows(history, start, end, weights)
	out := []map[string]any{}
	for _, key := range order {
		u := current[key]
		if !targets[key] {
			continue
		}
		counts, amounts := []float64{}, []float64{}
		for _, peer := range previous {
			if peer.Type == u.Type && peer.Key != key {
				counts = append(counts, float64(peer.Count))
				amounts = append(amounts, peer.Amount)
			}
		}
		if len(counts) < 10 {
			continue
		}
		cm, _ := median(counts)
		am, _ := median(amounts)
		countHit := cm > 0 && u.Count >= 30 && float64(u.Count) >= cm*5
		amountHit := am > 0 && u.Amount >= am*5
		if !countHit && !amountHit {
			continue
		}
		measured, base, threshold, unit := u.Amount, am, am*5, "量"
		if countHit {
			measured, base, threshold, unit = float64(u.Count), cm, math.Max(30, cm*5), "回"
		}
		h := hit("unit-total", u.Last, measured, base, threshold, "同種の他単位10件以上の直前7日合計の中央値の5倍以上（回数は30行以上）", "直近7日", unit)
		h["step"], h["at"], h["ratio"], h["unknown"], h["causeKey"] = nil, iso(end), measured/base, u.Unknown, "unit-total:"+key
		if u.Type == "draft" {
			h["draft"] = u.Last["draft"]
		}
		out = append(out, h)
	}
	return out
}

// unitTotalWindows totals each consultation or issue in the last week and the week before.
func unitTotalWindows(history []map[string]any, start, end time.Time, weights map[string]any) (current, previous map[string]*unitTotal, order []string) {
	current, previous = map[string]*unitTotal{}, map[string]*unitTotal{}
	order = []string{}
	for _, r := range history {
		key, kind := unitID(r)
		if key == "" || !inWindow(r, start.Add(-7*24*time.Hour), end) {
			continue
		}
		groups := previous
		if at(r).After(start) {
			groups = current
		}
		u := groups[key]
		if u == nil {
			u = &unitTotal{Key: key, Type: kind, Last: r}
			groups[key] = u
			if at(r).After(start) {
				order = append(order, key)
			}
		}
		u.Count++
		v, ok := weighted(r, weights)
		if ok {
			u.Amount += v
		} else {
			u.Unknown++
		}
		if at(r).After(at(u.Last)) {
			u.Last = r
		}
	}
	return current, previous, order
}

type providerSide struct {
	Total, Unknown int
	Counts         map[string]int
	Last           map[string]map[string]any
	LastAny        map[string]any
}

func side() *providerSide {
	return &providerSide{Counts: map[string]int{}, Last: map[string]map[string]any{}}
}
func ProviderShifts(rows, history []map[string]any) []map[string]any {
	last := latest(rows)
	if last == nil {
		return nil
	}
	end := at(last)
	before, recent, lastOf, steps := countProviders(history, end)
	out := providerDisappearances(before, recent, lastOf, end)
	return append(out, stepProviderShifts(steps, end)...)
}

// countProviders counts each provider's rows in the last 12 hours and the week
// before, and each step's rows by provider in the last day and the week before.
func countProviders(history []map[string]any, end time.Time) (before, recent map[string]int, lastOf map[string]map[string]any, steps map[string][2]*providerSide) {
	list := append([]map[string]any{}, history...)
	sort.SliceStable(list, func(i, j int) bool { return at(list[i]).Before(at(list[j])) })
	before, recent = map[string]int{}, map[string]int{}
	lastOf = map[string]map[string]any{}
	steps = map[string][2]*providerSide{}
	for _, r := range list {
		t := at(r)
		if t.IsZero() || t.After(end) {
			continue
		}
		p := fleet.Text(r["provider"])
		if p != "" {
			lastOf[p] = r
			if t.After(end.Add(-12 * time.Hour)) {
				recent[p]++
			} else if t.After(end.Add(-7*24*time.Hour - 12*time.Hour)) {
				before[p]++
			}
		}
		step, ok := r["step"].(string)
		if !ok || !t.After(end.Add(-8*24*time.Hour)) {
			continue
		}
		step = usage.CanonicalStep(step)
		group := steps[step]
		if group[0] == nil {
			group = [2]*providerSide{side(), side()}
			steps[step] = group
		}
		s := group[1]
		if t.After(end.Add(-24 * time.Hour)) {
			s = group[0]
		}
		if p == "" {
			s.Unknown++
		} else {
			s.Total++
			s.Counts[p]++
			s.Last[p] = r
			s.LastAny = r
		}
	}
	return before, recent, lastOf, steps
}

// providerDisappearances detects a provider used through the past week that
// stopped in the last 12 hours while others kept running.
func providerDisappearances(before, recent map[string]int, lastOf map[string]map[string]any, end time.Time) []map[string]any {
	out := []map[string]any{}
	recentTotal := 0
	for _, n := range recent {
		recentTotal += n
	}
	providers := []string{}
	for p := range before {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		if before[p] < 20 || recent[p] > 0 || recentTotal < 10 {
			continue
		}
		h := newAggregateHit("provider-shift", lastOf[p], end, end.Sub(at(lastOf[p])).Hours(), nil, 12, "直前7日に20行以上の実行先が直近12時間に0行で、他実行先が10行以上", "直近12時間", "時間", "provider-shift:*:"+p)
		h["step"] = nil
		out = append(out, h)
	}
	return out
}

// stepProviderShifts detects a step whose provider share moved by half or more.
func stepProviderShifts(steps map[string][2]*providerSide, end time.Time) []map[string]any {
	out := []map[string]any{}
	stepKeys := []string{}
	for step := range steps {
		stepKeys = append(stepKeys, step)
	}
	sort.Strings(stepKeys)
	for _, step := range stepKeys {
		c, b := steps[step][0], steps[step][1]
		if c.Total < 20 || b.Total < 20 {
			continue
		}
		seen := map[string]bool{}
		for p := range c.Counts {
			seen[p] = true
		}
		for p := range b.Counts {
			seen[p] = true
		}
		keys := []string{}
		for p := range seen {
			keys = append(keys, p)
		}
		sort.Strings(keys)
		for _, p := range keys {
			measured, base := float64(c.Counts[p])/float64(c.Total), float64(b.Counts[p])/float64(b.Total)
			if math.Abs(measured-base) < .5-1e-9 {
				continue
			}
			threshold := base - .5
			if measured > base {
				threshold = base + .5
			}
			last := c.Last[p]
			if last == nil {
				last = c.LastAny
			}
			h := newAggregateHit("provider-shift", last, end, measured, base, threshold, "現在と直前7日が各20行以上で、実行先の比率の差が0.5以上", "直近24時間", "比率", "provider-shift:"+step+":"+p)
			h["step"], h["provider"], h["unknown"] = step, p, map[string]any{"current": c.Unknown, "previous": b.Unknown}
			out = append(out, h)
		}
	}
	return out
}
