package orders

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB connects to the dev database. Tests skip without one, unless
// TRIAGE_REQUIRE_DB=1 makes a missing database a failure (used in CI and the gates).
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db, err := Open(context.Background())
	if err != nil {
		if os.Getenv("TRIAGE_REQUIRE_DB") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	t.Cleanup(db.Close)
	return db
}

func post(t *testing.T, srv *httptest.Server, query string, vars map[string]any) (data map[string]any, errs []any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data   map[string]any `json:"data"`
		Errors []any          `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Data, out.Errors
}

func TestGraphQLOverPostgres(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db))
	defer srv.Close()

	data, errs := post(t, srv, `query($e: String!) { customer(email: $e) { name orders { id status shipment { trackingNumber } } } }`,
		map[string]any{"e": "LIAM.WALSH@example.com"})
	if errs != nil {
		t.Fatal(errs)
	}
	got, _ := json.Marshal(data)
	for _, want := range []string{`"name":"Liam Walsh"`, `"id":"ORD-1020"`, `"trackingNumber":"1Z999AA10000000020"`, `"id":"ORD-1019"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("customer query missing %s in %s", want, got)
		}
	}
	// Newest order first.
	if strings.Index(string(got), "ORD-1020") > strings.Index(string(got), "ORD-1019") {
		t.Errorf("orders not newest first: %s", got)
	}

	data, _ = post(t, srv, `{ order(id: "ORD-9999") { id } }`, nil)
	if data["order"] != nil {
		t.Errorf("unknown order returned %v", data["order"])
	}
}

func TestRequestRefundStaysWithinBalance(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db))
	defer srv.Close()
	ctx := context.Background()
	const reason = "test: refund balance guard"
	// Refund requests are scratch data (an eval run starts by clearing them), so start this order from zero.
	if _, err := db.Exec(ctx, `DELETE FROM refund_requests WHERE order_id IN ('ORD-1017', 'ORD-1010')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(ctx, `DELETE FROM refund_requests WHERE reason = $1`, reason) })

	const mutation = `mutation($o: ID!, $a: Int!, $r: String!) { requestRefund(orderId: $o, amountCents: $a, reason: $r) { status amountCents } }`
	balance := func() float64 {
		data, errs := post(t, srv, `{ order(id: "ORD-1017") { refundableCents } }`, nil)
		if errs != nil {
			t.Fatal(errs)
		}
		return data["order"].(map[string]any)["refundableCents"].(float64)
	}

	before := balance()
	data, errs := post(t, srv, mutation, map[string]any{"o": "ORD-1017", "a": 1000, "r": reason})
	if errs != nil {
		t.Fatal(errs)
	}
	if got := data["requestRefund"].(map[string]any)["status"]; got != "pending_approval" {
		t.Errorf("status = %v, want pending_approval", got)
	}
	if after := balance(); after != before-1000 {
		t.Errorf("refundable %v -> %v, want a drop of 1000", before, after)
	}

	// More than what is left, a non-positive amount, a cancelled order: all refused, nothing written.
	for _, vars := range []map[string]any{
		{"o": "ORD-1017", "a": int(before), "r": reason},
		{"o": "ORD-1017", "a": 0, "r": reason},
		{"o": "ORD-1010", "a": 100, "r": reason},
		{"o": "ORD-9999", "a": 100, "r": reason},
	} {
		if _, errs := post(t, srv, mutation, vars); errs == nil {
			t.Errorf("refund %v accepted, want an error", vars)
		}
	}
	var n int
	db.QueryRow(ctx, `SELECT count(*) FROM refund_requests WHERE reason = $1`, reason).Scan(&n)
	if n != 1 {
		t.Errorf("%d refund rows written, want 1", n)
	}
}
