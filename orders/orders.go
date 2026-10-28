// Package orders serves the shop's order data as a GraphQL API over Postgres.
//
// It is the internal service the support agent's tools call. It trusts its
// caller: deciding which customer may see which order is the agent's job.
package orders

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.graphql
var schemaSDL string

// Open connects to DATABASE_URL, or to the database scripts/dev-db.sh and docker-compose.yml start.
func Open(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://triage:triage@localhost:54329/triage"
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s (run scripts/dev-db.sh start or docker compose up -d): %w", url, err)
	}
	return db, nil
}

// Handler serves GraphQL requests (POST {"query", "variables"}).
func Handler(db *pgxpool.Pool) http.Handler {
	return &relay.Handler{Schema: graphql.MustParseSchema(schemaSDL, &resolver{db: db})}
}

type resolver struct{ db *pgxpool.Pool }

func (r *resolver) Customer(ctx context.Context, args struct{ Email string }) (*customer, error) {
	c := &customer{db: r.db}
	err := r.db.QueryRow(ctx, `SELECT id, email, name FROM customers WHERE lower(email) = lower($1)`, args.Email).
		Scan(&c.id, &c.email, &c.name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (r *resolver) Order(ctx context.Context, args struct{ ID graphql.ID }) (*order, error) {
	o, err := loadOrder(ctx, r.db, string(args.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

func (r *resolver) RequestRefund(ctx context.Context, args struct {
	OrderID     graphql.ID
	AmountCents int32
	Reason      string
}) (*refundRequest, error) {
	if args.AmountCents <= 0 {
		return nil, errors.New("amountCents must be positive")
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Lock the order so two concurrent requests cannot both spend the same balance.
	var status string
	err = tx.QueryRow(ctx, `SELECT status::text FROM orders WHERE id = $1 FOR UPDATE`, string(args.OrderID)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("order %s not found", args.OrderID)
	}
	if err != nil {
		return nil, err
	}
	var total, requested int64
	err = tx.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(price_cents) FROM order_items WHERE order_id = $1), 0),
		COALESCE((SELECT sum(amount_cents) FROM refund_requests WHERE order_id = $1 AND status <> 'rejected'), 0)`,
		string(args.OrderID)).Scan(&total, &requested)
	if err != nil {
		return nil, err
	}
	if left := refundable(status, total, requested); int64(args.AmountCents) > left {
		return nil, fmt.Errorf("amount %d exceeds the refundable balance of %d cents", args.AmountCents, left)
	}

	rr := &refundRequest{orderID: string(args.OrderID), amountCents: args.AmountCents}
	err = tx.QueryRow(ctx, `INSERT INTO refund_requests (order_id, amount_cents, reason) VALUES ($1, $2, $3) RETURNING id, status`,
		rr.orderID, rr.amountCents, args.Reason).Scan(&rr.id, &rr.status)
	if err != nil {
		return nil, err
	}
	return rr, tx.Commit(ctx)
}

func refundable(status string, total, requested int64) int64 {
	if status == "cancelled" {
		return 0
	}
	return max(total-requested, 0)
}

type customer struct {
	db          *pgxpool.Pool
	id          int
	email, name string
}

func (c *customer) Email() string { return c.email }
func (c *customer) Name() string  { return c.name }

// ponytail: one query per order (N+1). Customers have a handful of orders; batch it if that changes.
func (c *customer) Orders(ctx context.Context) ([]*order, error) {
	rows, err := c.db.Query(ctx, `SELECT id FROM orders WHERE customer_id = $1 ORDER BY created_at DESC`, c.id)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]*order, len(ids))
	for i, id := range ids {
		if out[i], err = loadOrder(ctx, c.db, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type order struct {
	id, email, status, shipTo string
	createdAt                 time.Time
	shipBy                    *time.Time
	total, requested          int64
	items                     []*item
	shipment                  *shipment
}

func loadOrder(ctx context.Context, db *pgxpool.Pool, id string) (*order, error) {
	o := &order{}
	var carrier, tracking, shipStatus *string
	var shippedAt, deliveredAt *time.Time
	err := db.QueryRow(ctx, `
		SELECT o.id, c.email, o.status::text, o.created_at, o.ship_by, o.ship_to,
		       COALESCE((SELECT sum(price_cents) FROM order_items WHERE order_id = o.id), 0),
		       COALESCE((SELECT sum(amount_cents) FROM refund_requests WHERE order_id = o.id AND status <> 'rejected'), 0),
		       s.carrier, s.tracking_number, s.status::text, s.shipped_at, s.delivered_at
		FROM orders o
		JOIN customers c ON c.id = o.customer_id
		LEFT JOIN shipments s ON s.order_id = o.id
		WHERE o.id = $1`, id).
		Scan(&o.id, &o.email, &o.status, &o.createdAt, &o.shipBy, &o.shipTo, &o.total, &o.requested,
			&carrier, &tracking, &shipStatus, &shippedAt, &deliveredAt)
	if err != nil {
		return nil, err
	}
	if carrier != nil {
		o.shipment = &shipment{carrier: *carrier, tracking: *tracking, status: *shipStatus, shippedAt: *shippedAt, deliveredAt: deliveredAt}
	}
	rows, err := db.Query(ctx, `SELECT product, quantity, price_cents FROM order_items WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	o.items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (*item, error) {
		it := &item{}
		return it, row.Scan(&it.product, &it.quantity, &it.priceCents)
	})
	return o, err
}

func (o *order) ID() graphql.ID         { return graphql.ID(o.id) }
func (o *order) CustomerEmail() string  { return o.email }
func (o *order) Status() string         { return o.status }
func (o *order) CreatedAt() string      { return o.createdAt.UTC().Format(time.RFC3339) }
func (o *order) ShipBy() *string        { return date(o.shipBy) }
func (o *order) ShipTo() string         { return o.shipTo }
func (o *order) TotalCents() int32      { return int32(o.total) }
func (o *order) RefundableCents() int32 { return int32(refundable(o.status, o.total, o.requested)) }
func (o *order) Items() []*item         { return o.items }
func (o *order) Shipment() *shipment    { return o.shipment }

type item struct {
	product              string
	quantity, priceCents int32
}

func (i *item) Product() string   { return i.product }
func (i *item) Quantity() int32   { return i.quantity }
func (i *item) PriceCents() int32 { return i.priceCents }

type shipment struct {
	carrier, tracking, status string
	shippedAt                 time.Time
	deliveredAt               *time.Time
}

func (s *shipment) Carrier() string        { return s.carrier }
func (s *shipment) TrackingNumber() string { return s.tracking }
func (s *shipment) Status() string         { return s.status }
func (s *shipment) ShippedAt() string      { return *date(&s.shippedAt) }
func (s *shipment) DeliveredAt() *string   { return date(s.deliveredAt) }

type refundRequest struct {
	id              int32
	orderID, status string
	amountCents     int32
}

func (r *refundRequest) ID() graphql.ID      { return graphql.ID(fmt.Sprint(r.id)) }
func (r *refundRequest) OrderID() graphql.ID { return graphql.ID(r.orderID) }
func (r *refundRequest) AmountCents() int32  { return r.amountCents }
func (r *refundRequest) Status() string      { return r.status }

func date(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.DateOnly)
	return &s
}
