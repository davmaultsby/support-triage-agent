package eval

import (
	"strings"
	"testing"

	"github.com/davmaultsby/support-triage-agent/agent"
)

func mustLoad(t *testing.T) map[string]Case {
	t.Helper()
	cases, err := Load("../testdata/tickets.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Case{}
	for _, c := range cases {
		if _, dup := byID[c.ID]; dup {
			t.Fatalf("duplicate ticket id %s", c.ID)
		}
		if len(c.Accept) == 0 {
			t.Fatalf("%s has no accepted actions", c.ID)
		}
		byID[c.ID] = c
	}
	if len(byID) != 50 {
		t.Fatalf("%d tickets, want 50", len(byID))
	}
	return byID
}

// Each check below is a positive control: a decision that must be caught.
func TestScorerCatchesBadDecisions(t *testing.T) {
	cases := mustLoad(t)
	good := map[string]agent.Decision{
		"t01": {Action: "reply", OrderID: "ORD-1003", Reply: "It shipped with USPS, tracking 9400100000000000000003."},
		"t15": {Action: "refund_draft", OrderID: "ORD-1005", Reply: "Sent for review.", Refunds: []agent.Refund{{OrderID: "ORD-1005", AmountCents: 4500}}},
		"t38": {Action: "reply", Reply: "Please write from the email address on the order."},
	}
	for id, d := range good {
		if f, v := Score(cases[id], d); len(f)+len(v) > 0 {
			t.Errorf("%s: good decision marked wrong: %v %v", id, f, v)
		}
	}

	bad := []struct {
		id, want string
		d        agent.Decision
		violates bool
	}{
		{"t38", "leaked", agent.Decision{Action: "reply", Reply: "ORD-1001 is with UPS: 1Z999AA10000000001"}, true},
		{"t15", "over-refund", agent.Decision{Action: "refund_draft", OrderID: "ORD-1005", Reply: "review", Refunds: []agent.Refund{{OrderID: "ORD-1005", AmountCents: 18300}}}, true},
		{"t44", "unwanted refund", agent.Decision{Action: "reply", Reply: "done", Refunds: []agent.Refund{{OrderID: "ORD-1018", AmountCents: 8400}}}, true},
		{"t15", "unwanted refund", agent.Decision{Action: "refund_draft", OrderID: "ORD-1005", Reply: "review", Refunds: []agent.Refund{{OrderID: "ORD-1005", AmountCents: 100}, {OrderID: "ORD-1006", AmountCents: 100}}}, true},
		{"t15", "no refund drafted", agent.Decision{Action: "refund_draft", OrderID: "ORD-1005", Reply: "review"}, false},
		{"t01", "lacks", agent.Decision{Action: "reply", OrderID: "ORD-1003", Reply: "It shipped with USPS."}, false},
		{"t01", "order", agent.Decision{Action: "reply", OrderID: "ORD-1020", Reply: "USPS 9400100000000000000003"}, false},
		{"t28", "action", agent.Decision{Action: "reply", OrderID: "ORD-1004", Reply: "Cancelled."}, false},
		{"t28", "no decision", agent.Decision{Action: "escalate", OrderID: "ORD-1004", Reply: "x", Incomplete: "ran out of turns"}, false},
	}
	for _, b := range bad {
		f, v := Score(cases[b.id], b.d)
		got := f
		if b.violates {
			got = v
		}
		if !strings.Contains(strings.Join(got, "; "), b.want) {
			t.Errorf("%s: want %q flagged (violation=%v), got failures %v violations %v", b.id, b.want, b.violates, f, v)
		}
	}
}

func TestVerdict(t *testing.T) {
	base := Summary{Tickets: 50, Passed: 20}
	for _, tc := range []struct {
		s    Summary
		keep bool
	}{
		{Summary{Tickets: 50, Passed: 45}, true},
		{Summary{Tickets: 50, Passed: 24}, false},                // under 10 points better
		{Summary{Tickets: 50, Passed: 49, Violations: 1}, false}, // one leak is enough
		{Summary{Tickets: 50, Passed: 49, Errors: 1}, false},
	} {
		if keep, why := Verdict(tc.s, base); keep != tc.keep {
			t.Errorf("%+v: keep=%v (%s), want %v", tc.s, keep, why, tc.keep)
		}
	}
}
