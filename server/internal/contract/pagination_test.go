package contract

import (
	"net/url"
	"testing"
)

func TestPagination(t *testing.T) {
	for _, raw := range []string{"limit=0", "limit=201", "limit=", "limit=1&limit=2", "page=-1", "page=1.5", "page=999999999999999999999999"} {
		q, _ := url.ParseQuery(raw)
		if _, err := ParsePagination(q); err == nil || err.Status != 400 {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	p, err := ParsePagination(url.Values{})
	if err != nil || p.Page != 1 || p.Limit != 100 {
		t.Fatal(p, err)
	}
	p = Pagination{Page: 2, Limit: 2}
	got := Paginate([]int{1, 2, 3, 4, 5}, p)
	if got.Total != 5 || !got.HasMore || len(got.Items) != 2 || got.Items[0] != 3 {
		t.Fatal(got)
	}
	p.Page = int(^uint(0) >> 1)
	got = Paginate([]int{1}, p)
	if len(got.Items) != 0 || got.HasMore {
		t.Fatal(got)
	}
}
