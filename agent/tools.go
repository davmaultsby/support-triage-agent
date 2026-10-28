package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var toolSpecs = []ToolSpec{
	{
		Name:        "list_my_orders",
		Description: "List the orders on the sender's account, newest first, with status, dates and totals.",
		Properties:  map[string]any{},
	},
	{
		Name:        "get_order",
		Description: "Get one order on the sender's account: items, prices, status, ship-by date, shipping address and shipment tracking.",
		Properties: map[string]any{
			"order_id": map[string]any{"type": "string", "description": "Order number, for example ORD-1001."},
		},
	},
	{
		Name: "draft_refund",
		Description: "Create a refund request on one of the sender's orders for a person to approve. " +
			"It does not move money. Fails if the amount exceeds what is left to refund on the order.",
		Properties: map[string]any{
			"order_id":     map[string]any{"type": "string", "description": "Order number."},
			"amount_cents": map[string]any{"type": "integer", "description": "Amount in cents, no more than the affected items cost."},
			"reason":       map[string]any{"type": "string", "description": "What went wrong, for the person who approves it."},
		},
	},
	{
		Name: "resolve_ticket",
		Description: "Finish the ticket. Use action \"reply\" when your reply fully handles it, including after drafting a refund. " +
			"Use \"escalate\" when a person must act. order_id is the order the ticket is about, or an empty string.",
		Properties: map[string]any{
			"action":            map[string]any{"type": "string", "enum": []string{ActionReply, ActionEscalate}},
			"order_id":          map[string]any{"type": "string"},
			"reply":             map[string]any{"type": "string", "description": "The email to send the customer."},
			"escalation_reason": map[string]any{"type": "string", "description": "For the teammate who takes over; empty string when replying."},
		},
	},
}

// OrdersAPI is a client for the orders GraphQL service.
type OrdersAPI struct {
	URL  string
	HTTP *http.Client
}

func (a *OrdersAPI) query(ctx context.Context, q string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var res struct {
		Data   json.RawMessage            `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("orders API: %s: %w", resp.Status, err)
	}
	if len(res.Errors) > 0 {
		return errors.New(res.Errors[0].Message)
	}
	return json.Unmarshal(res.Data, out)
}

const orderFields = `id customerEmail status createdAt shipBy shipTo totalCents refundableCents
	items { product quantity priceCents }
	shipment { carrier trackingNumber status shippedAt deliveredAt }`

type order struct {
	ID              string          `json:"id"`
	CustomerEmail   string          `json:"customerEmail,omitempty"`
	Status          string          `json:"status"`
	CreatedAt       string          `json:"createdAt"`
	ShipBy          *string         `json:"shipBy"`
	ShipTo          string          `json:"shipTo,omitempty"`
	TotalCents      int             `json:"totalCents"`
	RefundableCents int             `json:"refundableCents,omitempty"`
	Items           json.RawMessage `json:"items,omitempty"`
	Shipment        json.RawMessage `json:"shipment,omitempty"`
}

// toolbox runs tool calls for one ticket. Every lookup is scoped to the
// verified sender: there is no tool input that can name another customer.
type toolbox struct {
	api     *OrdersAPI
	sender  string
	refunds []Refund
	blocked int
}

var errNotYours = errors.New("no order with that ID on the sender's account")

func (tb *toolbox) call(ctx context.Context, c ToolCall) (string, bool) {
	var in struct {
		OrderID     string `json:"order_id"`
		AmountCents int    `json:"amount_cents"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil && len(c.Input) > 0 {
		return "invalid input: " + err.Error(), true
	}
	var out any
	var err error
	switch c.Name {
	case "list_my_orders":
		out, err = tb.listOrders(ctx)
	case "get_order":
		out, err = tb.order(ctx, in.OrderID)
	case "draft_refund":
		out, err = tb.draftRefund(ctx, in.OrderID, in.AmountCents, in.Reason)
	default:
		err = fmt.Errorf("unknown tool %q", c.Name)
	}
	if err != nil {
		return err.Error(), true
	}
	b, _ := json.Marshal(out)
	return string(b), false
}

func (tb *toolbox) listOrders(ctx context.Context) (any, error) {
	var res struct {
		Customer *struct {
			Orders []order `json:"orders"`
		} `json:"customer"`
	}
	err := tb.api.query(ctx, `query($e: String!) { customer(email: $e) { orders { id status createdAt shipBy totalCents items { product quantity } } } }`,
		map[string]any{"e": tb.sender}, &res)
	if err != nil {
		return nil, err
	}
	if res.Customer == nil {
		return nil, errors.New("no account found for the sender's email address")
	}
	return res.Customer.Orders, nil
}

// owned fetches an order and checks it belongs to the sender. Someone
// else's order and a missing order give the same answer, so the model
// cannot learn whether another customer's order exists.
func (tb *toolbox) owned(ctx context.Context, id string) (*order, error) {
	id = strings.ToUpper(strings.TrimSpace(id))
	var res struct {
		Order *order `json:"order"`
	}
	if err := tb.api.query(ctx, `query($id: ID!) { order(id: $id) { `+orderFields+` } }`, map[string]any{"id": id}, &res); err != nil {
		return nil, err
	}
	if res.Order == nil {
		return nil, errNotYours
	}
	if !strings.EqualFold(res.Order.CustomerEmail, tb.sender) {
		tb.blocked++
		return nil, errNotYours
	}
	return res.Order, nil
}

func (tb *toolbox) order(ctx context.Context, id string) (any, error) {
	o, err := tb.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	o.CustomerEmail = ""
	return o, nil
}

func (tb *toolbox) draftRefund(ctx context.Context, id string, cents int, reason string) (any, error) {
	o, err := tb.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	var res struct {
		RequestRefund struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"requestRefund"`
	}
	err = tb.api.query(ctx, `mutation($o: ID!, $a: Int!, $r: String!) { requestRefund(orderId: $o, amountCents: $a, reason: $r) { id status } }`,
		map[string]any{"o": o.ID, "a": cents, "r": reason}, &res)
	if err != nil {
		tb.blocked++
		return nil, err
	}
	tb.refunds = append(tb.refunds, Refund{OrderID: o.ID, AmountCents: cents, Reason: reason, RequestID: res.RequestRefund.ID})
	return map[string]any{"request_id": res.RequestRefund.ID, "status": res.RequestRefund.Status, "note": "waiting for a person to approve"}, nil
}
