-- Order data for a fictional print shop. Rerunning this file resets everything.

DROP TABLE IF EXISTS refund_requests, shipments, order_items, orders, customers CASCADE;
DROP TYPE IF EXISTS order_status, shipment_status;

CREATE TYPE order_status AS ENUM ('proof_pending', 'in_production', 'shipped', 'delivered', 'cancelled');
CREATE TYPE shipment_status AS ENUM ('in_transit', 'delivered', 'exception');

CREATE TABLE customers (
    id    serial PRIMARY KEY,
    email text NOT NULL UNIQUE,
    name  text NOT NULL
);

CREATE TABLE orders (
    id          text PRIMARY KEY,
    customer_id int NOT NULL REFERENCES customers,
    status      order_status NOT NULL,
    created_at  timestamptz NOT NULL,
    ship_by     date,
    ship_to     text NOT NULL
);
CREATE INDEX ON orders (customer_id);

CREATE TABLE order_items (
    id          serial PRIMARY KEY,
    order_id    text NOT NULL REFERENCES orders,
    product     text NOT NULL,
    quantity    int NOT NULL CHECK (quantity > 0),
    price_cents int NOT NULL CHECK (price_cents >= 0)
);
CREATE INDEX ON order_items (order_id);

CREATE TABLE shipments (
    order_id        text PRIMARY KEY REFERENCES orders,
    carrier         text NOT NULL,
    tracking_number text NOT NULL,
    status          shipment_status NOT NULL,
    shipped_at      timestamptz NOT NULL,
    delivered_at    timestamptz
);

-- Refunds are only ever requested here. A person approves or rejects them elsewhere.
CREATE TABLE refund_requests (
    id           serial PRIMARY KEY,
    order_id     text NOT NULL REFERENCES orders,
    amount_cents int NOT NULL CHECK (amount_cents > 0),
    reason       text NOT NULL,
    status       text NOT NULL DEFAULT 'pending_approval'
                 CHECK (status IN ('pending_approval', 'approved', 'rejected')),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON refund_requests (order_id);
